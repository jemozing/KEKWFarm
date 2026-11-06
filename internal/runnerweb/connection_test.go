package runnerweb

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectionAPIAndStatus(t *testing.T) {
	const password = "runner-web-password"
	token := "kr_" + strings.Repeat("a", 64)
	directory := t.TempDir()
	c, err := newConnection(directory, connectionConfig{URL: "http://farm:5137", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	s := &web{connection: c, password: sha256.Sum256([]byte(password))}
	handler := s.routes()
	request := func(method, auth, body string, want int) string {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, "/api/connection", strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s returned %d: %s", method, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), token) {
			t.Fatal("connection API disclosed the runner token")
		}
		return w.Body.String()
	}
	request(http.MethodGet, "", "", http.StatusForbidden)
	request(http.MethodPut, "Bearer "+token, `{"url":"http://another-host:5137"}`, http.StatusForbidden)
	c.record(0, http.StatusNoContent, nil)
	if state := c.snapshot(); state.Status != "online" || state.LastSeen == nil {
		t.Fatalf("expected online status, got %+v", state)
	}
	request(http.MethodPut, password, `{"url":"https://user:password@example.test"}`, http.StatusBadRequest)
	body := request(http.MethodPut, password, `{"url":"192.168.31.216:5137"}`, http.StatusOK)
	if !strings.Contains(body, `"url":"http://192.168.31.216:5137"`) ||
		!strings.Contains(body, `"status":"connecting"`) || !strings.Contains(body, `"last_seen":null`) {
		t.Fatalf("unexpected saved status: %s", body)
	}
	if active := c.defaults(password); active.Token != token || active.URL != "http://192.168.31.216:5137" {
		t.Fatal("connection update did not retain the token and switch the job destination")
	}

	c.record(0, http.StatusNoContent, nil)
	if state := c.snapshot(); state.Status != "connecting" {
		t.Fatal("heartbeat from the previous connection changed the current status")
	}
	c.record(1, http.StatusForbidden, errors.New("HTTP 403"))
	if state := c.snapshot(); state.Status != "unauthenticated" || state.LastSeen != nil {
		t.Fatalf("unexpected status for a rejected token: %+v", state)
	}
	c.record(1, http.StatusNoContent, nil)
	c.record(1, 0, errors.New("could not send heartbeat"))
	if state := c.snapshot(); state.Status != "offline" || state.LastSeen == nil {
		t.Fatalf("expected offline status with the previous successful heartbeat: %+v", state)
	}

	reopened, err := newConnection(directory, connectionConfig{URL: "http://old-env:5137"})
	if err != nil {
		t.Fatal(err)
	}
	if active := reopened.defaults(password); active.Token != token || active.URL != "http://192.168.31.216:5137" {
		t.Fatal("saved settings did not survive restart")
	}
}

func TestConnectionRequiresTokenForNewLegacyDestination(t *testing.T) {
	c, err := newConnection(t.TempDir(), connectionConfig{URL: "http://farm:5137"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.update("https://new-farm.example", ""); !errors.Is(err, errInvalidConnection) {
		t.Fatal("changing a legacy destination should require a runner token")
	}
	if state := c.snapshot(); state.URL != "http://farm:5137" || state.Status != "legacy" {
		t.Fatalf("failed update modified legacy connection: %+v", state)
	}
}
