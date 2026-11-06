package checker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const maxHTTPResponse = 2 * 1024 * 1024

func newHTTP(config Config) (Submit, error) {
	client := &http.Client{
		Timeout: config.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	switch config.Protocol {
	case "volgactf", "ctf_moscow":
		return newVolga(client, config)
	case "volgactf_old":
		address, err := endpoint(config, "http", "/api/submit")
		if err != nil {
			return nil, err
		}
		return themis(client, address), nil
	}

	address, err := endpoint(config, "", "")
	if err != nil {
		return nil, err
	}
	if config.Protocol == "ctf01d" {
		if strings.TrimSpace(config.TeamID) == "" {
			return nil, errors.New("ctf01d requires checker team_id")
		}
		return ctf01d(client, address, config.TeamID), nil
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("%s requires checker token", config.Protocol)
	}
	protocol := strings.TrimSuffix(config.Protocol, "_http")
	return batchHTTP(client, address, config.Token, protocol), nil
}

func callHTTP(ctx context.Context, client *http.Client, method, address, contentType, token string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return 0, nil, errors.New("cannot build checker HTTP request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("X-Team-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			// The full request URL can contain a flag.
			err = requestErr.Err
		}
		return 0, nil, fmt.Errorf("checker HTTP request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponse+1))
	if err != nil {
		return 0, nil, fmt.Errorf("cannot read checker response: %w", err)
	}
	if len(data) > maxHTTPResponse {
		return 0, nil, errors.New("checker response too large")
	}
	return resp.StatusCode, data, nil
}

func batchHTTP(client *http.Client, address *url.URL, token, protocol string) Submit {
	return func(ctx context.Context, flags []string) ([]Result, error) {
		if len(flags) == 0 {
			return nil, nil
		}
		data, _ := json.Marshal(flags)
		status, body, err := callHTTP(ctx, client, http.MethodPut, address.String(), "application/json", token, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("checker returned HTTP %d", status)
		}
		var rows []struct {
			Flag string `json:"flag"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("invalid checker JSON response: %w", err)
		}
		pending := make(map[string]bool, len(flags))
		for _, flag := range flags {
			pending[flag] = true
		}
		results := make([]Result, 0, len(flags))
		for _, row := range rows {
			if pending[row.Flag] {
				results = append(results, classifyMessage(row.Flag, row.Msg, protocol))
				delete(pending, row.Flag)
			}
		}
		if len(pending) > 0 {
			return results, fmt.Errorf("checker omitted %d flag verdicts", len(pending))
		}
		return results, nil
	}
}

func ctf01d(client *http.Client, address *url.URL, teamID string) Submit {
	return func(ctx context.Context, flags []string) ([]Result, error) {
		results := make([]Result, 0, len(flags))
		for _, flag := range flags {
			flagURL := *address
			params := flagURL.Query()
			params.Set("teamid", teamID)
			params.Set("flag", flag)
			flagURL.RawQuery = params.Encode()
			code, body, err := callHTTP(ctx, client, http.MethodGet, flagURL.String(), "", "", nil)
			if err != nil {
				return results, err
			}
			status := Rejected
			switch {
			case code == http.StatusOK:
				status = Accepted
			case transientHTTPStatus(code):
				status = Queued
			case code < 200 || (code >= 300 && code < 400):
				return results, fmt.Errorf("checker returned HTTP %d", code)
			}
			results = append(results, Result{Flag: flag, Status: status, Response: string(body)})
		}
		return results, nil
	}
}

var themisFlag = regexp.MustCompile(`^[0-9a-f]{32}=$`)

var themisCodes = [...]string{
	"SUCCESS_FLAG_ACCEPTED", "ERROR_GENERIC", "ERROR_INVALID_IDENTITY", "ERROR_CONTEST_NOT_STARTED", "ERROR_CONTEST_PAUSED",
	"ERROR_CONTEST_COMPLETED", "ERROR_INVALID_FORMAT", "ERROR_ATTEMPTS_LIMIT", "ERROR_FLAG_EXPIRED", "ERROR_FLAG_YOURS",
	"ERROR_FLAG_SUBMITTED", "ERROR_FLAG_NOT_FOUND", "ERROR_SERVICE_NOT_UP",
}

func themis(client *http.Client, address *url.URL) Submit {
	return func(ctx context.Context, flags []string) ([]Result, error) {
		results := make([]Result, 0, len(flags))
		valid := make([]string, 0, len(flags))
		for _, flag := range flags {
			if themisFlag.MatchString(flag) {
				valid = append(valid, flag)
			} else {
				results = append(results, Result{Flag: flag, Status: Rejected, Response: "ERROR_INVALID_FORMAT"})
			}
		}
		if len(valid) == 0 {
			return results, nil
		}
		data, _ := json.Marshal(valid)
		code, body, err := callHTTP(ctx, client, http.MethodPost, address.String(), "application/json", "", bytes.NewReader(data))
		if err != nil {
			return results, err
		}
		if code != http.StatusOK {
			return results, fmt.Errorf("Themis checker returned HTTP %d", code)
		}
		var codes []*int
		if err := json.Unmarshal(body, &codes); err != nil {
			return results, fmt.Errorf("invalid Themis checker response: %w", err)
		}
		for i, value := range codes[:min(len(codes), len(valid))] {
			if value == nil {
				continue
			}
			code := *value
			status, message := Queued, fmt.Sprintf("unknown Themis result %d", code)
			if code >= 0 && code < len(themisCodes) {
				message = themisCodes[code]
			}
			switch code {
			case 0:
				status = Accepted
			case 8, 9, 10, 11:
				status = Rejected
			}
			results = append(results, Result{Flag: valid[i], Status: status, Response: message})
		}
		if len(results) != len(flags) || len(codes) != len(valid) {
			return results, errors.New("Themis checker returned an unexpected number of verdicts")
		}
		return results, nil
	}
}
