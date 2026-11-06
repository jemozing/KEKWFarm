package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type api struct {
	base, password, token string
	client                *http.Client
	logger                *log.Logger
}

type remoteConfig struct {
	Teams      map[string]string `json:"TEAMS"`
	FlagFormat string            `json:"FLAG_FORMAT"`
}

type foundFlag struct {
	Flag   string `json:"flag"`
	Sploit string `json:"sploit"`
	Team   string `json:"team"`
}

type requestError struct{ status int }

func (e requestError) Error() string { return fmt.Sprintf("farm returned HTTP %d", e.status) }

func (a api) request(ctx context.Context, method, path string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	} else {
		req.Header.Set("Authorization", a.password)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, requestError{res.StatusCode}
	}
	return res.Body, nil
}

func (a api) getConfig(ctx context.Context) (remoteConfig, error) {
	var config remoteConfig
	body, err := a.request(ctx, http.MethodGet, "/api/get_config", nil)
	if err != nil {
		return config, err
	}
	defer body.Close()
	err = json.NewDecoder(io.LimitReader(body, 8<<20)).Decode(&config)
	return config, err
}

func (a api) deliver(ctx context.Context, inbox <-chan foundFlag) {
	const maxBatch = 64
	for inbox != nil {
		var first foundFlag
		select {
		case <-ctx.Done():
			return
		case flag, ok := <-inbox:
			if !ok {
				return
			}
			first = flag
		}
		batch := make([]foundFlag, 1, maxBatch)
		batch[0] = first
		timer := time.NewTimer(50 * time.Millisecond)
	collect:
		for len(batch) < maxBatch {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				break collect
			case flag, ok := <-inbox:
				if !ok {
					inbox = nil
					break collect
				}
				batch = append(batch, flag)
			}
		}
		timer.Stop()

		payload, err := json.Marshal(batch)
		if err != nil {
			a.logger.Printf("Encode flags: %v", err)
			continue
		}
		delay := 500 * time.Millisecond
		for ctx.Err() == nil {
			body, err := a.request(ctx, http.MethodPost, "/api/post_flags", bytes.NewReader(payload))
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(body, 4096))
				body.Close()
				break
			}
			var failure requestError
			if errors.As(err, &failure) && (failure.status == 400 || failure.status == 413) {
				a.logger.Printf("Farm rejected a batch of %d flags: %v", len(batch), err)
				break
			}
			if ctx.Err() != nil {
				return
			}
			a.logger.Printf("Flag delivery failed, retrying: %v", err)
			if !pause(ctx, delay) {
				return
			}
			delay = min(2*delay, 5*time.Second)
		}
	}
}
