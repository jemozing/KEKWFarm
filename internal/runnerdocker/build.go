package runnerdocker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type archiveFile struct {
	name   string
	source *os.File
	mode   int64
}

// BuiltImage is the local tag reserved for this job's generated image.
func BuiltImage(jobID string) string {
	return "kekwfarm-job:" + jobID
}

func (e *Engine) BuildImage(ctx context.Context, tag string, recipe, source *os.File, filename string, output io.Writer) error {
	build, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	exists, err := e.hasImage(build, tag)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(output, "Using previously built image %s\n", tag)
		return nil
	}
	if filename == "" || filename == "." || filename == ".." || path.Base(filename) != filename ||
		strings.Contains(filename, "\\") || strings.EqualFold(filename, "Dockerfile") {
		return fmt.Errorf("invalid program filename for Docker build context")
	}
	fmt.Fprintf(output, "Building %s from Dockerfile\n", tag)
	contextTar := tarFiles(
		archiveFile{name: "Dockerfile", source: recipe, mode: 0o644},
		archiveFile{name: filename, source: source, mode: 0o755},
	)
	defer contextTar.Close()

	query := url.Values{
		"t": {tag}, "dockerfile": {"Dockerfile"}, "version": {"1"},
		"rm": {"1"}, "forcerm": {"1"},
		"memory": {"1073741824"},
	}
	res, err := e.request(build, http.MethodPost, "/build?"+query.Encode(), "application/x-tar", contextTar)
	if err != nil {
		return fmt.Errorf("start Docker build: %w", err)
	}
	defer res.Body.Close()

	limited := &io.LimitedReader{R: res.Body, N: 32 << 20}
	decoder := json.NewDecoder(limited)
	for {
		var event struct {
			Stream      string
			Error       string
			ErrorDetail struct{ Message string }
		}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read Docker build result: %w", err)
		}
		if event.Stream != "" {
			fmt.Fprint(output, event.Stream)
		}
		if message := event.ErrorDetail.Message; message != "" {
			return fmt.Errorf("Docker build: %s", message)
		}
		if event.Error != "" {
			return fmt.Errorf("Docker build: %s", event.Error)
		}
	}
	if limited.N == 0 {
		return fmt.Errorf("Docker build output exceeded its limit")
	}
	exists, err = e.hasImage(build, tag)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Docker build did not create image %s", tag)
	}
	fmt.Fprintf(output, "Image ready: %s\n", tag)
	return nil
}

func (e *Engine) RemoveBuiltImage(ctx context.Context, jobID string) error {
	remove, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.removeWorkers(remove, jobID); err != nil {
		return err
	}
	err := e.readJSON(remove, http.MethodDelete, "/images/"+url.PathEscape(BuiltImage(jobID))+"?noprune=1", nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func tarFiles(files ...archiveFile) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		archive := tar.NewWriter(writer)
		for _, source := range files {
			if err := writeArchiveFile(archive, source); err != nil {
				writer.CloseWithError(err)
				return
			}
		}
		writer.CloseWithError(archive.Close())
	}()
	return reader
}

func writeArchiveFile(archive *tar.Writer, source archiveFile) error {
	info, err := source.source.Stat()
	if err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: source.name, Size: info.Size(), Mode: source.mode}); err != nil {
		return err
	}
	_, err = io.Copy(archive, source.source)
	return err
}
