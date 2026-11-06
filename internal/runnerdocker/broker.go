package runnerdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var jobIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var imagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:@_-]{0,254}$`)

type BrokerConfig struct {
	Uploads      string
	Control      string
	DockerSocket string
	Network      string
}

type broker struct {
	engine *Engine
	files  *os.Root
	builds chan struct{}
}

// ServeBroker exposes only the job operations over a private Unix socket.
func ServeBroker(ctx context.Context, cfg BrokerConfig) error {
	files, err := os.OpenRoot(cfg.Uploads)
	if err != nil {
		return err
	}
	defer files.Close()
	socket := path.Join(cfg.Control, "engine.sock")
	if err := os.MkdirAll(cfg.Control, 0o700); err != nil {
		return err
	}
	listener, err := listenLocal(socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	engine, err := Open(ctx, cfg.DockerSocket, cfg.Network, cfg.Control)
	if err != nil {
		return err
	}

	b := &broker{engine: engine, files: files, builds: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ready", b.serve(b.ready))
	mux.HandleFunc("POST /prepare", b.serve(b.prepare))
	mux.HandleFunc("POST /run", b.serve(b.run))
	mux.HandleFunc("POST /remove", b.serve(b.remove))
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func listenLocal(socket string) (*net.UnixListener, error) {
	addr := &net.UnixAddr{Name: socket, Net: "unix"}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("runner engine socket path is occupied")
		}
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("runner engine socket is already in use")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func (b *broker) serve(operation func(context.Context, JobSpec, io.Writer) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		var spec JobSpec
		if err := decoder.Decode(&spec); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
			http.Error(w, "Invalid runner operation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		stream := eventWriter{json.NewEncoder(w), http.NewResponseController(w)}
		if err := stream.flush(); err != nil {
			return
		}
		if err := operation(r.Context(), spec, stream); err != nil {
			_ = stream.encoder.Encode(event{Error: err.Error()})
			return
		}
		_ = stream.encoder.Encode(event{Done: true})
	}
}

type eventWriter struct {
	encoder *json.Encoder
	control *http.ResponseController
}

func (w eventWriter) Write(data []byte) (int, error) {
	if err := w.encoder.Encode(event{Output: string(data)}); err != nil {
		return 0, err
	}
	return len(data), w.flush()
}

func (w eventWriter) flush() error {
	return w.control.Flush()
}

func (b *broker) ready(context.Context, JobSpec, io.Writer) error {
	return nil
}

func (b *broker) prepare(ctx context.Context, spec JobSpec, output io.Writer) error {
	if err := spec.validate(); err != nil {
		return err
	}
	select {
	case b.builds <- struct{}{}:
		defer func() { <-b.builds }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if !spec.BuildImage {
		return b.engine.EnsureImage(ctx, spec.Image)
	}
	program, err := b.openUpload(spec.ID, 10<<20)
	if err != nil {
		return err
	}
	defer program.Close()
	recipe, err := b.openUpload(spec.ID+".Dockerfile", 256<<10)
	if err != nil {
		return err
	}
	defer recipe.Close()
	return b.engine.BuildImage(ctx, BuiltImage(spec.ID), recipe, program, spec.ProgramFilename, output)
}

func (b *broker) run(ctx context.Context, spec JobSpec, output io.Writer) error {
	if err := spec.validate(); err != nil {
		return err
	}
	if spec.Target == "" || len(spec.Target) > 512 || strings.ContainsAny(spec.Target, "\x00\r\n") {
		return errors.New("invalid target")
	}
	source, err := b.openUpload(spec.ID, 10<<20)
	if err != nil {
		return err
	}
	defer source.Close()
	image := spec.Image
	if spec.BuildImage {
		image = BuiltImage(spec.ID)
	}
	return b.engine.Run(ctx, spec.ID, image, spec.Interpreter, source, spec.Target, output)
}

func (b *broker) remove(ctx context.Context, spec JobSpec, _ io.Writer) error {
	if !jobIDPattern.MatchString(spec.ID) {
		return errors.New("invalid job ID")
	}
	return b.engine.RemoveBuiltImage(ctx, spec.ID)
}

func (spec JobSpec) validate() error {
	if !jobIDPattern.MatchString(spec.ID) {
		return errors.New("invalid job ID")
	}
	switch spec.Interpreter {
	case "python3", "sh", "binary":
	default:
		return errors.New("invalid interpreter")
	}
	if !spec.BuildImage {
		if !imagePattern.MatchString(spec.Image) {
			return errors.New("invalid image name")
		}
		return nil
	}
	name := spec.ProgramFilename
	if spec.Image != "" || name == "" || len(name) > 160 || path.Base(name) != name ||
		strings.ContainsAny(name, "\\\x00\r\n") || name == "." || name == ".." ||
		strings.EqualFold(name, "Dockerfile") || strings.EqualFold(name, ".dockerignore") ||
		strings.EqualFold(name, "Dockerfile.dockerignore") {
		return errors.New("invalid Docker build inputs")
	}
	return nil
}

func (b *broker) openUpload(name string, maximum int64) (*os.File, error) {
	file, err := b.files.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		file.Close()
		return nil, fmt.Errorf("invalid upload file")
	}
	return file, nil
}
