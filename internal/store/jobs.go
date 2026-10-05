package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

const jobColumns = `id, key_id, spec, queue, state, steps, error, created_at, started_at, ended_at, expires_at`

// InsertJob stores a new job.
func (s *Store) InsertJob(ctx context.Context, j api.Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.KeyID, mustJSON(j.Spec), j.Spec.Queue, j.State, mustJSON(j.Steps), j.Error,
		j.CreatedAt.UnixNano(), toNanos(j.StartedAt), toNanos(j.EndedAt), toNanos(j.ExpiresAt))
	return err
}

// UpdateJob overwrites the mutable fields of a job.
func (s *Store) UpdateJob(ctx context.Context, j api.Job) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, steps=?, error=?, started_at=?, ended_at=?, expires_at=? WHERE id=?`,
		j.State, mustJSON(j.Steps), j.Error, toNanos(j.StartedAt), toNanos(j.EndedAt), toNanos(j.ExpiresAt), j.ID)
	return checkAffected(res, err)
}

// GetJob returns one job.
func (s *Store) GetJob(ctx context.Context, id string) (api.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}

// JobFilter selects jobs to list.
type JobFilter struct {
	KeyID  string
	States []api.JobState
	Queue  *string
	Labels map[string]string
	Before string
	Limit  int
	// Oldest lists oldest first instead of newest first.
	Oldest bool
}

// ListJobs returns jobs matching f.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]api.Job, error) {
	q := `SELECT ` + jobColumns + ` FROM jobs WHERE 1=1`
	var args []any
	if f.KeyID != "" {
		q += ` AND key_id = ?`
		args = append(args, f.KeyID)
	}
	if len(f.States) > 0 {
		q += ` AND state IN (?` + strings.Repeat(",?", len(f.States)-1) + `)`
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	if f.Queue != nil {
		q += ` AND queue = ?`
		args = append(args, *f.Queue)
	}
	for k, v := range f.Labels {
		q += ` AND json_extract(spec, '$.labels.' || json_quote(?)) = ?`
		args = append(args, k, v)
	}
	order := "DESC"
	if f.Oldest {
		order = "ASC"
	}
	if f.Before != "" {
		if f.Oldest {
			q += ` AND id > ?`
		} else {
			q += ` AND id < ?`
		}
		args = append(args, f.Before)
	}
	q += ` ORDER BY id ` + order
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ExpiredJobs returns the ids of finished jobs whose retention ended before
// now.
func (s *Store) ExpiredJobs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE expires_at IS NOT NULL AND expires_at < ?`, now.UnixNano())
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

// DeleteJob removes a job record.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, id)
	return checkAffected(res, err)
}

func scanJob(r scanner) (api.Job, error) {
	var j api.Job
	var spec, steps, queue string
	var created int64
	var started, ended, expires sql.NullInt64
	err := r.Scan(&j.ID, &j.KeyID, &spec, &queue, &j.State, &steps, &j.Error, &created, &started, &ended, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(spec), &j.Spec); err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(steps), &j.Steps); err != nil {
		return j, err
	}
	j.CreatedAt = time.Unix(0, created).UTC()
	j.StartedAt = fromNanos(started)
	j.EndedAt = fromNanos(ended)
	j.ExpiresAt = fromNanos(expires)
	return j, nil
}

// PutQueue creates or updates a queue.
func (s *Store) PutQueue(ctx context.Context, name string, concurrency int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO queues (name, concurrency) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET concurrency = excluded.concurrency`, name, concurrency)
	return err
}

// Queues returns every queue's concurrency by name.
func (s *Store) Queues(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, concurrency FROM queues`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var c int
		if err := rows.Scan(&name, &c); err != nil {
			return nil, err
		}
		out[name] = c
	}
	return out, rows.Err()
}

// DeleteQueue removes a queue.
func (s *Store) DeleteQueue(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM queues WHERE name=?`, name)
	return checkAffected(res, err)
}

// PutTemplate creates or replaces a template and returns the stored copy.
func (s *Store) PutTemplate(ctx context.Context, name string, spec api.TemplateSpec, by string, now time.Time) (api.Template, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO templates (name, spec, created_at, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		name, mustJSON(spec), now.UnixNano(), now.UnixNano(), by)
	if err != nil {
		return api.Template{}, err
	}
	return s.GetTemplate(ctx, name)
}

// GetTemplate returns one template.
func (s *Store) GetTemplate(ctx context.Context, name string) (api.Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx, `SELECT name, spec, created_at, updated_at, updated_by FROM templates WHERE name=?`, name))
}

// ListTemplates returns every template by name.
func (s *Store) ListTemplates(ctx context.Context) ([]api.Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, spec, created_at, updated_at, updated_by FROM templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTemplate removes a template.
func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE name=?`, name)
	return checkAffected(res, err)
}

func scanTemplate(r scanner) (api.Template, error) {
	var t api.Template
	var spec string
	var created, updated int64
	err := r.Scan(&t.Name, &spec, &created, &updated, &t.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal([]byte(spec), &t.TemplateSpec); err != nil {
		return t, err
	}
	t.CreatedAt = time.Unix(0, created).UTC()
	t.UpdatedAt = time.Unix(0, updated).UTC()
	return t, nil
}
