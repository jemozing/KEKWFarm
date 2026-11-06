package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type heartbeatTransport func(*http.Request) (*http.Response, error)

func (f heartbeatTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestReportHeartbeat(t *testing.T) {
	token := "kr_" + strings.Repeat("c", 64)
	jobs := HeartbeatJobs{Scheduled: 2, Running: 1, Completed: 5}
	originalClient := heartbeatClient
	client := *originalClient
	client.Transport = heartbeatTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/prefix/api/runner/heartbeat", "/advertised/api/runner/heartbeat":
			var body struct {
				Version string        `json:"version"`
				Jobs    HeartbeatJobs `json:"jobs"`
				Address *string       `json:"address"`
			}
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Content-Type") != "application/json" {
				t.Error("invalid heartbeat request")
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version != "dev" || body.Jobs != jobs {
				t.Errorf("invalid heartbeat payload: %+v, %v", body, err)
			}
			if r.URL.Path == "/advertised/api/runner/heartbeat" {
				if body.Address == nil || *body.Address != "2001:db8::42" {
					t.Error("heartbeat omitted or misformatted the advertised address")
				}
			} else if body.Address != nil {
				t.Error("heartbeat should omit an unset address")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/refused/api/runner/heartbeat":
			http.Error(w, token, http.StatusForbidden)
		case "/redirect/api/runner/heartbeat":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		default:
			t.Error("heartbeat followed a redirect or used the wrong path")
			w.WriteHeader(http.StatusNotFound)
		}
		return w.Result(), nil
	})
	heartbeatClient = &client
	t.Cleanup(func() { heartbeatClient = originalClient })
	const baseURL = "https://farm.example"

	for _, tc := range []struct {
		path    string
		address string
		want    int
	}{
		{"/prefix/", "", http.StatusNoContent},
		{"/advertised", "2001:0DB8:0:0:0:0:0:42", http.StatusNoContent},
		{"/refused", "", http.StatusForbidden},
		{"/redirect", "", http.StatusTemporaryRedirect},
	} {
		status, err := ReportHeartbeat(t.Context(), Defaults{URL: baseURL + tc.path, Token: token, Address: tc.address}, "", jobs)
		if status != tc.want || (err != nil) != (tc.want != http.StatusNoContent) {
			t.Fatalf("%s returned status %d and error %v", tc.path, status, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Fatal("heartbeat error included credentials")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if status, err := ReportHeartbeat(canceled, Defaults{URL: baseURL, Token: token}, "test", jobs); status != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled heartbeat returned status %d and error %v", status, err)
	}
}

func TestNormalizeAdvertisedAddress(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"", ""},
		{" 192.0.2.1 ", "192.0.2.1"},
		{"::ffff:192.0.2.1", "192.0.2.1"},
	} {
		if got, err := NormalizeAdvertisedAddress(tc.input); err != nil || got != tc.want {
			t.Errorf("address %q normalized to %q, error %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"farm.local", "https://192.0.2.1", "192.0.2.1:5138", "[2001:db8::1]", "fe80::1%eth0"} {
		if got, err := NormalizeAdvertisedAddress(input); err == nil || got != "" {
			t.Errorf("expected %q to be rejected", input)
		}
	}
}
