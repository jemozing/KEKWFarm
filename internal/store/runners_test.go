package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerRegistryMigratesAndStoresOnlyTokenHash(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "farm.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.ExecContext(ctx, `
        CREATE TABLE checker_settings (id INTEGER PRIMARY KEY CHECK (id = 1), config BLOB NOT NULL);
        INSERT INTO checker_settings (id, config) VALUES (1, X'7B7D');`)
	if closeErr := old.Close(); err != nil || closeErr != nil {
		t.Fatalf("prepare existing database: %v, %v", err, closeErr)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if config, err := s.LoadCheckerConfig(ctx); err != nil || string(config) != "{}" {
		t.Fatalf("existing checker config changed: %q, %v", config, err)
	}
	if runners, err := s.ListRunners(ctx); err != nil || runners == nil || len(runners) != 0 {
		t.Fatalf("empty runner registry: %+v, %v", runners, err)
	}

	runner, token, err := s.CreateRunner(ctx, "runner one")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.ID) != 32 || !strings.HasPrefix(token, "kr_") || len(token) != 67 || runner.Name != "runner one" || runner.CreatedAt.IsZero() {
		t.Fatalf("invalid created runner: %+v", runner)
	}
	var stored []byte
	var dataType string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash, typeof(token_hash) FROM runners WHERE id = ?`, runner.ID).Scan(&stored, &dataType); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(token))
	if dataType != "blob" || !bytes.Equal(stored, wantHash[:]) {
		t.Fatal("runner token was not stored as a SHA-256 digest")
	}
	if id, err := s.RunnerID(ctx, token); err != nil || id != runner.ID {
		t.Fatalf("resolve new token: %q, %v", id, err)
	}
	if runners, err := s.ListRunners(ctx); err != nil || len(runners) != 1 || runners[0] != runner {
		t.Fatalf("list new runner: %+v, %v", runners, err)
	}
	data, err := json.Marshal(runner)
	if err != nil || bytes.Contains(data, []byte(token)) || !bytes.Contains(data, []byte(`"last_seen":null`)) || !bytes.Contains(data, []byte(`"address":""`)) ||
		!bytes.Contains(data, []byte(`"created_at":"`+runner.CreatedAt.Format(time.RFC3339Nano)+`"`)) {
		t.Fatal("runner JSON should expose timestamps without the token")
	}
	other, otherToken, err := s.CreateRunner(ctx, "runner one")
	if err != nil || other.ID == runner.ID || otherToken == token {
		t.Fatalf("separate runners must have distinct credentials: %v", err)
	}
}

func TestRunnerHeartbeatAndRevocationAcrossConnections(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	admin, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runner, token, err := s.CreateRunner(ctx, "runner one")
	if err != nil {
		t.Fatal(err)
	}
	jobs := RunnerJobs{Scheduled: 1, Running: 2, Stopping: 3, Completed: 4, Failed: 5, Stopped: 6}
	const address = "192.0.2.20"
	if err := s.UpdateRunner(ctx, runner.ID, "1.2.3", address, jobs); err != nil {
		t.Fatal(err)
	}
	entries, err := admin.ListRunners(ctx)
	if err != nil || len(entries) != 1 || entries[0].LastSeen == nil || entries[0].LastSeen.Before(runner.CreatedAt) || entries[0].Version != "1.2.3" || entries[0].Address != address || entries[0].Jobs != jobs {
		t.Fatalf("heartbeat did not update all runner fields: %+v, %v", entries, err)
	}
	if err := admin.RevokeRunner(ctx, runner.ID); err != nil {
		t.Fatal(err)
	}
	if err := admin.RevokeRunner(ctx, runner.ID); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	for _, rejected := range []string{token, "", "kr_" + strings.Repeat("0", 64)} {
		if id, err := s.RunnerID(ctx, rejected); id != "" || !errors.Is(err, ErrRunnerNotFound) {
			t.Fatalf("unexpected token lookup result: %q, %v", id, err)
		}
	}
	if err := s.UpdateRunner(ctx, runner.ID, "2.0", "192.0.2.21", RunnerJobs{}); !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("revoked runner heartbeat was accepted: %v", err)
	}
	if err := admin.RevokeRunner(ctx, "missing"); !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("unknown runner revoke: %v", err)
	}
	if err := s.UpdateRunner(ctx, "missing", "2.0", "192.0.2.21", RunnerJobs{}); !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("unknown runner heartbeat: %v", err)
	}
	entries, err = admin.ListRunners(ctx)
	if err != nil || len(entries) != 1 || !entries[0].Revoked || entries[0].LastSeen == nil || entries[0].Version != "1.2.3" || entries[0].Address != address || entries[0].Jobs != jobs {
		t.Fatalf("revocation must preserve the last heartbeat: %+v, %v", entries, err)
	}
}

func TestRunnerAddressMigratesExistingHeartbeat(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "farm.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	if _, err := old.ExecContext(ctx, `
        CREATE TABLE runners (
            id TEXT PRIMARY KEY NOT NULL,
            name TEXT NOT NULL,
            token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
            created_at INTEGER NOT NULL,
            last_seen INTEGER,
            revoked INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
            version TEXT NOT NULL DEFAULT '',
            jobs_scheduled INTEGER NOT NULL DEFAULT 0,
            jobs_running INTEGER NOT NULL DEFAULT 0,
            jobs_stopping INTEGER NOT NULL DEFAULT 0,
            jobs_completed INTEGER NOT NULL DEFAULT 0,
            jobs_failed INTEGER NOT NULL DEFAULT 0,
            jobs_stopped INTEGER NOT NULL DEFAULT 0
        )`); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	token := "kr_" + strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(token))
	if _, err := old.ExecContext(ctx, `
        INSERT INTO runners (id, name, token_hash, created_at, last_seen, version, jobs_running)
        VALUES (?, 'existing runner', ?, 1000, 2000, '1.0', 3)`, id, hash[:]); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.ListRunners(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list migrated runner: %+v, %v", entries, err)
	}
	legacy := entries[0]
	if legacy.ID != id || legacy.Name != "existing runner" || legacy.CreatedAt.UnixMilli() != 1000 ||
		legacy.LastSeen == nil || legacy.LastSeen.UnixMilli() != 2000 || legacy.Address != "" || legacy.Version != "1.0" || legacy.Jobs != (RunnerJobs{Running: 3}) {
		t.Fatalf("migration changed existing runner metadata: %+v", legacy)
	}
	if got, err := s.RunnerID(ctx, token); err != nil || got != id {
		t.Fatalf("migration changed token lookup: %q, %v", got, err)
	}
	const address = "2001:db8::42"
	if err := s.UpdateRunner(ctx, id, "1.1", address, RunnerJobs{Completed: 3}); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	entries, err = other.ListRunners(ctx)
	if err != nil || len(entries) != 1 || entries[0].Address != address || entries[0].Version != "1.1" || entries[0].Jobs != (RunnerJobs{Completed: 3}) {
		t.Fatalf("new address did not persist across reopen: %+v, %v", entries, err)
	}
}
