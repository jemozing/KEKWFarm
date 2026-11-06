package farm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"duckfarm/internal/store"
)

func TestRunnerCredentialsAndHeartbeat(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "flags.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{config: Config{Password: "panel-password"}, store: db}
	handler := s.routes()
	request := func(method, path, authorization, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		r.Header.Set("Authorization", authorization)
		r.RemoteAddr = "[::ffff:192.0.2.42]:32100"
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s returned %d %s", method, path, w.Code, w.Body)
		}
		return w
	}
	const admin = "panel-password"
	created := request("POST", "/api/runners", admin, `{"name":"Worker One"}`, http.StatusCreated)
	var registration struct {
		Runner store.Runner
		Token  string
	}
	if err := json.NewDecoder(created.Body).Decode(&registration); err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + registration.Token
	if registration.Runner.Online || registration.Runner.LastSeen != nil || registration.Token == "" {
		t.Fatal("new runner should be offline with a one-time token")
	}
	request("GET", "/api/runners", auth, "", http.StatusForbidden)
	request("GET", "/api/checker_config", auth, "", http.StatusForbidden)
	request("GET", "/api/get_config", auth, "", http.StatusOK)
	request("POST", "/api/post_flags", auth, `[{"flag":"SYNTHETIC","sploit":"fixture","team":"Local"}]`, http.StatusOK)
	request("POST", "/api/runner/heartbeat", admin, `{"version":"test"}`, http.StatusForbidden)
	request("POST", "/api/runner/heartbeat", auth, `{"version":"test","jobs":{"running":-1}}`, http.StatusBadRequest)
	request("POST", "/api/runner/heartbeat", auth, `{"version":"test","jobs":{"running":2,"scheduled":1}}`, http.StatusNoContent)

	read := func() store.Runner {
		t.Helper()
		response := request("GET", "/api/runners", admin, "", http.StatusOK)
		if strings.Contains(response.Body.String(), registration.Token) {
			t.Fatal("runner list contains its token")
		}
		var runners []store.Runner
		if err := json.NewDecoder(response.Body).Decode(&runners); err != nil {
			t.Fatal(err)
		}
		if len(runners) != 1 {
			t.Fatalf("got %d runners", len(runners))
		}
		return runners[0]
	}
	online := read()
	if !online.Online || online.LastSeen == nil || online.Jobs.Running != 2 || online.Jobs.Scheduled != 1 || online.Address != "192.0.2.42" {
		t.Fatalf("heartbeat was not reflected in runner list: %+v", online)
	}
	request("POST", "/api/runner/heartbeat", auth, `{"version":"test","address":"http://192.0.2.7"}`, http.StatusBadRequest)
	request("POST", "/api/runner/heartbeat", auth, `{"version":"test","address":"2001:0db8::6"}`, http.StatusNoContent)
	if address := read().Address; address != "2001:db8::6" {
		t.Fatalf("expected the canonical advertised IP, got %q", address)
	}
	request("DELETE", "/api/runners/"+registration.Runner.ID, admin, "", http.StatusNoContent)
	request("GET", "/api/get_config", auth, "", http.StatusForbidden)
	request("POST", "/api/post_flags", auth, `[]`, http.StatusForbidden)
	request("POST", "/api/runner/heartbeat", auth, `{"version":"test"}`, http.StatusForbidden)
	if revoked := read(); revoked.Online || !revoked.Revoked {
		t.Fatalf("expected a revoked runner: %+v", revoked)
	}
	request("GET", "/api/get_config", admin, "", http.StatusOK)
}
