package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type HeartbeatJobs struct {
	Scheduled int `json:"scheduled"`
	Running   int `json:"running"`
	Stopping  int `json:"stopping"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Stopped   int `json:"stopped"`
}

var heartbeatClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ReportHeartbeat reports one snapshot and returns the farm's HTTP status.
// Status is zero if no HTTP response was received.
func ReportHeartbeat(ctx context.Context, defaults Defaults, version string, jobs HeartbeatJobs) (int, error) {
	if defaults.Token == "" {
		return 0, errors.New("runner token is required")
	}
	endpoint, err := heartbeatEndpoint(defaults.URL)
	if err != nil {
		return 0, err
	}
	return sendHeartbeat(ctx, endpoint, defaults, version, jobs)
}

// Heartbeat reports runner availability until ctx is canceled. A runner with no
// token uses legacy authentication and does not register.
func Heartbeat(ctx context.Context, defaults Defaults, version string, snapshot func() HeartbeatJobs) {
	if defaults.Token == "" {
		return
	}
	endpoint, err := heartbeatEndpoint(defaults.URL)
	if err != nil {
		log.Printf("Runner heartbeat: %v", err)
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var previousError string
	for ctx.Err() == nil {
		var jobs HeartbeatJobs
		if snapshot != nil {
			jobs = snapshot()
		}
		_, err := sendHeartbeat(ctx, endpoint, defaults, version, jobs)
		if err == nil {
			previousError = ""
		} else if ctx.Err() == nil && err.Error() != previousError {
			previousError = err.Error()
			log.Printf("Runner heartbeat: %s", previousError)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func heartbeatEndpoint(base string) (string, error) {
	if base == "" {
		base = "http://127.0.0.1:5137"
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("invalid farm URL")
	}
	return parsed.JoinPath("api", "runner", "heartbeat").String(), nil
}

// NormalizeAdvertisedAddress accepts an optional IP literal without a zone or port.
func NormalizeAdvertisedAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return "", errors.New("runner address must be a bare IPv4 or IPv6 address")
	}
	return address.Unmap().String(), nil
}

func sendHeartbeat(ctx context.Context, endpoint string, defaults Defaults, version string, jobs HeartbeatJobs) (int, error) {
	address, err := NormalizeAdvertisedAddress(defaults.Address)
	if err != nil {
		return 0, err
	}
	if version == "" {
		version = "dev"
	}
	data, err := json.Marshal(struct {
		Version string        `json:"version"`
		Jobs    HeartbeatJobs `json:"jobs"`
		Address string        `json:"address,omitempty"`
	}{version, jobs, address})
	if err != nil {
		return 0, errors.New("could not encode heartbeat")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("could not create heartbeat request")
	}
	request.Header.Set("Authorization", "Bearer "+defaults.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := heartbeatClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("could not send heartbeat request")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return response.StatusCode, fmt.Errorf("farm returned HTTP %d", response.StatusCode)
	}
	return response.StatusCode, nil
}
