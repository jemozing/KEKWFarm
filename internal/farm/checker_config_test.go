package farm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"duckfarm/internal/checker"
	"duckfarm/internal/store"
)

func TestCheckerConfigPersistsDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.sqlite")
	fallback := checker.Config{Protocol: "forcad_tcp", Host: "judge", Port: 31337, Token: "example"}
	open := func() *server {
		t.Helper()
		db, err := store.Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s := &server{config: Config{Checker: fallback, Password: "test"}, store: db, checkerChanged: make(chan struct{}, 1)}
		if err := s.loadChecker(t.Context()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	request := func(s *server, method, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, "/api/checker_config", strings.NewReader(body))
		r.Header.Set("Authorization", "test")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s returned %d, headers %v and body %s", method, w.Code, w.Header(), w.Body)
		}
		return w
	}

	s := open()
	if s.currentChecker().submit == nil {
		t.Fatal("expected the configured fallback checker")
	}
	request(s, http.MethodPut, `{"protocol":"unknown"}`, http.StatusBadRequest)
	if s.currentChecker().config.Protocol != fallback.Protocol {
		t.Fatal("invalid save replaced the active checker")
	}
	request(s, http.MethodPut, `{"protocol":""}`, http.StatusOK)
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	response := request(reopened, http.MethodGet, "", http.StatusOK)
	var saved checker.Config
	if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Protocol != "" || reopened.currentChecker().submit != nil {
		t.Fatal("restart re-enabled the fallback checker")
	}
}
