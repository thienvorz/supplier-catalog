package updater_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"catalogsync/internal/store"
	"catalogsync/internal/supplier"
	"catalogsync/internal/updater"
)

const feedsDir = "../../data/feeds"

var bg = context.Background()

// env is one isolated application instance: its own database file, its own
// fake supplier, its own service.
type env struct {
	t    *testing.T
	path string
	st   *store.Store
	src  *supplier.Memory
	svc  *updater.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return openEnv(t, filepath.Join(t.TempDir(), "catalog.db"), supplier.NewMemory())
}

// openEnv opens (or reopens) the database at path. Reopening the same path with a
// fresh service is how tests simulate restarting the application.
func openEnv(t *testing.T, path string, src *supplier.Memory) *env {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := updater.New(st, src, updater.Config{MaxFetchAttempts: 1})
	// Cleanups run last-in-first-out: stop workers first, then close the database.
	t.Cleanup(func() { st.Close() })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		svc.Shutdown(ctx)
	})
	return &env{t: t, path: path, st: st, src: src, svc: svc}
}

// loadDemo makes a demo feed from data/feeds available under its own name.
func (e *env) loadDemo(name string) {
	e.t.Helper()
	f, err := supplier.ReadFeedFile(filepath.Join(feedsDir, name+".json"))
	if err != nil {
		e.t.Fatalf("read feed: %v", err)
	}
	e.src.SetFeedBatches(name, f.Batches)
}

// run creates a job and processes it synchronously. The returned job is
// re-read from the database, which is what staff would see.
func (e *env) run(feed string) (store.Job, error) {
	e.t.Helper()
	job, err := e.svc.CreateJob(bg, feed)
	if err != nil {
		e.t.Fatalf("create job: %v", err)
	}
	runErr := e.svc.Run(bg, job.ID)
	return e.job(job.ID), runErr
}

// resume claims an interrupted job and processes it synchronously.
func (e *env) resume(id int64) (store.Job, error) {
	e.t.Helper()
	if _, err := e.svc.ClaimJob(bg, id); err != nil {
		e.t.Fatalf("claim job: %v", err)
	}
	runErr := e.svc.Run(bg, id)
	return e.job(id), runErr
}

func (e *env) job(id int64) store.Job {
	e.t.Helper()
	j, err := e.st.GetJob(bg, id)
	if err != nil {
		e.t.Fatalf("get job %d: %v", id, err)
	}
	return j
}

// dump renders the whole catalog as text, ignoring timestamps, so two
// databases can be compared with a single string comparison.
func (e *env) dump() string {
	e.t.Helper()
	ps, err := e.st.ListProducts(bg, "", 500, 0)
	if err != nil {
		e.t.Fatalf("list products: %v", err)
	}
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, "%s|%s|%d %s|%s|v%d\n", p.SKU, p.Name, p.PriceCents, p.Currency, p.Status, p.Version)
	}
	return b.String()
}

// history renders what one job changed, in order, without ids or timestamps.
func (e *env) history(jobID int64) []string {
	e.t.Helper()
	cs, err := e.st.ListJobChanges(bg, jobID, 500, 0)
	if err != nil {
		e.t.Fatalf("list changes: %v", err)
	}
	var out []string
	for _, c := range cs {
		before := "-"
		if c.VersionBefore != nil {
			before = fmt.Sprintf("v%d", *c.VersionBefore)
		}
		out = append(out, fmt.Sprintf("b%d %s %s %s->v%d [%s]", c.BatchIndex, c.SKU, c.Type, before, c.VersionAfter,
			strings.Join(c.ChangedFields, ",")))
	}
	return out
}

func (e *env) product(sku string) (name string, price int64, status string, version int64, found bool) {
	e.t.Helper()
	p, err := e.st.GetProduct(bg, sku)
	if err != nil {
		return "", 0, "", 0, false
	}
	return p.Name, p.PriceCents, string(p.Status), p.Version, true
}

func (e *env) productCount() int {
	e.t.Helper()
	ps, err := e.st.ListProducts(bg, "", 500, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(ps)
}

func (e *env) openIssues(jobID int64) int64 {
	e.t.Helper()
	n, err := e.st.CountOpenIssues(bg, jobID)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// --- record builders --------------------------------------------------------

func up(sku string, version int, name string, price int) string {
	return fmt.Sprintf(`{"event_id":"e-%s-%d","sku":%q,"type":"upsert","version":%d,"name":%q,"category":"Desk","price_cents":%d,"currency":"USD"}`,
		sku, version, sku, version, name, price)
}

func wd(sku string, version int) string {
	return fmt.Sprintf(`{"event_id":"w-%s-%d","sku":%q,"type":"withdraw","version":%d}`, sku, version, sku, version)
}

// waitFor polls until cond is true or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
