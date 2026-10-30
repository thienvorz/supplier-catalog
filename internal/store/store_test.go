package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"catalogsync/internal/catalog"
)

var now = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// newStore opens a fresh database in a temporary directory: every test is isolated.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func product(sku string, version int64, status catalog.Status) catalog.Product {
	p := catalog.Product{SKU: sku, Name: "n", Currency: "USD", Status: status, Version: version, CreatedAt: now, UpdatedAt: now}
	if status == catalog.StatusWithdrawn {
		w := now
		p.WithdrawnAt = &w
	}
	return p
}

func save(t *testing.T, s *Store, p catalog.Product) error {
	t.Helper()
	return s.WithTx(context.Background(), func(tx *Tx) error { return tx.SaveProduct(context.Background(), p) })
}

func TestOpenIsIdempotentAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := save(t, s, product("A", 1, catalog.StatusActive)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path) // second start must not recreate or wipe anything
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetProduct(context.Background(), "A"); err != nil {
		t.Fatalf("data lost across reopen: %v", err)
	}
}

func TestDatabaseRefusesToLowerAProductVersion(t *testing.T) {
	s := newStore(t)
	if err := save(t, s, product("A", 5, catalog.StatusActive)); err != nil {
		t.Fatal(err)
	}
	err := save(t, s, product("A", 4, catalog.StatusActive))
	if !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("want ErrStaleWrite, got %v", err)
	}
	p, _ := s.GetProduct(context.Background(), "A")
	if p.Version != 5 {
		t.Fatalf("version changed to %d", p.Version)
	}
}

func TestDatabaseRefusesToRestoreWithdrawnProductWithOlderOrEqualVersion(t *testing.T) {
	s := newStore(t)
	if err := save(t, s, product("A", 5, catalog.StatusWithdrawn)); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{3, 5} {
		if err := save(t, s, product("A", v, catalog.StatusActive)); !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("version %d: want ErrStaleWrite, got %v", v, err)
		}
	}
	if p, _ := s.GetProduct(context.Background(), "A"); p.Status != catalog.StatusWithdrawn {
		t.Fatalf("product was restored: %+v", p)
	}
	// A strictly newer version is allowed to relist.
	if err := save(t, s, product("A", 6, catalog.StatusActive)); err != nil {
		t.Fatalf("newer relist must be allowed: %v", err)
	}
}

func TestProductIdentityIsEnforcedByTheDatabase(t *testing.T) {
	s := newStore(t)
	for _, sku := range []string{"lower-case", " PADDED ", ""} {
		_, err := s.db.Exec(`INSERT INTO products (sku, status, version, created_at, updated_at)
			VALUES (?, 'active', 1, 'x', 'x')`, sku)
		if err == nil {
			t.Errorf("sku %q must be rejected", sku)
		}
	}
	if err := save(t, s, product("A", 1, catalog.StatusActive)); err != nil {
		t.Fatal(err)
	}
	// The same SKU is the same product: saving again updates, never duplicates.
	if err := save(t, s, product("A", 2, catalog.StatusActive)); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 product, got %d", n)
	}
}

func TestJobCannotBeCompletedWithoutSupplierFinalBatch(t *testing.T) {
	s := newStore(t)
	j, err := s.CreateJob(context.Background(), "feed", now)
	if err != nil {
		t.Fatal(err)
	}
	// Even a buggy UPDATE cannot declare the job complete.
	_, err = s.db.Exec(`UPDATE update_jobs SET status = 'completed', finished_at = 'x' WHERE id = ?`, j.ID)
	if err == nil {
		t.Fatal("database allowed 'completed' although the supplier's final batch was never applied")
	}
}

func TestOnlyOneUnfinishedJobPerFeed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first, err := s.CreateJob(ctx, "feed", now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateJob(ctx, "feed", now)
	var oje *OpenJobError
	if !errors.As(err, &oje) || oje.JobID != first.ID || !errors.Is(err, ErrOpenJobExists) {
		t.Fatalf("want OpenJobError for job %d, got %v", first.ID, err)
	}
	if _, err := s.CreateJob(ctx, "other-feed", now); err != nil {
		t.Fatalf("a different feed must be independent: %v", err)
	}

	// Interrupted still counts as unfinished...
	if err := s.MarkInterrupted(ctx, first.ID, "boom", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(ctx, "feed", now); !errors.Is(err, ErrOpenJobExists) {
		t.Fatalf("interrupted job must still block: %v", err)
	}
	// ...until it is cancelled.
	if _, err := s.Cancel(ctx, first.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(ctx, "feed", now); err != nil {
		t.Fatalf("after cancel a new job must be allowed: %v", err)
	}
}

func TestConcurrentStartsCreateExactlyOneJob(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateJob(context.Background(), "feed", now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrOpenJobExists):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d jobs were created, want exactly 1", ok)
	}
}

func TestJobStateMachine(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.CreateJob(ctx, "feed", now)

	if _, err := s.ClaimInterrupted(ctx, j.ID, now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a running job cannot be resumed: %v", err)
	}
	if _, err := s.Cancel(ctx, j.ID, now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a running job cannot be cancelled: %v", err)
	}

	s.MarkInterrupted(ctx, j.ID, "supplier down", now)
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != JobInterrupted || got.LastError != "supplier down" {
		t.Fatalf("got %+v", got)
	}

	claimed, err := s.ClaimInterrupted(ctx, j.ID, now)
	if err != nil || claimed.Status != JobRunning || claimed.Resumes != 1 || claimed.LastError != "" {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := s.ClaimInterrupted(ctx, j.ID, now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second claim must fail: %v", err)
	}
	if _, err := s.GetJob(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRecoverRunningOnlyTouchesRunningJobs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _ := s.CreateJob(ctx, "a", now)
	b, _ := s.CreateJob(ctx, "b", now)
	s.MarkInterrupted(ctx, b.ID, "x", now)

	n, err := s.RecoverRunning(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, err %v", n, err)
	}
	ja, _ := s.GetJob(ctx, a.ID)
	if ja.Status != JobInterrupted || ja.LastError == "" {
		t.Fatalf("job a: %+v", ja)
	}
}

func TestAdvanceJobCompletesOnlyOnFinalBatchAndChecksPosition(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.CreateJob(ctx, "feed", now)

	// Wrong position: refused, and nothing is written.
	err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.AdvanceJob(ctx, j.ID, 7, BatchResult{NextCursor: "8"}, now)
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("want ErrInvalidState, got %v", err)
	}

	err = s.WithTx(ctx, func(tx *Tx) error {
		return tx.AdvanceJob(ctx, j.ID, 0, BatchResult{NextCursor: "1", Delta: Counters{Received: 3, Created: 3}}, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != JobRunning || got.BatchesDone != 1 || got.Cursor != "1" || got.Counters.Created != 3 || got.FinishedAt != nil {
		t.Fatalf("after first batch: %+v", got)
	}

	err = s.WithTx(ctx, func(tx *Tx) error {
		return tx.AdvanceJob(ctx, j.ID, 1, BatchResult{SupplierDone: true, Delta: Counters{Received: 1, Stale: 1}}, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, j.ID)
	if got.Status != JobCompleted || !got.SupplierDone || got.FinishedAt == nil || got.Counters.Received != 4 {
		t.Fatalf("after final batch: %+v", got)
	}
	// A finished job cannot be advanced again.
	err = s.WithTx(ctx, func(tx *Tx) error { return tx.AdvanceJob(ctx, j.ID, 2, BatchResult{NextCursor: "3"}, now) })
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("completed job must not advance: %v", err)
	}
}

func TestTransactionRollsBackEverythingOnError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.SaveProduct(ctx, product("A", 1, catalog.StatusActive)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.GetProduct(ctx, "A"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("product survived a rolled-back transaction: %v", err)
	}
}

func TestIssueResolveIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.CreateJob(ctx, "feed", now)
	err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.AddIssue(ctx, NewIssue{JobID: j.ID, Kind: IssueInvalidRecord, Message: "bad", RawRecord: "{}", At: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same position cannot be recorded twice (protects against replays).
	err = s.WithTx(ctx, func(tx *Tx) error {
		return tx.AddIssue(ctx, NewIssue{JobID: j.ID, Kind: IssueInvalidRecord, Message: "bad", RawRecord: "{}", At: now})
	})
	if err == nil {
		t.Fatal("duplicate issue position must be rejected")
	}

	if n, _ := s.CountOpenIssues(ctx, j.ID); n != 1 {
		t.Fatalf("open issues = %d", n)
	}
	i, err := s.ResolveIssue(ctx, 1, "fixed at supplier", now)
	if err != nil || i.Status != "resolved" || i.ResolvedAt == nil {
		t.Fatalf("resolve: %+v %v", i, err)
	}
	if _, err := s.ResolveIssue(ctx, 1, "again", now); err != nil {
		t.Fatalf("second resolve must be harmless: %v", err)
	}
	if _, err := s.ResolveIssue(ctx, 42, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if n, _ := s.CountOpenIssues(ctx, 0); n != 0 {
		t.Fatalf("open issues = %d", n)
	}
}
