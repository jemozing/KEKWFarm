package runnerweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTestScheduler(t *testing.T, dir string, run func(context.Context, Job, string, io.Writer) error) *Scheduler {
	t.Helper()
	s, err := OpenScheduler(t.Context(), dir, run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func addTestJob(t *testing.T, s *Scheduler, job Job) Job {
	t.Helper()
	job, err := s.Add(job, strings.NewReader("fake program"))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func waitForJob(t *testing.T, s *Scheduler, id, status string) Job {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, job := range s.List() {
			if job.ID == id && job.Status == status {
				return job
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s: %+v", id, status, s.List())
	return Job{}
}

func TestSchedulerPersistsScheduledAndUploadedProgram(t *testing.T) {
	dir := t.TempDir()
	unexpected := make(chan Job, 1)
	run := func(_ context.Context, job Job, _ string, _ io.Writer) error {
		unexpected <- job
		return nil
	}
	s := openTestScheduler(t, dir, run)
	job := addTestJob(t, s, Job{
		Name: "example.sh", Interpreter: "sh", StartAt: time.Now().Add(time.Hour),
		Image:          "registry.example:5000/ctf/runtime:test-1",
		TimeoutSeconds: 10, Parallelism: 1,
	})
	s.Close()
	s = openTestScheduler(t, dir, run)
	if got := s.List(); !reflect.DeepEqual(got, []Job{job}) {
		t.Fatalf("persisted jobs = %+v, want %+v", got, job)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(dir, "jobs.json"): 0o600, s.program(job.ID): 0o700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("invalid persisted file %s: %v, %v", path, info, err)
		}
	}
	if err := s.Delete(job.ID); !errors.Is(err, ErrJobActive) {
		t.Fatalf("delete scheduled job: %v", err)
	}
	if err := s.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.program(job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("program was not deleted: %v", err)
	}
	s.Close()
	s = openTestScheduler(t, dir, run)
	if got := s.List(); len(got) != 0 || len(unexpected) != 0 {
		t.Fatalf("deleted future job remained or ran: %+v", got)
	}
}

func TestSchedulerPersistsDockerfileAndDeletesUploadPair(t *testing.T) {
	dir := t.TempDir()
	run := func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		t.Error("future job ran")
		return nil
	}
	s := openTestScheduler(t, dir, run)
	job, err := s.AddWithDockerfile(Job{
		Name: "example", ProgramFilename: "example script.py", Interpreter: "python3",
		StartAt: time.Now().Add(time.Hour), TimeoutSeconds: 10, Parallelism: 1,
	}, strings.NewReader("fake program"), strings.NewReader("FROM scratch\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !job.BuildImage || job.Image != "" || job.ProgramFilename != "example script.py" {
		t.Fatalf("unexpected build job: %+v", job)
	}
	if s.DockerfilePath("../outside") != "" || s.DockerfilePath(job.ID) != DockerfileForProgram(s.program(job.ID)) {
		t.Fatal("Dockerfile path escaped upload directory or mismatched program sibling")
	}
	s.Close()
	s = openTestScheduler(t, dir, run)
	if got := s.List(); !reflect.DeepEqual(got, []Job{job}) {
		t.Fatalf("persisted jobs = %+v, want %+v", got, job)
	}
	path := s.DockerfilePath(job.ID)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Dockerfile must be private: %v, %v", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "FROM scratch\n" {
		t.Fatalf("unexpected Dockerfile bytes %q, %v", data, err)
	}
	if err := s.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(job.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.program(job.ID), path} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("upload remained after deleting job: %s, %v", path, err)
		}
	}
}

func TestSchedulerDoesNotStartIncompleteRestoredBuildJob(t *testing.T) {
	dir := t.TempDir()
	run := func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		t.Error("incomplete build job ran")
		return nil
	}
	s := openTestScheduler(t, dir, run)
	job, err := s.AddWithDockerfile(Job{
		Name: "example", ProgramFilename: "example.sh", Interpreter: "sh",
		StartAt: time.Now().Add(time.Hour), TimeoutSeconds: 10, Parallelism: 1,
	}, strings.NewReader("fake program"), strings.NewReader("FROM scratch\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(s.DockerfilePath(job.ID)); err != nil {
		t.Fatal(err)
	}
	orphan := s.program(strings.Repeat("0", 32))
	for _, path := range []string{orphan, DockerfileForProgram(orphan)} {
		if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s = openTestScheduler(t, dir, run)
	failed := waitForJob(t, s, job.ID, statusFailed)
	if failed.LastError != "uploaded Dockerfile is missing" {
		t.Fatalf("unexpected restore error: %q", failed.LastError)
	}
	for _, path := range []string{orphan, DockerfileForProgram(orphan)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan remained after restoring scheduler: %s, %v", path, err)
		}
	}
}

func TestSchedulerStartsBuildJobOnlyAfterBothFilesAreReady(t *testing.T) {
	s := openTestScheduler(t, t.TempDir(), func(_ context.Context, job Job, program string, _ io.Writer) error {
		if !job.BuildImage || job.Image != "" || job.ProgramFilename != "main.py" {
			return fmt.Errorf("unexpected build job: %+v", job)
		}
		for path, want := range map[string]string{program: "fake program", DockerfileForProgram(program): "FROM scratch\n"} {
			if data, err := os.ReadFile(path); err != nil || string(data) != want {
				return fmt.Errorf("missing upload %s: %q, %v", path, data, err)
			}
		}
		return nil
	})
	job, err := s.AddWithDockerfile(Job{
		Name: "example", ProgramFilename: "main.py", Interpreter: "python3", TimeoutSeconds: 10, Parallelism: 1,
	}, strings.NewReader("fake program"), strings.NewReader("FROM scratch\n"))
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, job.ID, statusCompleted)
}

func TestSchedulerRunsJobAndBoundsLogs(t *testing.T) {
	s := openTestScheduler(t, t.TempDir(), func(_ context.Context, job Job, program string, logs io.Writer) error {
		data, err := os.ReadFile(program)
		if err != nil || string(data) != "fake program" || job.Interpreter != "python3" || job.Parallelism != 3 || job.Image != DefaultJobImage {
			return fmt.Errorf("unexpected callback arguments: %v, %s, %+v", err, data, job)
		}
		for i := range maxLogLines + 5 {
			fmt.Fprintln(logs, i)
		}
		fmt.Fprint(logs, strings.Repeat("x", maxLogBytes+20))
		return nil
	})
	job := addTestJob(t, s, Job{Name: "example.py", Interpreter: "python3", TimeoutSeconds: 10, Parallelism: 3})
	complete := waitForJob(t, s, job.ID, statusCompleted)
	if complete.StartAt.IsZero() || !complete.NextAt.IsZero() {
		t.Fatalf("unexpected completed schedule: %+v", complete)
	}
	lines, err := s.Logs(job.ID)
	if err != nil || len(lines) != maxLogLines || lines[0] != "6" || lines[len(lines)-1] != strings.Repeat("x", maxLogBytes)+"…" {
		t.Fatalf("unexpected bounded log, %d lines, err %v", len(lines), err)
	}
}

func TestSchedulerLimitsAndStopsActiveRounds(t *testing.T) {
	started := make(chan string, maxActiveJobs+1)
	s := openTestScheduler(t, t.TempDir(), func(ctx context.Context, job Job, _ string, _ io.Writer) error {
		started <- job.ID
		<-ctx.Done()
		return ctx.Err()
	})
	for range maxActiveJobs + 1 {
		addTestJob(t, s, Job{Name: "hold", Interpreter: "sh", IntervalSeconds: 1, TimeoutSeconds: 10, Parallelism: 1})
	}
	var active string
	for range maxActiveJobs {
		select {
		case active = <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("expected active job to start")
		}
	}
	if err := s.Delete(active); !errors.Is(err, ErrJobActive) {
		t.Fatalf("delete running job: %v", err)
	}
	select {
	case extra := <-started:
		t.Fatalf("unexpected overlapping or fifth round: %s", extra)
	case <-time.After(1100 * time.Millisecond):
	}
	if err := s.Stop(active); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, active, statusStopped)
	select {
	case next := <-started:
		if next == active {
			t.Fatal("stopped job was repeated")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting job did not acquire freed slot")
	}
}

func TestSchedulerResumesRepeatingJobAfterClose(t *testing.T) {
	dir := t.TempDir()
	started := make(chan time.Time, 4)
	run := func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		started <- time.Now()
		return nil
	}
	s := openTestScheduler(t, dir, run)
	job := addTestJob(t, s, Job{
		Name: "repeat", Interpreter: "sh", StartAt: time.Now().Add(150 * time.Millisecond),
		IntervalSeconds: 1, TimeoutSeconds: 5, Parallelism: 1,
	})
	select {
	case first := <-started:
		if first.Before(job.StartAt) {
			t.Fatal("scheduled job started early")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("scheduled job did not start")
	}
	s.Close()
	s = openTestScheduler(t, dir, run)
	select {
	case second := <-started:
		if second.Before(job.StartAt.Add(time.Second)) {
			t.Fatal("repeating job started early after restart")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("repeating job did not resume")
	}
	if err := s.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, job.ID, statusStopped)
}

func TestSchedulerTimesOutOnceAndDoesNotRepeatInterruptedJob(t *testing.T) {
	dir := t.TempDir()
	s := openTestScheduler(t, dir, func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		return context.DeadlineExceeded
	})
	job := addTestJob(t, s, Job{Name: "times out", Interpreter: "binary", TimeoutSeconds: 1, Parallelism: 1})
	failed := waitForJob(t, s, job.ID, statusFailed)
	if failed.LastError != context.DeadlineExceeded.Error() {
		t.Fatalf("unexpected timeout error: %q", failed.LastError)
	}
	s.Close()
	failed.Status = statusRunning // Simulate the manifest left by an interrupted process.
	failed.Image = ""             // The older manifest had no image field.
	data, err := json.Marshal([]Job{failed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s = openTestScheduler(t, dir, func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		t.Error("interrupted one-shot job was executed again")
		return nil
	})
	job = waitForJob(t, s, job.ID, statusFailed)
	if job.LastError != "runner stopped before the round finished" || job.Image != DefaultJobImage {
		t.Fatalf("unexpected crash recovery state: %+v", job)
	}
}

type filledReader struct{}

func (filledReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestSchedulerValidationAndUploadLimits(t *testing.T) {
	s := openTestScheduler(t, t.TempDir(), func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		t.Error("future job ran")
		return nil
	})
	valid := Job{Name: "test", Interpreter: "binary", StartAt: time.Now().Add(time.Hour), TimeoutSeconds: 1, Parallelism: 1}
	for _, mutate := range []func(*Job){
		func(j *Job) { j.Name = " " },
		func(j *Job) { j.Name = strings.Repeat("é", 81) },
		func(j *Job) { j.Interpreter = "bash" },
		func(j *Job) { j.Image = "--privileged" },
		func(j *Job) { j.Image = "image name" },
		func(j *Job) { j.Image = strings.Repeat("a", 256) },
		func(j *Job) { j.IntervalSeconds = 86401 },
		func(j *Job) { j.TimeoutSeconds = 0 },
		func(j *Job) { j.Parallelism = 33 },
	} {
		invalid := valid
		mutate(&invalid)
		if _, err := s.Add(invalid, strings.NewReader("program")); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("invalid job was accepted: %+v, %v", invalid, err)
		}
	}
	if _, err := s.Add(valid, io.LimitReader(filledReader{}, MaxUploadSize+1)); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("oversized upload: %v", err)
	}
	for range maxJobs {
		addTestJob(t, s, valid)
	}
	if _, err := s.Add(valid, strings.NewReader("program")); !errors.Is(err, ErrJobLimit) {
		t.Fatalf("excess job: %v", err)
	}
}

func TestSchedulerRejectsInvalidDockerfileUploadsWithoutLeavingFiles(t *testing.T) {
	s := openTestScheduler(t, t.TempDir(), func(_ context.Context, _ Job, _ string, _ io.Writer) error {
		t.Error("invalid job ran")
		return nil
	})
	valid := Job{Name: "test", ProgramFilename: "run.py", Interpreter: "python3", TimeoutSeconds: 1, Parallelism: 1}
	for _, name := range []string{"", ".", "..", "../x", `dir\x`, "x\ny", "dockerFILE", ".Dockerignore", "Dockerfile.dockerignore", strings.Repeat("x", 161)} {
		invalid := valid
		invalid.ProgramFilename = name
		if _, err := s.AddWithDockerfile(invalid, strings.NewReader("program"), strings.NewReader("FROM scratch\n")); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("unsafe filename %q accepted: %v", name, err)
		}
	}
	conflicting := valid
	conflicting.Image = "python:3"
	if _, err := s.AddWithDockerfile(conflicting, strings.NewReader("program"), strings.NewReader("FROM scratch\n")); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("conflicting image and Dockerfile accepted: %v", err)
	}
	missing := valid
	missing.BuildImage = true
	if _, err := s.Add(missing, strings.NewReader("program")); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("missing Dockerfile accepted: %v", err)
	}
	if _, err := s.AddWithDockerfile(valid, strings.NewReader("program"), strings.NewReader("")); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("empty Dockerfile accepted: %v", err)
	}
	if _, err := s.AddWithDockerfile(valid, strings.NewReader("program"), io.LimitReader(filledReader{}, MaxDockerfileSize+1)); !errors.Is(err, ErrDockerfileTooLarge) {
		t.Fatalf("oversized Dockerfile accepted: %v", err)
	}
	files, err := os.ReadDir(s.uploads)
	if err != nil || len(files) != 0 || len(s.List()) != 0 {
		t.Fatalf("rejected job left uploads or scheduled records: %v, %v", files, err)
	}
}
