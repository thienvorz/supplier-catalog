package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"catalogsync/internal/catalog"
)

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

const productColumns = `sku, name, description, category, price_cents, currency, status, version,
	withdrawn_at, created_at, updated_at`

func scanProduct(sc scanner) (catalog.Product, error) {
	var (
		p                   catalog.Product
		status              string
		withdrawn           sql.NullString
		createdAt, updateAt string
	)
	err := sc.Scan(&p.SKU, &p.Name, &p.Description, &p.Category, &p.PriceCents, &p.Currency,
		&status, &p.Version, &withdrawn, &createdAt, &updateAt)
	if err != nil {
		return catalog.Product{}, err
	}
	p.Status = catalog.Status(status)
	if p.WithdrawnAt, err = parseNullTime(withdrawn); err != nil {
		return catalog.Product{}, err
	}
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return catalog.Product{}, err
	}
	if p.UpdatedAt, err = parseTime(updateAt); err != nil {
		return catalog.Product{}, err
	}
	return p, nil
}

// GetProduct returns one product by (normalised) SKU or ErrNotFound.
func (s *Store) GetProduct(ctx context.Context, sku string) (catalog.Product, error) {
	p, err := scanProduct(s.db.QueryRowContext(ctx, `SELECT `+productColumns+` FROM products WHERE sku = ?`, sku))
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Product{}, fmt.Errorf("product %q: %w", sku, ErrNotFound)
	}
	return p, err
}

// ListProducts returns products ordered by SKU. status is "active", "withdrawn" or "" for all.
func (s *Store) ListProducts(ctx context.Context, status string, limit, offset int) ([]catalog.Product, error) {
	limit, offset = clampPage(limit, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+productColumns+` FROM products WHERE (? = '' OR status = ?) ORDER BY sku LIMIT ? OFFSET ?`,
		status, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []catalog.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProductCounts returns the number of products per status.
func (s *Store) ProductCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM products GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{"active": 0, "withdrawn": 0}
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

// ---------------------------------------------------------------------------
// Change history
// ---------------------------------------------------------------------------

// Change is one entry of the change history.
type Change struct {
	ID            int64           `json:"id"`
	JobID         int64           `json:"job_id"`
	BatchIndex    int64           `json:"batch_index"`
	SKU           string          `json:"sku"`
	Type          string          `json:"change_type"`
	VersionBefore *int64          `json:"version_before"`
	VersionAfter  int64           `json:"version_after"`
	ChangedFields []string        `json:"changed_fields"`
	EventID       string          `json:"event_id,omitempty"`
	Before        json.RawMessage `json:"before"`
	After         json.RawMessage `json:"after"`
	CreatedAt     time.Time       `json:"created_at"`
}

const changeColumns = `id, job_id, batch_index, sku, change_type, version_before, version_after,
	changed_fields, event_id, before_json, after_json, created_at`

func scanChange(sc scanner) (Change, error) {
	var (
		c         Change
		vb        sql.NullInt64
		fields    string
		before    sql.NullString
		after     string
		createdAt string
	)
	if err := sc.Scan(&c.ID, &c.JobID, &c.BatchIndex, &c.SKU, &c.Type, &vb, &c.VersionAfter,
		&fields, &c.EventID, &before, &after, &createdAt); err != nil {
		return Change{}, err
	}
	if vb.Valid {
		v := vb.Int64
		c.VersionBefore = &v
	}
	if err := json.Unmarshal([]byte(fields), &c.ChangedFields); err != nil {
		return Change{}, err
	}
	if before.Valid {
		c.Before = json.RawMessage(before.String)
	}
	c.After = json.RawMessage(after)
	var err error
	c.CreatedAt, err = parseTime(createdAt)
	return c, err
}

func (s *Store) queryChanges(ctx context.Context, where string, arg any, limit, offset int) ([]Change, error) {
	limit, offset = clampPage(limit, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+changeColumns+` FROM product_changes WHERE `+where+` ORDER BY id LIMIT ? OFFSET ?`, arg, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListJobChanges returns what one update changed, in the order it happened.
func (s *Store) ListJobChanges(ctx context.Context, jobID int64, limit, offset int) ([]Change, error) {
	return s.queryChanges(ctx, `job_id = ?`, jobID, limit, offset)
}

// ListProductChanges returns the full history of one product.
func (s *Store) ListProductChanges(ctx context.Context, sku string, limit, offset int) ([]Change, error) {
	return s.queryChanges(ctx, `sku = ?`, sku, limit, offset)
}

// --------------------------------------------------------------------------
type IssueKind string

const (
	IssueInvalidRecord   IssueKind = "invalid_record"
	IssueVersionConflict IssueKind = "version_conflict"
)

type Issue struct {
	ID             int64      `json:"id"`
	JobID          int64      `json:"job_id"`
	BatchIndex     int64      `json:"batch_index"`
	RecordIndex    int64      `json:"record_index"`
	Kind           IssueKind  `json:"kind"`
	SKU            string     `json:"sku,omitempty"`
	EventID        string     `json:"event_id,omitempty"`
	Message        string     `json:"message"`
	RawRecord      string     `json:"raw_record"`
	Occurrences    int64      `json:"occurrences"`
	Status         string     `json:"status"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

const issueColumns = `id, job_id, batch_index, record_index, kind, sku, event_id, message, raw_record,
	occurrences, status, resolution_note, created_at, resolved_at`

func scanIssue(sc scanner) (Issue, error) {
	var (
		i          Issue
		kind       string
		createdAt  string
		resolvedAt sql.NullString
	)
	if err := sc.Scan(&i.ID, &i.JobID, &i.BatchIndex, &i.RecordIndex, &kind, &i.SKU, &i.EventID,
		&i.Message, &i.RawRecord, &i.Occurrences, &i.Status, &i.ResolutionNote, &createdAt, &resolvedAt); err != nil {
		return Issue{}, err
	}
	i.Kind = IssueKind(kind)
	var err error
	if i.CreatedAt, err = parseTime(createdAt); err != nil {
		return Issue{}, err
	}
	i.ResolvedAt, err = parseNullTime(resolvedAt)
	return i, err
}

// IssueFilter selects issues. Zero values mean "any".
type IssueFilter struct {
	JobID  int64
	Status string
	Kind   string
	Limit  int
	Offset int
}


func (s *Store) ListIssues(ctx context.Context, f IssueFilter) ([]Issue, error) {
	limit, offset := clampPage(f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		 WHERE (? = 0 OR id IN (SELECT issue_id FROM issue_sightings WHERE job_id = ?))
		   AND (? = '' OR status = ?) AND (? = '' OR kind = ?)
		 ORDER BY id LIMIT ? OFFSET ?`,
		f.JobID, f.JobID, f.Status, f.Status, f.Kind, f.Kind, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}


func (s *Store) CountOpenIssues(ctx context.Context, jobID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues WHERE status = 'open'
		   AND (? = 0 OR id IN (SELECT issue_id FROM issue_sightings WHERE job_id = ?))`, jobID, jobID).Scan(&n)
	return n, err
}

func (s *Store) ResolveIssue(ctx context.Context, id int64, note string, now time.Time) (Issue, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE issues SET status = 'resolved', resolution_note = ?, resolved_at = ?
		 WHERE id = ? AND status = 'open'`, note, timeText(now), id); err != nil {
		return Issue{}, err
	}
	i, err := scanIssue(s.db.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, fmt.Errorf("issue %d: %w", id, ErrNotFound)
	}
	return i, err
}


var ErrStaleWrite = errors.New("refused to write older product information over newer")

func (t *Tx) GetProduct(ctx context.Context, sku string) (*catalog.Product, error) {
	p, err := scanProduct(t.tx.QueryRowContext(ctx, `SELECT `+productColumns+` FROM products WHERE sku = ?`, sku))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}


func (t *Tx) SaveProduct(ctx context.Context, p catalog.Product) error {
	var withdrawn any
	if p.WithdrawnAt != nil {
		withdrawn = timeText(*p.WithdrawnAt)
	}
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO products (`+productColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sku) DO UPDATE SET
		   name = excluded.name, description = excluded.description, category = excluded.category,
		   price_cents = excluded.price_cents, currency = excluded.currency, status = excluded.status,
		   version = excluded.version, withdrawn_at = excluded.withdrawn_at, updated_at = excluded.updated_at`,
		p.SKU, p.Name, p.Description, p.Category, p.PriceCents, p.Currency, string(p.Status), p.Version,
		withdrawn, timeText(p.CreatedAt), timeText(p.UpdatedAt))
	if err != nil {
		// Our guard triggers (see schema.sql) refuse stale writes with these messages.
		msg := err.Error()
		if strings.Contains(msg, "product version must not decrease") ||
			strings.Contains(msg, "can only be relisted by a newer version") {
			return fmt.Errorf("%w: %v", ErrStaleWrite, err)
		}
		return fmt.Errorf("save product %s: %w", p.SKU, err)
	}
	return nil
}

// NewChange describes a history entry to be written.
type NewChange struct {
	JobID         int64
	BatchIndex    int64
	Type          string
	Before        *catalog.Product // nil for a new product
	After         catalog.Product
	ChangedFields []string
	EventID       string
	At            time.Time
}

// AddChange writes one change-history entry.
func (t *Tx) AddChange(ctx context.Context, c NewChange) error {
	var before, versionBefore any
	if c.Before != nil {
		b, err := json.Marshal(c.Before)
		if err != nil {
			return err
		}
		before, versionBefore = string(b), c.Before.Version
	}
	after, err := json.Marshal(c.After)
	if err != nil {
		return err
	}
	fields := c.ChangedFields
	if fields == nil {
		fields = []string{}
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx,
		`INSERT INTO product_changes (job_id, batch_index, sku, change_type, version_before, version_after,
		   changed_fields, event_id, before_json, after_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.JobID, c.BatchIndex, c.After.SKU, c.Type, versionBefore, c.After.Version,
		string(fieldsJSON), c.EventID, before, string(after), timeText(c.At))
	return err
}

// NewIssue describes an issue to be written.
type NewIssue struct {
	JobID       int64
	BatchIndex  int64
	RecordIndex int64
	Kind        IssueKind
	SKU         string
	EventID     string
	Message     string
	RawRecord   string
	At          time.Time
}

const maxRawRecordBytes = 4096

func (t *Tx) AddIssue(ctx context.Context, i NewIssue) error {
	sum := sha256.Sum256([]byte(string(i.Kind) + "\x00" + i.RawRecord))
	fingerprint := hex.EncodeToString(sum[:])

	raw := i.RawRecord
	if len(raw) > maxRawRecordBytes {
		raw = raw[:maxRawRecordBytes] + "…(truncated)"
	}

	var issueID int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT id FROM issues WHERE fingerprint = ? AND status = 'open'`, fingerprint).Scan(&issueID)
	switch {
	case err == nil:
		if _, err := t.tx.ExecContext(ctx, `UPDATE issues SET occurrences = occurrences + 1 WHERE id = ?`, issueID); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := t.tx.ExecContext(ctx,
			`INSERT INTO issues (job_id, batch_index, record_index, kind, sku, event_id, message, raw_record, fingerprint, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i.JobID, i.BatchIndex, i.RecordIndex, string(i.Kind), i.SKU, i.EventID, i.Message, raw, fingerprint, timeText(i.At))
		if err != nil {
			return err
		}
		if issueID, err = res.LastInsertId(); err != nil {
			return err
		}
	default:
		return err
	}

	_, err = t.tx.ExecContext(ctx,
		`INSERT INTO issue_sightings (issue_id, job_id, batch_index, record_index) VALUES (?, ?, ?, ?)`,
		issueID, i.JobID, i.BatchIndex, i.RecordIndex)
	return err
}

type BatchResult struct {
	Delta      Counters
	NextCursor string
	SupplierDone bool
}

func (t *Tx) AdvanceJob(ctx context.Context, jobID, expectedBatches int64, r BatchResult, now time.Time) error {
	d := r.Delta
	done := boolInt(r.SupplierDone)
	res, err := t.tx.ExecContext(ctx,
		`UPDATE update_jobs SET
		   next_cursor = ?, batches_done = batches_done + 1, supplier_done = ?,
		   received = received + ?, created = created + ?, updated = updated + ?, relisted = relisted + ?,
		   withdrawn = withdrawn + ?, unchanged = unchanged + ?, duplicates = duplicates + ?,
		   stale = stale + ?, invalid = invalid + ?, conflicts = conflicts + ?,
		   status = CASE WHEN ? = 1 THEN 'completed' ELSE status END,
		   finished_at = CASE WHEN ? = 1 THEN ? ELSE finished_at END,
		   updated_at = ?
		 WHERE id = ? AND status = 'running' AND batches_done = ?`,
		r.NextCursor, done,
		d.Received, d.Created, d.Updated, d.Relisted, d.Withdrawn, d.Unchanged, d.Duplicates, d.Stale, d.Invalid, d.Conflicts,
		done, done, timeText(now), timeText(now),
		jobID, expectedBatches)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: update %d is no longer running at batch %d", ErrInvalidState, jobID, expectedBatches)
	}
	return nil
}
