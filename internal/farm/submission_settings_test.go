package farm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"duckfarm/internal/checker"
	"duckfarm/internal/store"
)

func TestSubmissionSettingsApplyAndPersist(t *testing.T) {
	const (
		admin = "local-settings-test-password"
		path  = "/api/submission_settings"
	)
	type settings struct {
		FlagFormat   string `json:"flag_format"`
		FlagLifetime int    `json:"flag_lifetime_seconds"`
		SubmitPeriod int    `json:"submit_period_seconds"`
		SubmitLimit  int    `json:"submit_flag_limit"`
	}
	fallback := settings{FlagFormat: "OLD-[A-Z]+", FlagLifetime: 300, SubmitPeriod: 2, SubmitLimit: 100}
	updated := settings{FlagFormat: "NEW-[A-Z]+", FlagLifetime: 450, SubmitPeriod: 7, SubmitLimit: 9}
	judge := checker.Config{Protocol: "forcad_tcp", Host: "judge.invalid", Port: 31337, Token: "checker-fixture"}
	base := Config{
		Database: filepath.Join(t.TempDir(), "farm.sqlite"), Password: admin,
		FlagFormat: fallback.FlagFormat, FlagLifetime: fallback.FlagLifetime,
		SubmitPeriod: fallback.SubmitPeriod, SubmitLimit: fallback.SubmitLimit,
		Timezone: "UTC",
	}
	open := func(config Config) (*server, http.Handler) {
		t.Helper()
		db, err := store.Open(t.Context(), config.Database)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s := &server{
			config: config, store: db, location: time.UTC, wake: make(chan struct{}, 1),
			settingsChanged: make(chan struct{}, 1),
		}
		if err := s.loadChecker(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := s.loadSubmissionSettings(t.Context()); err != nil {
			t.Fatal(err)
		}
		return s, s.routes()
	}
	request := func(handler http.Handler, method, route, auth, body string, want int, out any) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, route, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s with %q returned %d, want %d: %s", method, route, body, w.Code, want, w.Body)
		}
		if route == path && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s was cacheable: %v", method, route, w.Header())
		}
		if out != nil {
			if err := json.NewDecoder(w.Body).Decode(out); err != nil {
				t.Fatalf("%s %s response: %v", method, route, err)
			}
		}
	}
	encode := func(value settings) string {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	checkCurrent := func(handler http.Handler, want settings) {
		t.Helper()
		var current settings
		request(handler, http.MethodGet, path, admin, "", http.StatusOK, &current)
		if current != want {
			t.Fatalf("current settings = %+v, want %+v", current, want)
		}
	}
	checkLegacy := func(handler http.Handler, auth string, want settings) {
		t.Helper()
		var legacy struct {
			FlagFormat   string `json:"FLAG_FORMAT"`
			FlagLifetime int    `json:"FLAG_LIFETIME"`
			SubmitPeriod int    `json:"SUBMIT_PERIOD"`
			SubmitLimit  int    `json:"SUBMIT_FLAG_LIMIT"`
		}
		request(handler, http.MethodGet, "/api/get_config", auth, "", http.StatusOK, &legacy)
		if (settings{legacy.FlagFormat, legacy.FlagLifetime, legacy.SubmitPeriod, legacy.SubmitLimit}) != want {
			t.Fatalf("runner config = %+v, want %+v", legacy, want)
		}
	}
	checkChecker := func(handler http.Handler) {
		t.Helper()
		var checkerAfter checker.Config
		request(handler, http.MethodGet, "/api/checker_config", admin, "", http.StatusOK, &checkerAfter)
		if !reflect.DeepEqual(checkerAfter, judge) {
			t.Fatalf("settings save changed checker: %+v", checkerAfter)
		}
	}

	s, routes := open(base)
	_, token, err := s.store.CreateRunner(t.Context(), "Fixture runner")
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + token
	request(routes, http.MethodGet, path, "", "", http.StatusForbidden, nil)
	request(routes, http.MethodPut, path, "", encode(updated), http.StatusForbidden, nil)
	request(routes, http.MethodPut, path, bearer, encode(updated), http.StatusForbidden, nil)
	checkCurrent(routes, fallback)
	checkLegacy(routes, bearer, fallback)

	rawJudge, err := json.Marshal(judge)
	if err != nil {
		t.Fatal(err)
	}
	request(routes, http.MethodPut, "/api/checker_config", admin, string(rawJudge), http.StatusOK, nil)
	var saved settings
	request(routes, http.MethodPut, path, admin, encode(updated), http.StatusOK, &saved)
	if saved != updated {
		t.Fatalf("saved settings = %+v, want %+v", saved, updated)
	}
	for _, invalid := range []settings{
		{FlagFormat: "(", FlagLifetime: 300, SubmitPeriod: 2, SubmitLimit: 100},
		{FlagFormat: "BAD-[A-Z]+", FlagLifetime: 0, SubmitPeriod: 2, SubmitLimit: 100},
		{FlagFormat: "BAD-[A-Z]+", FlagLifetime: 300, SubmitPeriod: 86401, SubmitLimit: 100},
		{FlagFormat: "BAD-[A-Z]+", FlagLifetime: 300, SubmitPeriod: 2, SubmitLimit: 10001},
	} {
		request(routes, http.MethodPut, path, admin, encode(invalid), http.StatusBadRequest, nil)
		checkCurrent(routes, updated)
	}
	checkLegacy(routes, bearer, updated)
	checkChecker(routes)

	var filtered struct {
		FlagFormat string `json:"flag_format"`
	}
	request(routes, http.MethodGet, "/api/filter_config", admin, "", http.StatusOK, &filtered)
	if filtered.FlagFormat != updated.FlagFormat {
		t.Fatalf("panel flag format = %q, want %q", filtered.FlagFormat, updated.FlagFormat)
	}
	request(routes, http.MethodPost, "/api/post_flags", admin, `{"text":"OLD-ONE"}`, http.StatusBadRequest, nil)
	var ingest struct {
		Queued int `json:"queued"`
	}
	request(routes, http.MethodPost, "/api/post_flags", admin, `{"text":"OLD-ONE and NEW-ONE"}`, http.StatusOK, &ingest)
	var flags struct {
		Flags []store.Item `json:"flags"`
	}
	request(routes, http.MethodGet, "/api/filter_flags", admin, "", http.StatusOK, &flags)
	if ingest.Queued != 1 || len(flags.Flags) != 1 || flags.Flags[0].Flag != "NEW-ONE" {
		t.Fatalf("manual ingest queued %d and stored %+v", ingest.Queued, flags.Flags)
	}

	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	base.FlagFormat, base.SubmitLimit = "FALLBACK-[A-Z]+", 12
	_, reopened := open(base)
	checkCurrent(reopened, updated)
	checkLegacy(reopened, bearer, updated)
	checkChecker(reopened)
}

func TestSubmissionWorkerUsesLiveSettings(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "farm.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{
		config: Config{
			Password: "worker-test-password", FlagFormat: "FLAG-[A-Z]+", FlagLifetime: 300,
			SubmitPeriod: 60, SubmitLimit: 10, SubmitTimeout: 5, RetryDelay: 1,
		},
		store: db, wake: make(chan struct{}, 1), settingsChanged: make(chan struct{}, 1),
	}
	if err := s.loadSubmissionSettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	batches := make(chan []string, 4)
	s.checker.submit = func(_ context.Context, flags []string) ([]checker.Result, error) {
		batches <- flags
		results := make([]checker.Result, 0, len(flags))
		for _, flag := range flags {
			results = append(results, checker.Result{Flag: flag, Status: checker.Accepted})
		}
		return results, nil
	}
	add := func(received time.Time, flags ...store.Item) {
		t.Helper()
		if err := db.Add(t.Context(), flags, received.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	add(time.Now(), store.Item{Flag: "FLAG-FIRST"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.work(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitBatch := func() []string {
		t.Helper()
		select {
		case batch := <-batches:
			return batch
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not submit a batch")
			return nil
		}
	}
	if first := waitBatch(); len(first) != 1 || first[0] != "FLAG-FIRST" {
		t.Fatalf("first batch = %v", first)
	}

	add(time.Now().Add(-time.Minute), store.Item{Flag: "FLAG-STALE"})
	add(time.Now(), store.Item{Flag: "FLAG-FRESH"}, store.Item{Flag: "FLAG-SECOND"})
	body := strings.NewReader(`{"flag_format":"FLAG-[A-Z]+","flag_lifetime_seconds":15,"submit_period_seconds":1,"submit_flag_limit":1}`)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/submission_settings", body)
	request.Header.Set("Authorization", s.config.Password)
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", response.Code, response.Body)
	}
	if next := waitBatch(); len(next) != 1 || (next[0] != "FLAG-FRESH" && next[0] != "FLAG-SECOND") {
		t.Fatalf("first batch after settings change = %v, want one fresh flag", next)
	}
	expired, _, err := db.List(t.Context(), store.Filter{Flag: "FLAG-STALE", Status: "SKIPPED"}, 1, 0)
	if err != nil || len(expired) != 1 {
		t.Fatalf("stale flag was not skipped: %+v, %v", expired, err)
	}
}
