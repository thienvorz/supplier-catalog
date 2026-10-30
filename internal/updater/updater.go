// Package updater runs supplier updates.
//
// How an update works, in four sentences:
//  1. Staff start an update for a feed; a job row is created in state "running".
//  2. A worker fetches one batch at a time from the supplier.
//  3. Each batch is applied in ONE database transaction that also moves the
//     job's cursor forward (and, for the supplier's final batch, completes the job).
//  4. If anything goes wrong or the process dies, the job is "interrupted" and
//     staff resume it from the stored cursor; nothing already committed is redone.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"catalogsync/internal/catalog"
	"catalogsync/internal/store"
	"catalogsync/internal/supplier"
)

// Errors returned to callers.
var (
	ErrInvalidFeed  = errors.New("invalid feed name")
	ErrShuttingDown = errors.New("application is shutting down")
	// ErrNoProgress is returned when the supplier says "more to come" without a
	// usable cursor. Looping on it forever would hang the update.
	ErrNoProgress = errors.New("supplier did not provide a new cursor although the feed is not finished")
)

var feedPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Config tunes the service. Zero values get sensible defaults.
type Config struct {
	// MaxFetchAttempts is how many times one batch fetch is tried before the
	// update is interrupted. Default 3.
	MaxFetchAttempts int
	// RetryBackoff is the pause before the second attempt; it doubles after
	// each further failure. Zero means no pause (handy in tests); the server
	// command defaults to 500ms.
	RetryBackoff time.Duration
	Logger       *slog.Logger
	// Now is the clock; tests may replace it.
	Now func() time.Time
}

// Hooks are fault-injection points for tests and the crash demo.
// They are nil in normal operation.
type Hooks struct {
	// BeforeCommit runs after a batch was applied but before it is committed.
	// Returning an error aborts the batch: it is rolled back as if the process
	// had died at that point.
	BeforeCommit func(jobID, batchIndex int64) error
	// AfterCommit runs right after a batch committed.
	AfterCommit func(jobID, batchIndex int64)
}

// Service starts, runs, resumes and cancels updates.
type Service struct {
	store  *store.Store
	source supplier.Source
	cfg    Config
	Hooks  Hooks

	mu      sync.Mutex
	closed  bool
	cancels map[int64]context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a Service.
func New(st *store.Store, src supplier.Source, cfg Config) *Service {
	if cfg.MaxFetchAttempts <= 0 {
		cfg.MaxFetchAttempts = 3
	}
	if cfg.RetryBackoff < 0 {
		cfg.RetryBackoff = 0
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: st, source: src, cfg: cfg, cancels: map[int64]context.CancelFunc{}}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Recover must be called once at startup, before serving requests. Jobs still
// marked "running" belong to a process that no longer exists; they become
// "interrupted" so staff can resume them. Returns how many were recovered.
func (s *Service) Recover(ctx context.Context) (int64, error) {
	n, err := s.store.RecoverRunning(ctx, s.cfg.Now())
	if err == nil && n > 0 {
		s.cfg.Logger.Warn("recovered unfinished updates after restart", "count", n)
	}
	return n, err
}

// CreateJob validates the feed name and creates a job in state "running"
// without starting a worker. Most callers want Start instead.
func (s *Service) CreateJob(ctx context.Context, feed string) (store.Job, error) {
	feed = strings.TrimSpace(feed)
	if !feedPattern.MatchString(feed) {
		return store.Job{}, fmt.Errorf("%w: use 1-100 letters, digits, '.', '_' or '-'", ErrInvalidFeed)
	}
	if s.isClosed() {
		return store.Job{}, ErrShuttingDown
	}
	return s.store.CreateJob(ctx, feed, s.cfg.Now())
}

// ClaimJob moves an interrupted job back to "running" without starting a
// worker. Most callers want Resume instead.
func (s *Service) ClaimJob(ctx context.Context, id int64) (store.Job, error) {
	if s.isClosed() {
		return store.Job{}, ErrShuttingDown
	}
	return s.store.ClaimInterrupted(ctx, id, s.cfg.Now())
}

// Start creates a job and processes it in the background.
func (s *Service) Start(ctx context.Context, feed string) (store.Job, error) {
	job, err := s.CreateJob(ctx, feed)
	if err != nil {
		return store.Job{}, err
	}
	s.launch(job.ID)
	return job, nil
}

// Resume continues an interrupted job in the background from its stored cursor.
func (s *Service) Resume(ctx context.Context, id int64) (store.Job, error) {
	job, err := s.ClaimJob(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	s.launch(job.ID)
	return job, nil
}

// Cancel abandons an interrupted job for good. It is never reported as complete.
func (s *Service) Cancel(ctx context.Context, id int64) (store.Job, error) {
	return s.store.Cancel(ctx, id, s.cfg.Now())
}

// Shutdown stops all workers. Each one finishes or rolls back the batch it is
// applying, marks its job "interrupted" and exits. Waits until ctx expires.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// launch runs the job in a goroutine that outlives the HTTP request.
func (s *Service) launch(jobID int64) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.interrupt(jobID, "update interrupted: the application was shutting down")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancels[jobID] = cancel
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.cancels, jobID)
			s.mu.Unlock()
		}()
		if err := s.Run(ctx, jobID); err != nil {
			s.cfg.Logger.Warn("update interrupted", "job", jobID, "reason", err)
		}
	}()
}

// Run processes a job that is in state "running" until the supplier's final
// batch has been applied (returns nil) or something goes wrong (returns the
// error, after marking the job "interrupted").
//
// It always continues from the cursor stored in the database, so calling it
// again after a failure, or from a brand-new process, simply carries on.
func (s *Service) Run(ctx context.Context, jobID int64) (err error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Status != store.JobRunning {
		return fmt.Errorf("%w: update %d is %s, not running", store.ErrInvalidState, jobID, job.Status)
	}

	defer func() {
		if err == nil {
			return
		}
		reason := err.Error()
		if ctx.Err() != nil {
			reason = "update interrupted: the application was shutting down"
		}
		s.interrupt(jobID, reason)
	}()

	log := s.cfg.Logger.With("job", jobID, "feed", job.Feed)
	log.Info("update running", "from_batch", job.BatchesDone, "cursor", job.Cursor)

	for {
		batch, err := s.fetch(ctx, job.Feed, job.Cursor)
		if err != nil {
			return fmt.Errorf("fetching batch %d from supplier: %w", job.BatchesDone, err)
		}
		if !batch.Done && (batch.NextCursor == "" || batch.NextCursor == job.Cursor) {
			return fmt.Errorf("after batch %d: %w", job.BatchesDone, ErrNoProgress)
		}

		if err := s.applyBatch(ctx, job, batch); err != nil {
			return fmt.Errorf("applying batch %d: %w", job.BatchesDone, err)
		}
		if s.Hooks.AfterCommit != nil {
			s.Hooks.AfterCommit(jobID, job.BatchesDone)
		}

		// The database is the source of truth: reload instead of trusting memory.
		if job, err = s.store.GetJob(ctx, jobID); err != nil {
			return err
		}
		log.Info("batch applied", "batches_done", job.BatchesDone, "status", job.Status)
		if job.Status == store.JobCompleted {
			return nil
		}
	}
}

// interrupt marks a running job interrupted. It uses its own context because
// the one the worker used may already be cancelled (shutdown).
func (s *Service) interrupt(jobID int64, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.MarkInterrupted(ctx, jobID, reason, s.cfg.Now()); err != nil {
		s.cfg.Logger.Error("could not mark update interrupted", "job", jobID, "error", err)
	}
}

// fetch gets one batch, retrying transient supplier failures with backoff.
func (s *Service) fetch(ctx context.Context, feed, cursor string) (supplier.Batch, error) {
	backoff := s.cfg.RetryBackoff
	var lastErr error
	for attempt := 1; attempt <= s.cfg.MaxFetchAttempts; attempt++ {
		b, err := s.source.FetchBatch(ctx, feed, cursor)
		if err == nil {
			return b, nil
		}
		if ctx.Err() != nil {
			return supplier.Batch{}, ctx.Err()
		}
		if errors.Is(err, supplier.ErrFeedNotFound) {
			return supplier.Batch{}, err // retrying cannot help
		}
		lastErr = err
		if attempt == s.cfg.MaxFetchAttempts {
			break
		}
		s.cfg.Logger.Warn("supplier fetch failed, will retry", "attempt", attempt, "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return supplier.Batch{}, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
	}
	return supplier.Batch{}, fmt.Errorf("supplier unavailable after %d attempts: %w", s.cfg.MaxFetchAttempts, lastErr)
}

// applyBatch applies every record of one batch and advances the job, all in a
// single transaction: either the whole batch and its progress are saved, or
// nothing is.
func (s *Service) applyBatch(ctx context.Context, job store.Job, b supplier.Batch) error {
	batchIndex := job.BatchesDone

	err := s.store.WithTx(ctx, func(tx *store.Tx) error {
		now := s.cfg.Now()
		var delta store.Counters

		for i, raw := range b.Records {
			delta.Received++

			rec, err := catalog.ParseRecord(raw)
			if err != nil {
				delta.Invalid++
				sku, eventID := peek(raw)
				if err := tx.AddIssue(ctx, store.NewIssue{
					JobID: job.ID, BatchIndex: batchIndex, RecordIndex: int64(i),
					Kind: store.IssueInvalidRecord, SKU: sku, EventID: eventID,
					Message: err.Error(), RawRecord: string(raw), At: now,
				}); err != nil {
					return err
				}
				continue
			}

			existing, err := tx.GetProduct(ctx, rec.SKU)
			if err != nil {
				return err
			}
			d := catalog.Decide(existing, rec, now)
			count(&delta, d.Outcome)

			switch {
			case d.Outcome == catalog.OutcomeConflict:
				if err := tx.AddIssue(ctx, store.NewIssue{
					JobID: job.ID, BatchIndex: batchIndex, RecordIndex: int64(i),
					Kind: store.IssueVersionConflict, SKU: rec.SKU, EventID: rec.EventID,
					Message: d.Reason, RawRecord: string(raw), At: now,
				}); err != nil {
					return err
				}

			case d.Outcome.WritesProduct():
				if err := tx.SaveProduct(ctx, d.Product); err != nil {
					return err
				}
				if d.Outcome.IsChange() {
					if err := tx.AddChange(ctx, store.NewChange{
						JobID: job.ID, BatchIndex: batchIndex, Type: string(d.Outcome),
						Before: existing, After: d.Product, ChangedFields: d.ChangedFields,
						EventID: rec.EventID, At: now,
					}); err != nil {
						return err
					}
				}
			}
		}

		// Internal consistency check: every record must be accounted for.
		if delta.Accounted() != delta.Received {
			return fmt.Errorf("internal error: %d records received but %d accounted for", delta.Received, delta.Accounted())
		}

		if err := tx.AdvanceJob(ctx, job.ID, batchIndex, store.BatchResult{
			Delta: delta, NextCursor: b.NextCursor, SupplierDone: b.Done,
		}, now); err != nil {
			return err
		}

		if s.Hooks.BeforeCommit != nil {
			return s.Hooks.BeforeCommit(job.ID, batchIndex)
		}
		return nil
	})
	return err
}

func count(c *store.Counters, o catalog.Outcome) {
	switch o {
	case catalog.OutcomeCreated:
		c.Created++
	case catalog.OutcomeUpdated:
		c.Updated++
	case catalog.OutcomeRelisted:
		c.Relisted++
	case catalog.OutcomeWithdrawn:
		c.Withdrawn++
	case catalog.OutcomeUnchanged:
		c.Unchanged++
	case catalog.OutcomeDuplicate:
		c.Duplicates++
	case catalog.OutcomeStale:
		c.Stale++
	case catalog.OutcomeConflict:
		c.Conflicts++
	}
}

// peek extracts the SKU and event id from a record that failed validation, so
// the issue can say which product it was about. Best effort: it never fails.
func peek(raw []byte) (sku, eventID string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", ""
	}
	str := func(key string) string {
		var s string
		if v, ok := m[key]; ok {
			_ = json.Unmarshal(v, &s)
		}
		s = strings.TrimSpace(s)
		if len(s) > 100 {
			s = s[:100]
		}
		return s
	}
	return catalog.NormalizeSKU(str("sku")), str("event_id")
}
