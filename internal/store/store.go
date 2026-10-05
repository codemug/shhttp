// Package store persists keys and session metadata in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/codemug/shhttp/v2/pkg/api"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// migrations are applied in order; PRAGMA user_version records how many have
// run. Never edit an existing entry, only append.
var migrations = []string{
	`CREATE TABLE keys (
		id                TEXT PRIMARY KEY,
		name              TEXT NOT NULL,
		scopes            TEXT NOT NULL,
		policy            TEXT NOT NULL,
		secret_hash       TEXT NOT NULL,
		prev_secret_hash  TEXT NOT NULL DEFAULT '',
		prev_valid_until  INTEGER,
		created_at        INTEGER NOT NULL,
		expires_at        INTEGER,
		last_used_at      INTEGER,
		revoked_at        INTEGER
	);
	CREATE TABLE sessions (
		id          TEXT PRIMARY KEY,
		key_id      TEXT NOT NULL,
		spec        TEXT NOT NULL,
		state       TEXT NOT NULL,
		pid         INTEGER NOT NULL DEFAULT 0,
		exit_code   INTEGER,
		signal      TEXT NOT NULL DEFAULT '',
		error       TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		started_at  INTEGER,
		ended_at    INTEGER,
		expires_at  INTEGER
	);
	CREATE INDEX sessions_key ON sessions (key_id, id);
	CREATE INDEX sessions_state ON sessions (state);
	CREATE INDEX sessions_expires ON sessions (expires_at);`,
	`CREATE TABLE jobs (
		id          TEXT PRIMARY KEY,
		key_id      TEXT NOT NULL,
		spec        TEXT NOT NULL,
		queue       TEXT NOT NULL DEFAULT '',
		state       TEXT NOT NULL,
		steps       TEXT NOT NULL,
		error       TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		started_at  INTEGER,
		ended_at    INTEGER,
		expires_at  INTEGER
	);
	CREATE INDEX jobs_key ON jobs (key_id, id);
	CREATE INDEX jobs_state ON jobs (state, queue, id);
	CREATE INDEX jobs_expires ON jobs (expires_at);
	CREATE TABLE queues (
		name        TEXT PRIMARY KEY,
		concurrency INTEGER NOT NULL
	);
	INSERT INTO queues (name, concurrency) VALUES ('default', 1);
	CREATE TABLE templates (
		name        TEXT PRIMARY KEY,
		spec        TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		updated_by  TEXT NOT NULL
	);`,
}

// Store is the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writes, which avoids SQLITE_BUSY and is fast
	// enough for metadata.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this server supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Times are stored as Unix nanoseconds; NULL means unset.

func toNanos(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func fromNanos(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(0, v.Int64).UTC()
	return &t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// KeyRecord is a key with its secret hashes.
type KeyRecord struct {
	api.Key
	SecretHash     string
	PrevSecretHash string
}

const keyColumns = `id, name, scopes, policy, secret_hash, prev_secret_hash, prev_valid_until,
	created_at, expires_at, last_used_at, revoked_at`

// InsertKey stores a new key.
func (s *Store) InsertKey(ctx context.Context, k KeyRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO keys (`+keyColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID, k.Name, mustJSON(k.Scopes), mustJSON(k.Policy), k.SecretHash, k.PrevSecretHash,
		toNanos(k.PreviousSecretValidUntil), k.CreatedAt.UnixNano(), toNanos(k.ExpiresAt),
		toNanos(k.LastUsedAt), toNanos(k.RevokedAt))
	return err
}

// UpdateKey overwrites every field of an existing key.
func (s *Store) UpdateKey(ctx context.Context, k KeyRecord) error {
	res, err := s.db.ExecContext(ctx, `UPDATE keys SET name=?, scopes=?, policy=?, secret_hash=?,
		prev_secret_hash=?, prev_valid_until=?, expires_at=?, last_used_at=?, revoked_at=? WHERE id=?`,
		k.Name, mustJSON(k.Scopes), mustJSON(k.Policy), k.SecretHash, k.PrevSecretHash,
		toNanos(k.PreviousSecretValidUntil), toNanos(k.ExpiresAt), toNanos(k.LastUsedAt),
		toNanos(k.RevokedAt), k.ID)
	return checkAffected(res, err)
}

// TouchKey records that a key was used at t.
func (s *Store) TouchKey(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE keys SET last_used_at=? WHERE id=?`, t.UnixNano(), id)
	return err
}

// GetKey returns one key.
func (s *Store) GetKey(ctx context.Context, id string) (KeyRecord, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM keys WHERE id=?`, id))
}

// ListKeys returns all keys, oldest first.
func (s *Store) ListKeys(ctx context.Context) ([]KeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRecord
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanKey(r scanner) (KeyRecord, error) {
	var k KeyRecord
	var scopes, policy string
	var created int64
	var prevUntil, expires, lastUsed, revoked sql.NullInt64
	err := r.Scan(&k.ID, &k.Name, &scopes, &policy, &k.SecretHash, &k.PrevSecretHash, &prevUntil,
		&created, &expires, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal([]byte(scopes), &k.Scopes); err != nil {
		return k, err
	}
	if err := json.Unmarshal([]byte(policy), &k.Policy); err != nil {
		return k, err
	}
	k.CreatedAt = time.Unix(0, created).UTC()
	k.PreviousSecretValidUntil = fromNanos(prevUntil)
	k.ExpiresAt = fromNanos(expires)
	k.LastUsedAt = fromNanos(lastUsed)
	k.RevokedAt = fromNanos(revoked)
	return k, nil
}

const sessionColumns = `id, key_id, spec, state, pid, exit_code, signal, error,
	created_at, started_at, ended_at, expires_at`

// InsertSession stores a new session.
func (s *Store) InsertSession(ctx context.Context, x api.Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		x.ID, x.KeyID, mustJSON(x.Spec), x.State, x.PID, intOrNil(x.ExitCode), x.Signal, x.Error,
		x.CreatedAt.UnixNano(), toNanos(x.StartedAt), toNanos(x.EndedAt), toNanos(x.ExpiresAt))
	return err
}

// UpdateSession overwrites the mutable fields of a session.
func (s *Store) UpdateSession(ctx context.Context, x api.Session) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=?, pid=?, exit_code=?, signal=?, error=?,
		started_at=?, ended_at=?, expires_at=? WHERE id=?`,
		x.State, x.PID, intOrNil(x.ExitCode), x.Signal, x.Error, toNanos(x.StartedAt),
		toNanos(x.EndedAt), toNanos(x.ExpiresAt), x.ID)
	return checkAffected(res, err)
}

// GetSession returns one session.
func (s *Store) GetSession(ctx context.Context, id string) (api.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id=?`, id))
}

// SessionFilter selects sessions to list.
type SessionFilter struct {
	KeyID  string // empty means all keys
	State  api.SessionState
	Labels map[string]string
	// Before returns only sessions with an id lower than this (cursor).
	Before string
	Limit  int
}

// ListSessions returns sessions newest first.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]api.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE 1=1`
	var args []any
	if f.KeyID != "" {
		q += ` AND key_id = ?`
		args = append(args, f.KeyID)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	for k, v := range f.Labels {
		q += ` AND json_extract(spec, '$.labels.' || json_quote(?)) = ?`
		args = append(args, k, v)
	}
	if f.Before != "" {
		q += ` AND id < ?`
		args = append(args, f.Before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// MarkUnfinishedLost marks every pending or running session as lost and
// returns their ids. It is called at startup: those processes died with the
// previous server.
func (s *Store) MarkUnfinishedLost(ctx context.Context, now time.Time, retention time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE state IN (?, ?)`, api.StatePending, api.StateRunning)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET state=?, error=?, ended_at=?, expires_at=? WHERE state IN (?, ?)`,
		api.StateLost, "the server stopped while the session was running", now.UnixNano(),
		now.Add(retention).UnixNano(), api.StatePending, api.StateRunning)
	return ids, err
}

// ExpiredSessions returns the ids of finished sessions whose retention ended
// before now.
func (s *Store) ExpiredSessions(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE expires_at IS NOT NULL AND expires_at < ?`, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteSession removes a session record.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return checkAffected(res, err)
}

func scanSession(r scanner) (api.Session, error) {
	var x api.Session
	var spec string
	var created int64
	var exit, started, ended, expires sql.NullInt64
	err := r.Scan(&x.ID, &x.KeyID, &spec, &x.State, &x.PID, &exit, &x.Signal, &x.Error,
		&created, &started, &ended, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	if err != nil {
		return x, err
	}
	if err := json.Unmarshal([]byte(spec), &x.Spec); err != nil {
		return x, err
	}
	if exit.Valid {
		c := int(exit.Int64)
		x.ExitCode = &c
	}
	x.CreatedAt = time.Unix(0, created).UTC()
	x.StartedAt = fromNanos(started)
	x.EndedAt = fromNanos(ended)
	x.ExpiresAt = fromNanos(expires)
	if x.StartedAt != nil && x.EndedAt != nil {
		d := x.EndedAt.Sub(*x.StartedAt).Milliseconds()
		x.DurationMS = &d
	}
	return x, nil
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
