package checker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Accepted = "ACCEPTED"
	Rejected = "REJECTED"
	Queued   = "QUEUED"
)

type Config struct {
	Protocol string            `json:"protocol"`
	URL      string            `json:"url"`
	Host     string            `json:"host"`
	Port     int               `json:"port"`
	Token    string            `json:"token"`
	TeamID   string            `json:"team_id"`
	Options  map[string]string `json:"options"`
	Timeout  time.Duration     `json:"-"`
}

type Result struct {
	Flag     string
	Status   string
	Response string
}

// Submit can return both known verdicts and an error. Flags without a verdict
// are eligible for another attempt.
type Submit func(context.Context, []string) ([]Result, error)

func New(config Config) (Submit, error) {
	if config.Timeout < 0 {
		return nil, errors.New("checker timeout must be positive")
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	for option := range config.Options {
		switch option {
		case "validator", "public_key":
			continue
		case "info_limit":
			if config.Protocol == "volgactf" {
				continue
			}
		case "submit_limit":
			if config.Protocol == "volgactf" || config.Protocol == "ctf_moscow" {
				continue
			}
		}
		return nil, fmt.Errorf("unknown checker option %q for %q", option, config.Protocol)
	}

	switch config.Protocol {
	case "forcad_tcp", "ructf_tcp", "ctfcup_tcp", "faust":
		return newTCP(config)
	case "forcad_http", "ructf_http", "ctf01d", "ctf_moscow", "volgactf", "volgactf_old":
		return newHTTP(config)
	default:
		return nil, fmt.Errorf("unknown checker protocol %q", config.Protocol)
	}
}

func endpoint(config Config, scheme, defaultPath string) (*url.URL, error) {
	raw := strings.TrimSpace(config.URL)
	if raw == "" && defaultPath != "" {
		raw = strings.TrimSpace(config.Host)
		if raw != "" && !strings.Contains(raw, "://") {
			if strings.Count(raw, ":") > 1 && !strings.HasPrefix(raw, "[") {
				raw = "[" + raw + "]"
			}
			raw = scheme + "://" + raw
		}
		base, err := parseHTTPURL(raw)
		if err != nil {
			return nil, err
		}
		if config.Port != 0 {
			if config.Port < 1 || config.Port > 65535 {
				return nil, errors.New("checker port must be in 1..65535")
			}
			base.Host = net.JoinHostPort(base.Hostname(), strconv.Itoa(config.Port))
		}
		return base.JoinPath(defaultPath), nil
	}
	return parseHTTPURL(raw)
}

func parseHTTPURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("checker endpoint must be an absolute HTTP(S) URL")
	}
	return parsed, nil
}

func classifyMessage(flag, message, protocol string) Result {
	message = strings.TrimSpace(message)
	message = strings.TrimPrefix(message, "["+flag+"] ")
	text := strings.ToLower(message)
	status := Queued
	switch {
	case containsAny(text, "timeout", "game not started", "try again later", "game over", "is not up", "no such flag", "rate limit", "ratelimit", "too many requests"):
	case strings.Contains(text, "not accepted"):
		status = Rejected
	case containsAny(text, "accepted", "congrat"):
		status = Accepted
	case containsAny(text, "bad", "wrong", "expired", "unknown", "your own", "too old", "not in database", "already submitted", "invalid flag"):
		status = Rejected
	case protocol != "ructf" && containsAny(text, "self", "invalid", "already_submitted", "team_not_found", "too_old"):
		status = Rejected
	case protocol == "forcad" && strings.Contains(text, "stolen"):
		status = Rejected
	}
	return Result{Flag: flag, Status: status, Response: message}
}

func containsAny(text string, snippets ...string) bool {
	for _, snippet := range snippets {
		if strings.Contains(text, snippet) {
			return true
		}
	}
	return false
}

func transientHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}
