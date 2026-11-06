package runnerweb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxUploadSize is the maximum program size in bytes.
	MaxUploadSize = 10 << 20
	// MaxDockerfileSize is the maximum Dockerfile size in bytes.
	MaxDockerfileSize = 256 << 10
	// DefaultJobImage provides the default Python and shell runtime.
	DefaultJobImage = "python:3.14-slim-trixie"

	maxJobs          = 32
	maxActiveJobs    = 4
	maxLogLines      = 200
	maxLogBytes      = 2048
	dockerfileSuffix = ".Dockerfile"

	statusScheduled = "scheduled"
	statusRunning   = "running"
	statusStopping  = "stopping"
	statusStopped   = "stopped"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

var (
	ErrJobNotFound        = errors.New("job not found")
	ErrJobActive          = errors.New("job is active")
	ErrJobLimit           = errors.New("job limit reached")
	ErrUploadTooLarge     = errors.New("upload exceeds 10 MiB")
	ErrDockerfileTooLarge = errors.New("Dockerfile exceeds 256 KiB")
	ErrInvalidJob         = errors.New("invalid job")
	imageReference        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/.:_@-]*$`)
)

type Job struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Interpreter     string    `json:"interpreter"`
	Image           string    `json:"image"`
	BuildImage      bool      `json:"build_image"`
	ProgramFilename string    `json:"program_filename"`
	StartAt         time.Time `json:"start_at"`
	NextAt          time.Time `json:"next_at,omitzero"`
	IntervalSeconds int       `json:"interval_seconds"`
	TimeoutSeconds  int       `json:"timeout_seconds"`
	Parallelism     int       `json:"parallelism"`
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
}

type Scheduler struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	workers sync.WaitGroup
	dataDir string
	uploads string
	run     func(context.Context, Job, string, io.Writer) error

	mu      sync.Mutex
	jobs    map[string]*scheduledJob
	active  int
	pending int
}

type scheduledJob struct {
	Job
	cancel context.CancelFunc
	logs   jobLog
}

func OpenScheduler(ctx context.Context, dataDir string, run func(context.Context, Job, string, io.Writer) error) (*Scheduler, error) {
	if dataDir == "" || run == nil {
		return nil, errors.New("scheduler requires a data directory and callback")
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	uploads := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Scheduler{
		ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		dataDir: dataDir, uploads: uploads, run: run, jobs: make(map[string]*scheduledJob),
	}
	if err := s.restore(); err != nil {
		cancel()
		return nil, fmt.Errorf("restore runner jobs: %w", err)
	}
	go s.schedule()
	return s, nil
}

func (s *Scheduler) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

func (s *Scheduler) Add(job Job, upload io.Reader) (Job, error) {
	return s.AddWithDockerfile(job, upload, nil)
}

func (s *Scheduler) AddWithDockerfile(job Job, upload, dockerfile io.Reader) (Job, error) {
	job.Name = strings.TrimSpace(job.Name)
	job.Image = strings.TrimSpace(job.Image)
	if job.BuildImage && dockerfile == nil {
		return Job{}, fmt.Errorf("%w: Dockerfile is required to build an image", ErrInvalidJob)
	}
	job.BuildImage = dockerfile != nil
	if !job.BuildImage && job.Image == "" {
		job.Image = DefaultJobImage
	}
	if err := validateJob(job); err != nil {
		return Job{}, err
	}
	if upload == nil {
		return Job{}, fmt.Errorf("%w: upload is required", ErrInvalidJob)
	}
	if err := s.reserveUpload(); err != nil {
		return Job{}, err
	}
	defer func() {
		s.mu.Lock()
		s.pending--
		s.mu.Unlock()
	}()

	programFile, err := s.stageUpload(upload, "program", MaxUploadSize, 0o700, ErrUploadTooLarge)
	if err != nil {
		return Job{}, err
	}
	defer os.Remove(programFile)
	var dockerfileTemp string
	if dockerfile != nil {
		dockerfileTemp, err = s.stageUpload(dockerfile, "Dockerfile", MaxDockerfileSize, 0o600, ErrDockerfileTooLarge)
		if err != nil {
			return Job{}, err
		}
		defer os.Remove(dockerfileTemp)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return Job{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Job{}, err
	}
	job.ID = hex.EncodeToString(id[:])
	if s.jobs[job.ID] != nil {
		return Job{}, errors.New("generated duplicate job ID")
	}
	now := time.Now()
	if job.StartAt.IsZero() {
		job.StartAt = now
	}
	job.StartAt = job.StartAt.UTC()
	job.NextAt = job.StartAt
	job.Status = statusScheduled
	job.LastError = ""
	program := s.program(job.ID)
	if err := os.Rename(programFile, program); err != nil {
		return Job{}, err
	}
	if dockerfileTemp != "" {
		if err := os.Rename(dockerfileTemp, DockerfileForProgram(program)); err != nil {
			os.Remove(program)
			return Job{}, err
		}
	}
	s.jobs[job.ID] = &scheduledJob{Job: job}
	if err := s.persist(); err != nil {
		delete(s.jobs, job.ID)
		s.removeUploads(job.ID)
		return Job{}, err
	}
	s.signal()
	return job, nil
}

func (s *Scheduler) stageUpload(input io.Reader, label string, limit int64, mode os.FileMode, tooLarge error) (_ string, err error) {
	file, err := os.CreateTemp(s.uploads, ".upload-")
	if err != nil {
		return "", err
	}
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(file.Name())
		}
	}()
	size, err := io.Copy(file, io.LimitReader(input, limit+1))
	if err != nil {
		return "", err
	}
	if size > limit {
		return "", tooLarge
	}
	if size == 0 {
		return "", fmt.Errorf("%w: empty %s upload", ErrInvalidJob, label)
	}
	if err := file.Chmod(mode); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return file.Name(), nil
}

func (s *Scheduler) reserveUpload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if len(s.jobs)+s.pending >= maxJobs {
		return ErrJobLimit
	}
	s.pending++
	return nil
}

func validateJob(job Job) error {
	if strings.TrimSpace(job.Name) == "" || len(job.Name) > 160 {
		return fmt.Errorf("%w: name must be 1 to 160 bytes", ErrInvalidJob)
	}
	switch job.Interpreter {
	case "python3", "sh", "binary":
	default:
		return fmt.Errorf("%w: interpreter must be python3, sh or binary", ErrInvalidJob)
	}
	if job.BuildImage {
		if job.Image != "" {
			return fmt.Errorf("%w: choose a Docker image or a Dockerfile", ErrInvalidJob)
		}
		if job.ProgramFilename == "" || strings.EqualFold(job.ProgramFilename, "Dockerfile") ||
			strings.EqualFold(job.ProgramFilename, ".dockerignore") || strings.EqualFold(job.ProgramFilename, "Dockerfile.dockerignore") {
			return fmt.Errorf("%w: program filename is reserved or missing", ErrInvalidJob)
		}
	} else if len(job.Image) > 255 || !imageReference.MatchString(job.Image) {
		return fmt.Errorf("%w: invalid Docker image reference", ErrInvalidJob)
	}
	if filename := job.ProgramFilename; filename != "" && (len(filename) > 160 || filename != strings.TrimSpace(filename) ||
		filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") ||
		!utf8.ValidString(filename) || strings.IndexFunc(filename, unicode.IsControl) >= 0) {
		return fmt.Errorf("%w: program filename must be a safe basename of up to 160 bytes", ErrInvalidJob)
	}
	if job.IntervalSeconds < 0 || job.IntervalSeconds > 86400 ||
		job.TimeoutSeconds < 1 || job.TimeoutSeconds > 3600 ||
		job.Parallelism < 1 || job.Parallelism > 32 {
		return fmt.Errorf("%w: invalid interval, timeout or parallelism", ErrInvalidJob)
	}
	return nil
}

func (s *Scheduler) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	previous := job.Job
	switch job.Status {
	case statusScheduled:
		job.Status = statusStopped
		job.NextAt = time.Time{}
	case statusRunning:
		job.Status = statusStopping
	default:
		return nil
	}
	if err := s.persist(); err != nil {
		job.Job = previous
		return err
	}
	if job.cancel != nil {
		job.cancel()
	}
	s.signal()
	return nil
}

func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	if job.Status == statusScheduled || job.cancel != nil {
		return ErrJobActive
	}
	delete(s.jobs, id)
	if err := s.persist(); err != nil {
		s.jobs[id] = job
		return err
	}
	s.signal()
	if err := s.removeUploads(id); err != nil {
		return fmt.Errorf("job deleted, removing upload: %w", err)
	}
	return nil
}

func (s *Scheduler) Logs(id string) ([]string, error) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, ErrJobNotFound
	}
	return job.logs.snapshot(), nil
}

func (s *Scheduler) Close() {
	s.cancel()
	<-s.done
	s.workers.Wait()
}

func (s *Scheduler) schedule() {
	defer close(s.done)
	for s.ctx.Err() == nil {
		next, started := s.dispatch()
		if started {
			continue
		}
		var timer *time.Timer
		var due <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			due = timer.C
		}
		select {
		case <-s.ctx.Done():
		case <-s.wake:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Scheduler) dispatch() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.active >= maxActiveJobs {
		return time.Time{}, false
	}
	var next *scheduledJob
	for _, job := range s.jobs {
		if job.Status != statusScheduled {
			continue
		}
		if next == nil || job.NextAt.Before(next.NextAt) || (job.NextAt.Equal(next.NextAt) && job.ID < next.ID) {
			next = job
		}
	}
	if next == nil {
		return time.Time{}, false
	}
	now := time.Now()
	if next.NextAt.After(now) {
		return next.NextAt, false
	}
	previous := next.Job
	next.Status = statusRunning
	next.NextAt = time.Time{}
	if err := s.persist(); err != nil {
		next.Job = previous
		log.Printf("save runner job before starting: %v", err)
		return now.Add(time.Second), false
	}
	ctx, cancel := context.WithCancel(s.ctx)
	next.cancel = cancel
	s.active++
	s.workers.Add(1)
	go s.execute(ctx, cancel, next, next.Job)
	return time.Time{}, true
}

func (s *Scheduler) execute(ctx context.Context, cancel context.CancelFunc, state *scheduledJob, job Job) {
	defer s.workers.Done()
	err := s.run(ctx, job, s.program(job.ID), &state.logs)
	if err == nil {
		err = ctx.Err()
	}
	cancel()
	state.logs.flush()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	state.cancel = nil
	state.LastError = ""
	switch {
	case state.Status == statusStopping:
		state.Status = statusStopped
	case state.IntervalSeconds > 0:
		state.Status = statusScheduled
		state.NextAt = nextRound(state.Job, time.Now())
		if err != nil && s.ctx.Err() == nil {
			state.LastError = boundedError(err)
		}
	case err != nil:
		state.Status = statusFailed
		state.LastError = boundedError(err)
	default:
		state.Status = statusCompleted
	}
	if err := s.persist(); err != nil {
		log.Printf("save runner job result: %v", err)
	}
	s.signal()
}

func nextRound(job Job, now time.Time) time.Time {
	if job.StartAt.After(now) {
		return job.StartAt
	}
	interval := time.Duration(job.IntervalSeconds) * time.Second
	return now.Add(interval - now.Sub(job.StartAt)%interval).UTC()
}

func boundedError(err error) string {
	message := err.Error()
	if len(message) > maxLogBytes {
		return message[:maxLogBytes]
	}
	return message
}

func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) program(id string) string {
	return filepath.Join(s.uploads, id)
}

// DockerfileForProgram returns the Dockerfile sibling of a scheduler-provided program path.
func DockerfileForProgram(programPath string) string {
	return programPath + dockerfileSuffix
}

// DockerfilePath resolves an upload by ID. It returns an empty path for an invalid ID.
func (s *Scheduler) DockerfilePath(id string) string {
	if !validJobID(id) {
		return ""
	}
	return DockerfileForProgram(s.program(id))
}

func (s *Scheduler) removeUploads(id string) error {
	program := s.program(id)
	var result error
	for _, path := range []string{program, DockerfileForProgram(program)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *Scheduler) snapshot() []Job {
	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job.Job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].StartAt.Equal(jobs[j].StartAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].StartAt.Before(jobs[j].StartAt)
	})
	return jobs
}

func (s *Scheduler) persist() error {
	file, err := os.CreateTemp(s.dataDir, ".jobs-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(s.snapshot()); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(s.dataDir, "jobs.json"))
}

func (s *Scheduler) restore() error {
	data, err := os.ReadFile(filepath.Join(s.dataDir, "jobs.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var jobs []Job
	if err == nil {
		if err := json.Unmarshal(data, &jobs); err != nil {
			return err
		}
	}
	if len(jobs) > maxJobs {
		return ErrJobLimit
	}
	changed := false
	now := time.Now()
	for _, job := range jobs {
		if !validJobID(job.ID) || s.jobs[job.ID] != nil {
			return errors.New("invalid or duplicate persisted job ID")
		}
		if !job.BuildImage && job.Image == "" {
			job.Image = DefaultJobImage
			changed = true
		}
		if err := validateJob(job); err != nil {
			return err
		}
		if job.StartAt.IsZero() {
			return errors.New("missing start time for persisted job")
		}
		switch job.Status {
		case statusRunning:
			job.LastError = "runner stopped before the round finished"
			if job.IntervalSeconds > 0 {
				job.Status = statusScheduled
				job.NextAt = nextRound(job, now)
			} else {
				job.Status = statusFailed
			}
			changed = true
		case statusStopping:
			job.Status = statusStopped
			changed = true
		case statusScheduled, statusStopped, statusCompleted, statusFailed:
		default:
			return fmt.Errorf("invalid status for persisted job %s", job.ID)
		}
		if job.Status == statusScheduled {
			if job.NextAt.IsZero() {
				job.NextAt = job.StartAt
				changed = true
			}
			missing, err := s.missingUpload(job)
			if err != nil {
				return err
			}
			if missing != "" {
				job.Status = statusFailed
				job.NextAt = time.Time{}
				job.LastError = "uploaded " + missing + " is missing"
				changed = true
			}
		}
		s.jobs[job.ID] = &scheduledJob{Job: job}
	}
	if changed {
		if err := s.persist(); err != nil {
			return err
		}
	}
	return s.removeOrphans()
}

func (s *Scheduler) missingUpload(job Job) (string, error) {
	program := s.program(job.ID)
	files := []struct{ path, label string }{{program, "program"}}
	if job.BuildImage {
		files = append(files, struct{ path, label string }{DockerfileForProgram(program), "Dockerfile"})
	}
	for _, file := range files {
		info, err := os.Lstat(file.path)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
			return file.label, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

func validJobID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (s *Scheduler) removeOrphans() error {
	files, err := os.ReadDir(s.uploads)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		id, dockerfile := strings.CutSuffix(name, dockerfileSuffix)
		job := s.jobs[id]
		if strings.HasPrefix(name, ".upload-") || (validJobID(id) && (job == nil || (dockerfile && !job.BuildImage))) {
			if err := os.Remove(s.program(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

type jobLog struct {
	mu        sync.Mutex
	lines     [maxLogLines]string
	next      int
	count     int
	pending   []byte
	truncated bool
}

func (l *jobLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		line, rest, newline := bytes.Cut(data, []byte{'\n'})
		length := min(maxLogBytes-len(l.pending), len(line))
		l.pending = append(l.pending, line[:length]...)
		l.truncated = l.truncated || length < len(line)
		if newline {
			l.push()
		}
		data = rest
	}
	return size, nil
}

func (l *jobLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 {
		l.push()
	}
}

func (l *jobLog) push() {
	l.lines[l.next] = l.line()
	l.next = (l.next + 1) % maxLogLines
	l.count = min(l.count+1, maxLogLines)
	l.pending = l.pending[:0]
	l.truncated = false
}

func (l *jobLog) line() string {
	line := strings.TrimSuffix(string(l.pending), "\r")
	if l.truncated {
		line += "…"
	}
	return line
}

func (l *jobLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.count
	if len(l.pending) > 0 {
		count = min(count, maxLogLines-1)
	}
	result := make([]string, 0, maxLogLines)
	for i := l.next - count + maxLogLines; i < l.next+maxLogLines; i++ {
		result = append(result, l.lines[i%maxLogLines])
	}
	if len(l.pending) > 0 {
		result = append(result, l.line())
	}
	return result
}
