package runnerweb

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"duckfarm/internal/runner"
	"duckfarm/internal/runnerdocker"
)

//go:embed web
var assets embed.FS

type web struct {
	scheduler  *Scheduler
	engine     *runnerdocker.Client
	password   [32]byte
	connection *connection
}

type Config struct {
	Listen       string
	Directory    string
	FarmURL      string
	Password     string
	BrokerSocket string
	Token        string
	Version      string
	Address      string
}

func Run(ctx context.Context, config Config) error {
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	engine, err := runnerdocker.Connect(ctx, config.BrokerSocket)
	if err != nil {
		return err
	}
	connection, err := newConnection(config.Directory, connectionConfig{URL: config.FarmURL, Token: config.Token})
	if err != nil {
		return err
	}
	scheduler, err := OpenScheduler(ctx, config.Directory, func(ctx context.Context, job Job, file string, output io.Writer) error {
		spec := runnerdocker.JobSpec{
			ID: job.ID, Image: job.Image, Interpreter: job.Interpreter,
			BuildImage: job.BuildImage, ProgramFilename: job.ProgramFilename,
		}
		if err := engine.Prepare(ctx, spec, output); err != nil {
			return err
		}
		return runner.RunOnce(ctx, connection.defaults(config.Password), runner.RoundOptions{
			Name: job.Name, Path: file, Parallelism: job.Parallelism,
			Timeout: time.Duration(job.TimeoutSeconds) * time.Second,
			Execute: func(ctx context.Context, target string, output io.Writer) error {
				request := spec
				request.Target = target
				return engine.Run(ctx, request, output)
			},
		}, output)
	})
	if err != nil {
		return err
	}
	defer scheduler.Close()
	heartbeatContext, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		connection.monitor(heartbeatContext, config.Version, config.Address, func() runner.HeartbeatJobs {
			return heartbeatJobs(scheduler.List())
		})
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()

	panel := &web{
		scheduler: scheduler, engine: engine, password: sha256.Sum256([]byte(config.Password)), connection: connection,
	}
	server := &http.Server{
		Handler: panel.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Printf("KEKWFarm Runner listening on %s", listener.Addr())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
	}
	return nil
}

func heartbeatJobs(jobs []Job) runner.HeartbeatJobs {
	var counts runner.HeartbeatJobs
	for _, job := range jobs {
		switch job.Status {
		case statusScheduled:
			counts.Scheduled++
		case statusRunning:
			counts.Running++
		case statusStopping:
			counts.Stopping++
		case statusCompleted:
			counts.Completed++
		case statusFailed:
			counts.Failed++
		case statusStopped:
			counts.Stopped++
		}
	}
	return counts
}

func (s *web) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /api/login", s.login)
	api.HandleFunc("GET /api/connection", s.auth(s.getConnection))
	api.HandleFunc("PUT /api/connection", s.auth(s.putConnection))
	api.HandleFunc("GET /api/jobs", s.auth(func(w http.ResponseWriter, _ *http.Request) {
		jobs := s.scheduler.List()
		if jobs == nil {
			jobs = []Job{}
		}
		jsonResponse(w, http.StatusOK, jobs)
	}))
	api.HandleFunc("POST /api/jobs", s.auth(s.add))
	api.HandleFunc("GET /api/jobs/{id}/logs", s.auth(s.logs))
	api.HandleFunc("POST /api/jobs/{id}/stop", s.auth(func(w http.ResponseWriter, r *http.Request) {
		jobResult(w, s.scheduler.Stop(r.PathValue("id")))
	}))
	api.HandleFunc("DELETE /api/jobs/{id}", s.auth(s.delete))

	static, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return http.NewCrossOriginProtection().Handler(mux)
}

func (s *web) matches(password string) bool {
	value := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(value[:], s.password[:]) == 1
}

func (s *web) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.matches(r.Header.Get("Authorization")) {
			problem(w, http.StatusForbidden, "Invalid password")
			return
		}
		next(w, r)
	}
}

func (s *web) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
		problem(w, http.StatusBadRequest, "Invalid login request")
		return
	}
	if !s.matches(input.Password) {
		problem(w, http.StatusForbidden, "Invalid password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *web) add(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize+MaxDockerfileSize+(64<<10))
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, "Upload is too large")
		} else {
			problem(w, http.StatusBadRequest, "Invalid upload form")
		}
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["file"]
	dockerfiles := r.MultipartForm.File["dockerfile"]
	fieldCount := 1
	if len(dockerfiles) == 1 {
		fieldCount++
	}
	if len(files) != 1 || len(dockerfiles) > 1 || len(r.MultipartForm.File) != fieldCount {
		problem(w, http.StatusBadRequest, "Upload one program and optionally one Dockerfile")
		return
	}
	if files[0].Size > MaxUploadSize {
		problem(w, http.StatusRequestEntityTooLarge, "Upload must be at most 10 MiB")
		return
	}
	file, err := files[0].Open()
	if err != nil {
		problem(w, http.StatusBadRequest, "Could not read uploaded file")
		return
	}
	defer file.Close()
	var recipe io.Reader
	if len(dockerfiles) == 1 {
		if dockerfiles[0].Size > MaxDockerfileSize {
			jobResult(w, ErrDockerfileTooLarge)
			return
		}
		dockerfile, err := dockerfiles[0].Open()
		if err != nil {
			problem(w, http.StatusBadRequest, "Could not read uploaded Dockerfile")
			return
		}
		defer dockerfile.Close()
		recipe = dockerfile
	}

	job := Job{
		Name: strings.TrimSpace(r.PostFormValue("name")), Interpreter: r.PostFormValue("interpreter"),
		Image: r.PostFormValue("image"), ProgramFilename: files[0].Filename,
		TimeoutSeconds: 10, Parallelism: 4,
	}
	if job.Name == "" {
		job.Name = files[0].Filename
	}
	if start := r.PostFormValue("start_at"); start != "" {
		job.StartAt, err = time.Parse(time.RFC3339, start)
		if err != nil {
			problem(w, http.StatusBadRequest, "Invalid start time")
			return
		}
	}
	for _, field := range []struct {
		name string
		dest *int
	}{
		{"interval_seconds", &job.IntervalSeconds},
		{"timeout_seconds", &job.TimeoutSeconds},
		{"parallelism", &job.Parallelism},
	} {
		if value := r.PostFormValue(field.name); value != "" {
			*field.dest, err = strconv.Atoi(value)
			if err != nil {
				problem(w, http.StatusBadRequest, "Invalid "+field.name)
				return
			}
		}
	}
	saved, err := s.scheduler.AddWithDockerfile(job, file, recipe)
	if err != nil {
		jobResult(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, saved)
}

func (s *web) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.scheduler.Delete(id); err != nil {
		jobResult(w, err)
		return
	}
	if err := s.engine.Remove(context.Background(), id); err != nil {
		log.Printf("remove generated job image: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *web) logs(w http.ResponseWriter, r *http.Request) {
	lines, err := s.scheduler.Logs(r.PathValue("id"))
	if err != nil {
		jobResult(w, err)
		return
	}
	if lines == nil {
		lines = []string{}
	}
	jsonResponse(w, http.StatusOK, map[string][]string{"lines": lines})
}

func jobResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrJobNotFound):
		problem(w, http.StatusNotFound, "Job not found")
	case errors.Is(err, ErrJobActive), errors.Is(err, ErrJobLimit):
		problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrUploadTooLarge), errors.Is(err, ErrDockerfileTooLarge):
		problem(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, ErrInvalidJob):
		problem(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("Runner job: %v", err)
		problem(w, http.StatusInternalServerError, "Could not update job")
	}
}

func problem(w http.ResponseWriter, status int, detail string) {
	jsonResponse(w, status, map[string]string{"error": detail})
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write runner response: %v", err)
	}
}
