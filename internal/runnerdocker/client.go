package runnerdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type JobSpec struct {
	ID              string `json:"id"`
	Image           string `json:"image"`
	BuildImage      bool   `json:"build_image"`
	ProgramFilename string `json:"program_filename"`
	Interpreter     string `json:"interpreter"`
	Target          string `json:"target,omitempty"`
}

type event struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	Done   bool   `json:"done,omitempty"`
}

type Client struct {
	http *http.Client
}

func unixHTTP(socket string) *http.Client {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			ResponseHeaderTimeout: 20 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func Connect(ctx context.Context, socket string) (*Client, error) {
	client := &Client{http: unixHTTP(socket)}
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		err := client.call(deadline, "ready", JobSpec{}, io.Discard)
		if err == nil {
			return client, nil
		}
		select {
		case <-deadline.Done():
			return nil, fmt.Errorf("runner engine is unavailable: %w", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Client) Prepare(ctx context.Context, spec JobSpec, output io.Writer) error {
	return c.call(ctx, "prepare", spec, output)
}

func (c *Client) Run(ctx context.Context, spec JobSpec, output io.Writer) error {
	return c.call(ctx, "run", spec, output)
}

func (c *Client) Remove(ctx context.Context, id string) error {
	deadline, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	return c.call(deadline, "remove", JobSpec{ID: id}, io.Discard)
}

func (c *Client) call(ctx context.Context, operation string, spec JobSpec, output io.Writer) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runner-engine/"+operation, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("runner engine rejected request (%d): %s", res.StatusCode, message)
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, 64<<20))
	for {
		var message event
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("runner engine closed the output before reporting completion")
			}
			return err
		}
		if message.Output != "" {
			if _, err := io.WriteString(output, message.Output); err != nil {
				return err
			}
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
		if message.Done {
			return nil
		}
	}
}
