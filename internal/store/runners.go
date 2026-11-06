package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const runnerTokenPrefix = "kr_"

var ErrRunnerNotFound = errors.New("runner not found")

type Runner struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen"`
	Revoked   bool       `json:"revoked"`
	Version   string     `json:"version"`
	Address   string     `json:"address"`
	Jobs      RunnerJobs `json:"jobs"`
	Online    bool       `json:"online"`
}

type RunnerJobs struct {
	Scheduled int `json:"scheduled"`
	Running   int `json:"running"`
	Stopping  int `json:"stopping"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Stopped   int `json:"stopped"`
}

func migrateRunnerAddress(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Reserve the write lock before reading the schema so concurrent openers
	// cannot both attempt to add the column.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pragma_table_info('runners') WHERE name = 'address')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE runners ADD COLUMN address TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func (s *Store) CreateRunner(ctx context.Context, name string) (Runner, string, error) {
	var secret [16 + 32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Runner{}, "", err
	}
	id := hex.EncodeToString(secret[:16])
	token := runnerTokenPrefix + hex.EncodeToString(secret[16:])
	hash := sha256.Sum256([]byte(token))
	created := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runners (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`, id, name, hash[:], created); err != nil {
		return Runner{}, "", err
	}
	return Runner{ID: id, Name: name, CreatedAt: time.UnixMilli(created).UTC()}, token, nil
}

func (s *Store) ListRunners(ctx context.Context) ([]Runner, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, last_seen, revoked, version, address,
               jobs_scheduled, jobs_running, jobs_stopping, jobs_completed, jobs_failed, jobs_stopped
        FROM runners ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runners := []Runner{}
	for rows.Next() {
		var runner Runner
		var created int64
		var seen sql.NullInt64
		jobs := &runner.Jobs
		if err := rows.Scan(&runner.ID, &runner.Name, &created, &seen, &runner.Revoked, &runner.Version, &runner.Address,
			&jobs.Scheduled, &jobs.Running, &jobs.Stopping, &jobs.Completed, &jobs.Failed, &jobs.Stopped); err != nil {
			return nil, err
		}
		runner.CreatedAt = time.UnixMilli(created).UTC()
		if seen.Valid {
			at := time.UnixMilli(seen.Int64).UTC()
			runner.LastSeen = &at
		}
		runners = append(runners, runner)
	}
	return runners, rows.Err()
}

func (s *Store) RunnerID(ctx context.Context, token string) (string, error) {
	if len(token) != len(runnerTokenPrefix)+hex.EncodedLen(32) || !strings.HasPrefix(token, runnerTokenPrefix) {
		return "", ErrRunnerNotFound
	}
	hash := sha256.Sum256([]byte(token))
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM runners WHERE token_hash = ? AND revoked = 0`, hash[:]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRunnerNotFound
	}
	return id, err
}

func (s *Store) UpdateRunner(ctx context.Context, id, version, address string, jobs RunnerJobs) error {
	return runnerWrite(s.db.ExecContext(ctx, `
        UPDATE runners SET last_seen = max(coalesce(last_seen, 0), ?), version = ?, address = ?,
            jobs_scheduled = ?, jobs_running = ?, jobs_stopping = ?,
            jobs_completed = ?, jobs_failed = ?, jobs_stopped = ?
        WHERE id = ? AND revoked = 0`, time.Now().UnixMilli(), version, address,
		jobs.Scheduled, jobs.Running, jobs.Stopping, jobs.Completed, jobs.Failed, jobs.Stopped, id))
}

func (s *Store) RevokeRunner(ctx context.Context, id string) error {
	return runnerWrite(s.db.ExecContext(ctx, `UPDATE runners SET revoked = 1 WHERE id = ?`, id))
}

func runnerWrite(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed == 0 {
		return ErrRunnerNotFound
	}
	return err
}
