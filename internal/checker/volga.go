package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type volgaVerdict struct {
	status  string
	message string
}

var volgaVerdicts = map[string]volgaVerdict{
	"SUCCESS":                       {Accepted, "success"},
	"ERROR_UNKNOWN":                 {Queued, "unknown error"},
	"ERROR_ACCESS_DENIED":           {Queued, "access denied"},
	"ERROR_COMPETITION_NOT_STARTED": {Queued, "competition has not started"},
	"ERROR_COMPETITION_PAUSED":      {Queued, "competition is paused"},
	"ERROR_COMPETITION_FINISHED":    {Queued, "competition has finished"},
	"ERROR_FLAG_INVALID":            {Rejected, "invalid flag"},
	"ERROR_RATELIMIT":               {Queued, "ratelimit exceeded"},
	"ERROR_FLAG_EXPIRED":            {Rejected, "expired"},
	"ERROR_FLAG_YOUR_OWN":           {Rejected, "your own flag"},
	"ERROR_FLAG_SUBMITTED":          {Rejected, "already submitted"},
	"ERROR_FLAG_NOT_FOUND":          {Rejected, "not found"},
	"ERROR_SERVICE_STATE_INVALID":   {Queued, "attacking team service down"},
}

func newVolga(client *http.Client, config Config) (Submit, error) {
	address, err := endpoint(config, "https", "/api/flag/v1/submit")
	if err != nil {
		return nil, err
	}
	infoLimit, err := checkerLimit(config.Options, "info_limit")
	if err != nil {
		return nil, err
	}
	limit, err := checkerLimit(config.Options, "submit_limit")
	if err != nil {
		return nil, err
	}
	moscow := config.Protocol == "ctf_moscow"
	return func(ctx context.Context, flags []string) ([]Result, error) {
		results := make([]Result, 0, len(flags))
		eligible := make([]string, 0, len(flags))
		infoCount := min(infoLimit, len(flags))
		for _, flag := range flags[:infoCount] {
			fresh, result, err := volgaInfo(ctx, client, address, flag)
			if err != nil {
				return results, err
			}
			if fresh {
				eligible = append(eligible, flag)
			} else {
				results = append(results, result)
			}
		}
		eligible = append(eligible, flags[infoCount:]...)
		if limit > 0 && len(eligible) > limit {
			for _, flag := range eligible[limit:] {
				results = append(results, Result{Flag: flag, Status: Queued, Response: "flag submission ratelimit"})
			}
			eligible = eligible[:limit]
		}
		for _, flag := range eligible {
			code, body, err := callHTTP(ctx, client, http.MethodPost, address.String(), "text/plain", "", strings.NewReader(flag))
			if err != nil {
				return results, err
			}
			message := strings.TrimSpace(string(body))
			if code == http.StatusTooManyRequests {
				message = "ERROR_RATELIMIT"
			}
			if (moscow && !moscowResponseStatus(code)) || (message == "SUCCESS" && code != http.StatusOK) {
				message = "ERROR_UNKNOWN"
			}
			result := volgaResult(flag, message, moscow)
			results = append(results, result)
			if message == "ERROR_RATELIMIT" {
				return results, fmt.Errorf("checker submission rate limit reached")
			}
		}
		return results, nil
	}, nil
}

func checkerLimit(options map[string]string, name string) (int, error) {
	value, found := options[name]
	if !found {
		return 0, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("checker option %s must be a non-negative integer", name)
	}
	return limit, nil
}

func moscowResponseStatus(status int) bool {
	switch status {
	case http.StatusOK, http.StatusBadRequest, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func volgaResult(flag, code string, moscow bool) Result {
	verdict, ok := volgaVerdicts[code]
	if !ok {
		code = "ERROR_UNKNOWN"
		verdict = volgaVerdicts[code]
	}
	if moscow {
		verdict.message = code
		switch code {
		case "ERROR_COMPETITION_FINISHED":
			verdict.status = Rejected
		case "ERROR_FLAG_NOT_FOUND":
			verdict.status = Queued
		}
	}
	return Result{Flag: flag, Status: verdict.status, Response: verdict.message}
}

func volgaInfo(ctx context.Context, client *http.Client, address *url.URL, flag string) (bool, Result, error) {
	info := address.JoinPath("..", "info")
	info.RawPath = info.EscapedPath() + "/" + url.PathEscape(flag)
	info.Path += "/" + flag
	code, body, err := callHTTP(ctx, client, http.MethodGet, info.String(), "", "", nil)
	if err != nil {
		return false, Result{}, err
	}
	result := Result{Flag: flag, Status: Queued}
	if code != http.StatusOK {
		message := strings.TrimSpace(string(body))
		switch {
		case code == http.StatusTooManyRequests || message == "ERROR_RATELIMIT":
			result.Response = "flag info ratelimit"
		case message == "ERROR_ACCESS_DENIED" || message == "ERROR_NOT_FOUND":
			result.Response = "error response from flag getinfo: " + message
		default:
			result.Response = fmt.Sprintf("flag info returned HTTP %d", code)
		}
		return false, result, nil
	}
	var data struct {
		Expires time.Time `json:"exp"`
	}
	if err := json.Unmarshal(body, &data); err != nil || data.Expires.IsZero() {
		result.Response = "invalid flag info expiry"
		return false, result, nil
	}
	if data.Expires.Before(time.Now().Add(2 * time.Second)) {
		result.Status, result.Response = Rejected, "expired"
		return false, result, nil
	}
	return true, Result{}, nil
}
