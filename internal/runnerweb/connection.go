package runnerweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"duckfarm/internal/runner"
)

var errInvalidConnection = errors.New("invalid connection configuration")

type connectionStatus struct {
	URL             string     `json:"url"`
	Status          string     `json:"status"`
	LastSeen        *time.Time `json:"last_seen"`
	Error           string     `json:"error"`
	TokenConfigured bool       `json:"token_configured"`
}

type connection struct {
	mu        sync.Mutex
	directory string
	config    connectionConfig
	status    connectionStatus
	revision  uint64
	changed   chan struct{}
}

func newConnection(directory string, defaults connectionConfig) (*connection, error) {
	config, err := loadConnection(directory, defaults)
	if err != nil {
		return nil, err
	}
	return &connection{
		directory: directory, config: config, status: pendingConnection(config),
		changed: make(chan struct{}, 1),
	}, nil
}

func pendingConnection(config connectionConfig) connectionStatus {
	status := connectionStatus{URL: config.URL, Status: "connecting", TokenConfigured: config.Token != ""}
	if config.Token == "" {
		status.Status = "legacy"
	}
	return status
}

func (c *connection) defaults(password string) runner.Defaults {
	c.mu.Lock()
	defer c.mu.Unlock()
	return runner.Defaults{URL: c.config.URL, Token: c.config.Token, Password: password}
}

func (c *connection) snapshot() connectionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *connection) update(url, token string) (connectionStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(token) == "" {
		token = c.config.Token
	}
	config, err := validateConnection(connectionConfig{URL: url, Token: token})
	if err != nil {
		return c.status, fmt.Errorf("%w: %v", errInvalidConnection, err)
	}
	if config.Token == "" && config.URL != c.config.URL {
		return c.status, fmt.Errorf("%w: enter a runner token to change the farm URL", errInvalidConnection)
	}
	if err := saveConnection(c.directory, config); err != nil {
		return c.status, err
	}
	if config != c.config {
		c.config = config
		c.status = pendingConnection(config)
		c.revision++
	}
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return c.status, nil
}

func (c *connection) monitor(ctx context.Context, version, address string, jobs func() runner.HeartbeatJobs) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		c.mu.Lock()
		config, revision := c.config, c.revision
		c.mu.Unlock()
		if config.Token != "" {
			code, err := runner.ReportHeartbeat(ctx, runner.Defaults{URL: config.URL, Token: config.Token, Address: address}, version, jobs())
			if ctx.Err() == nil {
				c.record(revision, code, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.changed:
		}
	}
}

func (c *connection) record(revision uint64, code int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revision != revision {
		return
	}
	switch {
	case err == nil:
		now := time.Now().UTC()
		c.status.Status, c.status.LastSeen, c.status.Error = "online", &now, ""
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		c.status.Status, c.status.Error = "unauthenticated", "Farm rejected the runner token"
	default:
		c.status.Status, c.status.Error = "offline", err.Error()
	}
}

func (s *web) getConnection(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, http.StatusOK, s.connection.snapshot())
}

func (s *web) putConnection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		problem(w, http.StatusBadRequest, "Invalid connection request")
		return
	}
	status, err := s.connection.update(input.URL, input.Token)
	if errors.Is(err, errInvalidConnection) {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		log.Printf("Save runner connection: %v", err)
		problem(w, http.StatusInternalServerError, "Could not save runner connection")
		return
	}
	jsonResponse(w, http.StatusOK, status)
}
