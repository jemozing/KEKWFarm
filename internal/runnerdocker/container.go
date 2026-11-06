package runnerdocker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const program = "/kekwfarm-program"

func (e *Engine) Run(ctx context.Context, jobID, image, interpreter string, source *os.File, target string, output io.Writer) (err error) {
	var entrypoint []string
	switch interpreter {
	case "python3":
		entrypoint = []string{"python3", "-u", program}
	case "sh":
		entrypoint = []string{"/bin/sh", program}
	case "binary":
		entrypoint = []string{program}
	default:
		return errors.New("unknown interpreter")
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}

	name := "kekwfarm-job-" + strings.ToLower(rand.Text())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := e.remove(cleanup, name); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove worker container: %w", cleanupErr))
		}
	}()

	spec := map[string]any{
		"Image": image, "Entrypoint": entrypoint, "Cmd": []string{target},
		"User": "65532:65532", "WorkingDir": "/tmp", "Tty": false,
		"Healthcheck": map[string][]string{"Test": {"NONE"}},
		"Labels":      map[string]string{ownerLabel: e.owner, jobLabel: jobID},
		"HostConfig": map[string]any{
			"NetworkMode": e.network, "Init": true, "CapDrop": []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges"}, "PidsLimit": 64, "Memory": 512 << 20,
			"LogConfig": map[string]any{
				"Type": "json-file", "Config": map[string]string{"max-size": "2m", "max-file": "2"},
			},
		},
	}
	if err := e.readJSON(ctx, http.MethodPost, "/containers/create?name="+name, spec, nil); err != nil {
		return fmt.Errorf("create worker container: %w", err)
	}
	path := "/containers/" + name
	if err := e.copyProgram(ctx, path, source); err != nil {
		return fmt.Errorf("copy program to worker: %w", err)
	}
	if err := e.readJSON(ctx, http.MethodPost, path+"/start", nil, nil); err != nil {
		return fmt.Errorf("start worker container: %w", err)
	}
	if err := e.streamLogs(ctx, path, output); err != nil {
		return fmt.Errorf("read worker output: %w", err)
	}
	var verdict struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := e.readJSON(ctx, http.MethodPost, path+"/wait?condition=not-running", nil, &verdict); err != nil {
		return fmt.Errorf("wait for worker container: %w", err)
	}
	if verdict.Error != nil && verdict.Error.Message != "" {
		return errors.New(verdict.Error.Message)
	}
	if verdict.StatusCode != 0 {
		return fmt.Errorf("worker exited with code %d", verdict.StatusCode)
	}
	return nil
}

func (e *Engine) remove(ctx context.Context, id string) error {
	err := e.readJSON(ctx, http.MethodDelete, "/containers/"+id+"?force=1&v=1", nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (e *Engine) copyProgram(ctx context.Context, container string, source *os.File) error {
	reader := tarFiles(archiveFile{name: strings.TrimPrefix(program, "/"), source: source, mode: 0o555})
	defer reader.Close()
	res, err := e.request(ctx, http.MethodPut, container+"/archive?path=/", "application/x-tar", reader)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func (e *Engine) streamLogs(ctx context.Context, container string, output io.Writer) error {
	res, err := e.request(ctx, http.MethodGet, container+"/logs?stdout=1&stderr=1&follow=1", "", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// Non-TTY Docker streams prefix each stdout or stderr chunk with an eight-byte header.
	var header [8]byte
	for {
		_, err := io.ReadFull(res.Body, header[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header[0] != 1 && header[0] != 2 {
			return errors.New("invalid Docker output stream")
		}
		length := binary.BigEndian.Uint32(header[4:])
		if _, err := io.CopyN(output, res.Body, int64(length)); err != nil {
			return err
		}
	}
}
