package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS flags (
    flag TEXT PRIMARY KEY NOT NULL,
    sploit TEXT NOT NULL,
    team TEXT NOT NULL,
    time INTEGER NOT NULL,
    status TEXT NOT NULL,
    checksystem_response TEXT NOT NULL DEFAULT '',
    available_at INTEGER NOT NULL,
    lease_until INTEGER NOT NULL DEFAULT 0,
    attempted INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS flags_sploit_idx ON flags(sploit);
CREATE INDEX IF NOT EXISTS flags_team_idx ON flags(team);
CREATE INDEX IF NOT EXISTS flags_status_time_idx ON flags(status, time, flag);
CREATE INDEX IF NOT EXISTS flags_time_idx ON flags(time, flag);
CREATE INDEX IF NOT EXISTS flags_ready_idx ON flags(attempted, available_at, time, flag, lease_until) WHERE status = 'QUEUED';
CREATE TABLE IF NOT EXISTS checker_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    config BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS submission_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    config BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS runners (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at INTEGER NOT NULL,
    last_seen INTEGER,
    revoked INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    version TEXT NOT NULL DEFAULT '',
    address TEXT NOT NULL DEFAULT '',
    jobs_scheduled INTEGER NOT NULL DEFAULT 0,
    jobs_running INTEGER NOT NULL DEFAULT 0,
    jobs_stopping INTEGER NOT NULL DEFAULT 0,
    jobs_completed INTEGER NOT NULL DEFAULT 0,
    jobs_failed INTEGER NOT NULL DEFAULT 0,
    jobs_stopped INTEGER NOT NULL DEFAULT 0
);
`

type Store struct {
	db *sql.DB
}

type Item struct {
	Flag     string `json:"flag"`
	Sploit   string `json:"sploit"`
	Team     string `json:"team"`
	Time     int64  `json:"time"`
	Status   string `json:"status"`
	Response string `json:"checksystem_response"`
}

type Filter struct {
	Sploit, Team, Status, Flag, Response string
	Since, Until                         *int64
}

type Count struct {
	Sploit, Team, Key string
	Total             int
}

func init() {
	// SQLite's built-in lower() handles ASCII only. Responses can contain Unicode.
	sqlite.MustRegisterDeterministicScalarFunction("duckfarm_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, _ := args[0].(string)
		return strings.ToLower(value), nil
	})
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("SQLite path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open SQLite file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`+schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize SQLite store: %w", err)
	}
	if err := migrateRunnerAddress(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate runner address: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) LoadCheckerConfig(ctx context.Context) ([]byte, error) {
	var config []byte
	err := s.db.QueryRowContext(ctx, `SELECT config FROM checker_settings WHERE id = 1`).Scan(&config)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return config, err
}

func (s *Store) SaveCheckerConfig(ctx context.Context, config []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO checker_settings (id, config) VALUES (1, ?)
        ON CONFLICT(id) DO UPDATE SET config = excluded.config`, config)
	return err
}

func (s *Store) LoadSubmissionSettings(ctx context.Context) ([]byte, error) {
	var config []byte
	err := s.db.QueryRowContext(ctx, `SELECT config FROM submission_settings WHERE id = 1`).Scan(&config)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return config, err
}

func (s *Store) SaveSubmissionSettings(ctx context.Context, config []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO submission_settings (id, config) VALUES (1, ?)
        ON CONFLICT(id) DO UPDATE SET config = excluded.config`, config)
	return err
}

func (s *Store) Add(ctx context.Context, items []Item, now int64) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO flags (flag, sploit, team, time, status, available_at)
        VALUES (?, ?, ?, ?, 'QUEUED', ?)
        ON CONFLICT(flag) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, item := range items {
		if _, err := stmt.ExecContext(ctx, item.Flag, item.Sploit, item.Team, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (f Filter) where() (string, []any) {
	var conditions []string
	var args []any
	for _, field := range []struct {
		condition, value string
	}{
		{"sploit = ?", f.Sploit},
		{"team = ?", f.Team},
		{"status = ?", f.Status},
		{"instr(duckfarm_lower(flag), ?) > 0", strings.ToLower(f.Flag)},
		{"instr(duckfarm_lower(checksystem_response), ?) > 0", strings.ToLower(f.Response)},
	} {
		if field.value != "" {
			conditions = append(conditions, field.condition)
			args = append(args, field.value)
		}
	}
	if f.Since != nil {
		conditions = append(conditions, "time >= ?")
		args = append(args, *f.Since)
	}
	if f.Until != nil {
		conditions = append(conditions, "time <= ?")
		args = append(args, *f.Until)
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func (s *Store) List(ctx context.Context, f Filter, limit, offset int) ([]Item, int, error) {
	if limit <= 0 || offset < 0 {
		return nil, 0, errors.New("invalid pagination")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	where, args := f.where()
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM flags`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `
        SELECT flag, sploit, team, time, status, checksystem_response FROM flags`+where+`
        ORDER BY time DESC, flag DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.Flag, &item.Sploit, &item.Team, &item.Time, &item.Status, &item.Response); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Store) Distinct(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT DISTINCT 'sploit', sploit FROM flags
        UNION ALL SELECT DISTINCT 'status', status FROM flags
        UNION ALL SELECT DISTINCT 'team', team FROM flags
        ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string][]string{"sploit": {}, "status": {}, "team": {}}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = append(values[key], value)
	}
	return values, rows.Err()
}

func (s *Store) Summary(ctx context.Context, f Filter, byResponse bool) ([]Count, error) {
	key := "status"
	if byResponse {
		key = "substr(checksystem_response, 1, 64)"
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT sploit, team, `+key+`, count(*) FROM flags`+where+`
        GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := []Count{}
	for rows.Next() {
		var count Count
		if err := rows.Scan(&count.Sploit, &count.Team, &count.Key, &count.Total); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

// Claim leases ready flags until the given Unix timestamp, preferring new flags to retries.
// The returned items carry the flag and its original arrival time.
func (s *Store) Claim(ctx context.Context, now, lease int64, limit int) ([]Item, error) {
	if lease <= now || limit <= 0 {
		return nil, errors.New("invalid flag lease or limit")
	}
	rows, err := s.db.QueryContext(ctx, `
        WITH eligible AS MATERIALIZED (
            SELECT flag FROM flags WHERE status = 'QUEUED' AND available_at <= ? AND lease_until <= ?
            ORDER BY attempted, available_at, time, flag LIMIT ?
        )
        UPDATE flags SET lease_until = ?
        WHERE flag IN (SELECT flag FROM eligible)
        RETURNING flag, time, attempted, available_at`, now, now, limit, lease)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		item        Item
		attempted   bool
		availableAt int64
	}
	var claimed []candidate
	for rows.Next() {
		var row candidate
		if err := rows.Scan(&row.item.Flag, &row.item.Time, &row.attempted, &row.availableAt); err != nil {
			return nil, err
		}
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// SQLite does not guarantee the order of RETURNING rows.
	sort.Slice(claimed, func(i, j int) bool {
		a, b := claimed[i], claimed[j]
		if a.attempted != b.attempted {
			return !a.attempted
		}
		if a.availableAt != b.availableAt {
			return a.availableAt < b.availableAt
		}
		if a.item.Time != b.item.Time {
			return a.item.Time < b.item.Time
		}
		return a.item.Flag < b.item.Flag
	})
	flags := make([]Item, 0, len(claimed))
	for _, row := range claimed {
		flags = append(flags, row.item)
	}
	return flags, nil
}

// Resolve records results only while the caller's lease still owns the flag.
func (s *Store) Resolve(ctx context.Context, items []Item, lease, retryAt int64) error {
	if len(items) == 0 {
		return nil
	}
	if lease <= 0 {
		return errors.New("invalid flag lease")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
        UPDATE flags SET status = ?, checksystem_response = ?, available_at = ?, lease_until = 0, attempted = 1
        WHERE flag = ? AND status = 'QUEUED' AND lease_until = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, item := range items {
		status := item.Status
		if status == "" {
			status = "QUEUED"
		}
		if _, err := stmt.ExecContext(ctx, status, item.Response, retryAt, item.Flag, lease); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Maintain expires old queued flags without a live lease and optionally purges old terminal flags.
func (s *Store) Maintain(ctx context.Context, now, expire, retain int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE flags SET status = 'SKIPPED' WHERE status = 'QUEUED' AND time < ? AND lease_until <= ?`, expire, now); err != nil {
		return err
	}
	if retain > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM flags WHERE status <> 'QUEUED' AND time < ?`, retain); err != nil {
			return err
		}
	}
	return tx.Commit()
}
