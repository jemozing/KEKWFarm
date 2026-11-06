//go:build unix

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"duckfarm/internal/farm"
	"duckfarm/internal/runner"
	"duckfarm/internal/store"
)

func TestRunnerToChecker(t *testing.T) {
	const password = "local-test-password"
	check := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var flags []string
		if err := json.NewDecoder(r.Body).Decode(&flags); err != nil ||
			r.Method != http.MethodPut || r.Header.Get("X-Team-Token") != "test-token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		results := make([]map[string]string, 0, len(flags))
		for _, flag := range flags {
			results = append(results, map[string]string{"flag": flag, "msg": "flag accepted"})
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer check.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "farm.json")
	database := filepath.Join(dir, "flags.sqlite")
	config, err := json.Marshal(map[string]any{
		"listen": "127.0.0.1:0", "database": database, "password": password,
		"teams": map[string]string{"Test team": "demo"}, "flag_format": "FLAG-[A-Z]+",
		"submit_period_seconds": 1, "retry_delay_seconds": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVER_PASSWORD", "")
	cfg, err := farm.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- farm.Run(ctx, cfg, func(address net.Addr) { ready <- address }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var base string
	select {
	case address := <-ready:
		base = "http://" + address.String()
	case <-ctx.Done():
		t.Fatal("server did not start")
	}

	script := filepath.Join(dir, "example.sh")
	// Repeat the first flag and write the second one to stderr without a trailing newline.
	if err := os.WriteFile(script, []byte(`
test "$1" = demo || exit 2
printf 'FLAG-ONE\nFLAG-ONE\n'
printf 'FLAG-TWO' >&2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(ctx, []string{script, "--interpreter", "/bin/sh", "--endless"},
		runner.Defaults{URL: base, Password: password}); err != nil {
		t.Fatal(err)
	}
	request := func(method, route, auth, data string) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, base+route, strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", auth)
		r.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	expectStatus := func(method, route, auth, data string, want int) {
		t.Helper()
		res := request(method, route, auth, data)
		defer res.Body.Close()
		if res.StatusCode != want {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("%s returned %d, want %d: %s", route, res.StatusCode, want, body)
		}
	}
	expectStatus("GET", "/", "", "", http.StatusOK)
	expectStatus("GET", "/api/filter_flags", "", "", http.StatusForbidden)
	expectStatus("POST", "/api/login", "", `{"password":"local-test-password"}`, http.StatusNoContent)
	expectStatus("POST", "/api/post_flags", password, `{"text":"paste FLAG-MANUAL"}`, http.StatusOK)
	checkerConfig, err := json.Marshal(map[string]string{"protocol": "forcad_http", "url": check.URL, "token": "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus("PUT", "/api/checker_config", "", string(checkerConfig), http.StatusForbidden)
	expectStatus("PUT", "/api/checker_config", password, `{"protocol":"forcad_http"}`, http.StatusBadRequest)
	expectStatus("PUT", "/api/checker_config", password, string(checkerConfig), http.StatusOK)

	for {
		res := request("GET", "/api/filter_flags", password, "")
		var list struct {
			Flags []store.Item
			Total int
		}
		err := json.NewDecoder(res.Body).Decode(&list)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("list flags: status %d, %v", res.StatusCode, err)
		}
		if list.Total == 3 {
			accepted := make(map[string]string)
			for _, item := range list.Flags {
				if item.Status == "ACCEPTED" && item.Response == "flag accepted" {
					accepted[item.Flag] = item.Team
				}
			}
			if accepted["FLAG-ONE"] == "Test team" && accepted["FLAG-TWO"] == "Test team" &&
				accepted["FLAG-MANUAL"] == "*" {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("did not receive all verdicts: %#v", list)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
