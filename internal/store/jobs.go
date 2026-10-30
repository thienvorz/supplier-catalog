package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JobStatus is the lifecycle state of an update.
type JobStatus string

const (
	JobRunning     JobStatus = "running"
	JobInterrupted JobStatus = "interrupted"
	JobCompleted   JobStatus = "completed"
	JobCancelled   JobStatus = "cancelled"
)

type Counters struct {
	Received   int64 `json:"received"`
	Created    int64 `json:"created"`
	Updated    int64 `json:"updated"`
	Relisted   int64 `json:"relisted"`
	Withdrawn  int64 `json:"withdrawn"`
	Unchanged  int64 `json:"unchanged"`
	Duplicates int64 `json:"duplicates"`
	Stale      int64 `json:"stale"`
	Invalid    int64 `json:"invalid"`
	Conflicts  int64 `json:"conflicts"`
}

// Accounted is the sum of all outcome counters; it must equal Received.
func (c Counters) Accounted() int64 {
	return c.Created + c.Updated + c.Relisted + c.Withdrawn + c.Unchanged +
		c.Duplicates + c.Stale + c.Invalid + c.Conflicts
}

// Job is one run of an update from the supplier.
type Job struct {
	ID           int64      `json:"id"`
	Feed         string     `json:"feed"`
	Status       JobStatus  `json:"status"`
	Cursor       string     `json:"next_cursor"`
	BatchesDone  int64      `json:"batches_done"`
	SupplierDone bool       `json:"supplier_done"`
	Resumes      int64      `json:"resumes"`
	Counters     Counters   `json:"counters"`
	LastError    string     `json:"last_error"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

const jobColumns = `id, feed, status, next_cursor, batches_done, supplier_done, resumes,
	received, created, updated, relisted, withdrawn, unchanged, duplicates, stale, invalid, conflicts,
	last_error, created_at, updated_at, finished_at`

func scanJob(sc scanner) (Job, error) {
	var (
		j               Job
		status          string
		done            int
		createdAt, upAt string
		finishedAt      sql.NullString
		c               = &j.Counters
	)
	err := sc.Scan(&j.ID, &j.Feed, &status, &j.Cursor, &j.BatchesDone, &done, &j.Resumes,
		&c.Received, &c.Created, &c.Updated, &c.Relisted, &c.Withdrawn, &c.Unchanged,
		&c.Duplicates, &c.Stale, &c.Invalid, &c.Conflicts,
		&j.LastError, &createdAt, &upAt, &finishedAt)
	if err != nil {
		return Job{}, err
	}
	j.Status = JobStatus(status)
	j.SupplierDone = done == 1
	if j.CreatedAt, err = parseTime(createdAt); err != nil {
		return Job{}, err
	}
	if j.UpdatedAt, err = parseTime(upAt); err != nil {
		return Job{}, err
	}
	if j.FinishedAt, err = parseNullTime(finishedAt); err != nil {
		return Job{}, err
	}
	return j, nil
}


func (s *Store) CreateJob(ctx context.Context, feed string, now time.Time) (Job, error) {
	var job Job
	err := s.WithTx(ctx, func(tx *Tx) error {
		var openID int64
		err := tx.tx.QueryRowContext(ctx,
			`SELECT id FROM update_jobs WHERE feed = ? AND status IN ('running','interrupted')`, feed).Scan(&openID)
		if err == nil {
			return &OpenJobError{JobID: openID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.tx.ExecContext(ctx,
			`INSERT INTO update_jobs (feed, status, created_at, updated_at) VALUES (?, 'running', ?, ?)`,
			feed, timeText(now), timeText(now))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		job, err = scanJob(tx.tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM update_jobs WHERE id = ?`, id))
		return err
	})
	if err != nil && isUniqueViolation(err) {
		return Job{}, ErrOpenJobExists
	}
	return job, err
}

// GetJob returns one job or ErrNotFound.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM update_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("update %d: %w", id, ErrNotFound)
	}
	return j, err
}

// ListJobs returns jobs, newest first. status may be empty for all.
func (s *Store) ListJobs(ctx context.Context, status string, limit, offset int) ([]Job, error) {
	limit, offset = clampPage(limit, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM update_jobs WHERE (? = '' OR status = ?) ORDER BY id DESC LIMIT ? OFFSET ?`,
		status, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}


func (s *Store) ClaimInterrupted(ctx context.Context, id int64, now time.Time) (Job, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE update_jobs SET status = 'running', last_error = '', resumes = resumes + 1, updated_at = ?
		 WHERE id = ? AND status = 'interrupted'`, timeText(now), id)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Job{}, s.explainRefusal(ctx, id, "resume")
	}
	return s.GetJob(ctx, id)
}

// MarkInterrupted records that a running job stopped before finishing.
// It does nothing if the job is no longer running.
func (s *Store) MarkInterrupted(ctx context.Context, id int64, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE update_jobs SET status = 'interrupted', last_error = ?, updated_at = ?
		 WHERE id = ? AND status = 'running'`, reason, timeText(now), id)
	return err
}


func (s *Store) RecoverRunning(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE update_jobs SET status = 'interrupted', updated_at = ?,
		   last_error = 'the application stopped while this update was running; resume it to continue'
		 WHERE status = 'running'`, timeText(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}


func (s *Store) Cancel(ctx context.Context, id int64, now time.Time) (Job, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE update_jobs SET status = 'cancelled', finished_at = ?, updated_at = ?
		 WHERE id = ? AND status = 'interrupted'`, timeText(now), timeText(now), id)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Job{}, s.explainRefusal(ctx, id, "cancel")
	}
	return s.GetJob(ctx, id)
}

// explainRefusal turns "0 rows updated" into a precise error.
func (s *Store) explainRefusal(ctx context.Context, id int64, action string) error {
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: cannot %s update %d because it is %s", ErrInvalidState, action, id, j.Status)
}


func (s *Store) JobSummary(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM update_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
