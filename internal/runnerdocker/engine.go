package runnerdocker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ownerLabel = "io.kekwfarm.runner.owner"
	jobLabel   = "io.kekwfarm.runner.job"
)

type Engine struct {
	client  *http.Client
	network string
	owner   string
	slots   chan struct{}
}

func Open(ctx context.Context, socket, network, directory string) (*Engine, error) {
	if socket == "" || network == "" || directory == "" {
		return nil, errors.New("Docker socket, worker network and runner data directory are required")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 20 * time.Second,
	}
	engine := &Engine{
		client: &http.Client{Transport: transport}, network: network, slots: make(chan struct{}, 32),
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	marker := filepath.Join(directory, "docker-owner")
	owner, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		owner = []byte(rand.Text())
		err = os.WriteFile(marker, owner, 0o600)
	}
	if err != nil {
		return nil, err
	}
	engine.owner = strings.TrimSpace(string(owner))
	if engine.owner == "" {
		return nil, errors.New("empty Docker ownership marker")
	}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := engine.removeWorkers(check, ""); err != nil {
		return nil, fmt.Errorf("connect to Docker and clean previous worker containers: %w", err)
	}
	return engine, nil
}

func (e *Engine) removeWorkers(ctx context.Context, jobID string) error {
	filters, err := json.Marshal(map[string][]string{"label": {ownerLabel + "=" + e.owner}})
	if err != nil {
		return err
	}
	var containers []struct {
		ID     string
		Labels map[string]string
	}
	path := "/containers/json?all=1&filters=" + url.QueryEscape(string(filters))
	if err := e.readJSON(ctx, http.MethodGet, path, nil, &containers); err != nil {
		return err
	}
	for _, item := range containers {
		if item.Labels[ownerLabel] != e.owner || (jobID != "" && item.Labels[jobLabel] != jobID) {
			continue
		}
		if err := e.remove(ctx, item.ID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) EnsureImage(ctx context.Context, image string) error {
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	exists, err := e.hasImage(check, image)
	if err != nil || exists {
		return err
	}

	res, err := e.request(check, http.MethodPost, "/images/create?fromImage="+url.QueryEscape(image), "", nil)
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	defer res.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(res.Body, 32<<20))
	for {
		var event struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("read Docker pull result: %w", err)
		}
		if event.Error != "" {
			return fmt.Errorf("pull image %s: %s", image, event.Error)
		}
	}
}

func (e *Engine) hasImage(ctx context.Context, image string) (bool, error) {
	res, err := e.request(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", "", nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res.Body.Close()
	return true, nil
}

func (e *Engine) readJSON(ctx context.Context, method, path string, payload, result any) error {
	var body io.Reader
	var contentType string
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	res, err := e.request(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if result != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(result)
	}
	return nil
}

type engineError struct {
	status  int
	message string
}

func (e engineError) Error() string {
	return fmt.Sprintf("Docker returned HTTP %d: %s", e.status, e.message)
}

func hasStatus(err error, status int) bool {
	var response engineError
	return errors.As(err, &response) && response.status == status
}

func (e *Engine) request(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.44"+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	var problem struct{ Message string }
	_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&problem)
	return nil, engineError{res.StatusCode, problem.Message}
}
