package updater_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"catalogsync/internal/store"
	"catalogsync/internal/supplier"
	"catalogsync/internal/updater"
)

const (
	day1 = "day1-initial"
	day2 = "day2-changes"
)

// ---------------------------------------------------------------------------
// The demo data: the numbers in README.md and DEMO.md come from these tests.
// ---------------------------------------------------------------------------

func TestDemoFeedsProduceTheDocumentedResults(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.loadDemo(day2)

	// ---- day 1 ----
	j1, err := e.run(day1)
	if err != nil {
		t.Fatalf("day1: %v", err)
	}
	want1 := store.Counters{Received: 22, Created: 10, Updated: 1, Withdrawn: 2, Duplicates: 2, Stale: 2, Invalid: 4, Conflicts: 1}
	if j1.Status != store.JobCompleted || j1.BatchesDone != 4 || !j1.SupplierDone {
		t.Fatalf("day1 job: %+v", j1)
	}
	if j1.Counters != want1 {
		t.Fatalf("day1 counters\n got %+v\nwant %+v", j1.Counters, want1)
	}
	if j1.Counters.Accounted() != j1.Counters.Received {
		t.Fatalf("every record must be accounted for: %+v", j1.Counters)
	}
	if n := e.openIssues(j1.ID); n != 5 {
		t.Fatalf("open issues after day1 = %d, want 5 (4 invalid + 1 conflict)", n)
	}
	counts, _ := e.st.ProductCounts(bg)
	if counts["active"] != 9 || counts["withdrawn"] != 2 {
		t.Fatalf("product counts after day1: %v", counts)
	}

	// Specific rules visible in the data:
	if _, _, status, version, _ := e.product("HP-1008"); status != "withdrawn" || version != 3 {
		t.Errorf("HP-1008: the withdrawal (v3) arrived before the older upsert (v2) and must win; got %s v%d", status, version)
	}
	if _, price, _, version, _ := e.product("HP-1007"); price != 2400 || version != 2 {
		t.Errorf("HP-1007: v2 arrived first, the late v1 must be ignored; got %d v%d", price, version)
	}
	if _, price, _, _, _ := e.product("HP-1005"); price != 2200 {
		t.Errorf("HP-1005: a conflicting same-version record must not overwrite; price=%d", price)
	}
	if name, _, _, _, found := e.product("HP-1011"); !found || name != "Wool Throw Blanket" {
		t.Errorf("HP-1011: ' hp-1011 ' must be normalised to HP-1011")
	}
	for _, sku := range []string{"HP-1009", "HP-1010", "HP-1012"} {
		if _, _, _, _, found := e.product(sku); found {
			t.Errorf("%s was invalid and must not be in the catalog", sku)
		}
	}

	// ---- day 2 ----
	j2, err := e.run(day2)
	if err != nil {
		t.Fatalf("day2: %v", err)
	}
	want2 := store.Counters{Received: 12, Created: 2, Updated: 2, Relisted: 1, Withdrawn: 1, Unchanged: 1, Duplicates: 2, Stale: 2, Invalid: 1}
	if j2.Status != store.JobCompleted || j2.Counters != want2 {
		t.Fatalf("day2 job: %+v\nwant counters %+v", j2, want2)
	}
	counts, _ = e.st.ProductCounts(bg)
	if counts["active"] != 11 || counts["withdrawn"] != 2 || e.productCount() != 13 {
		t.Fatalf("product counts after day2: %v", counts)
	}
	if n := e.openIssues(0); n != 6 {
		t.Fatalf("open issues overall = %d, want 6", n)
	}
	if _, _, status, version, _ := e.product("HP-1008"); status != "active" || version != 4 {
		t.Errorf("HP-1008 must be relisted by the NEWER v4; got %s v%d", status, version)
	}
	if _, _, status, version, _ := e.product("HP-1004"); status != "withdrawn" || version != 2 {
		t.Errorf("HP-1004: an old upsert must not restore it; got %s v%d", status, version)
	}
	if _, _, status, _, _ := e.product("HP-1006"); status != "withdrawn" {
		t.Errorf("HP-1006 must be withdrawn")
	}

	// History: what changed, per product.
	h, _ := e.st.ListProductChanges(bg, "HP-1008", 50, 0)
	if len(h) != 2 || h[0].Type != "withdrawn" || h[1].Type != "relisted" {
		t.Errorf("HP-1008 history: %+v", h)
	}
	h, _ = e.st.ListProductChanges(bg, "HP-1001", 50, 0)
	if len(h) != 2 || h[1].Type != "updated" || !reflect.DeepEqual(h[1].ChangedFields, []string{"price_cents"}) {
		t.Errorf("HP-1001 history: %+v", h)
	}
	if h, _ = e.st.ListProductChanges(bg, "HP-1002", 50, 0); len(h) != 1 {
		t.Errorf("HP-1002 only moved its version number, so it has exactly 1 history entry (creation); got %d", len(h))
	}
}

// ---------------------------------------------------------------------------
// Idempotency, ordering, withdrawal protection
// ---------------------------------------------------------------------------

func TestImportingTheSameUpdateTwiceCreatesNoDuplicatesAndNoChanges(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	first, _ := e.run(day1)
	before := e.dump()
	changesBefore, _ := e.st.ListJobChanges(bg, first.ID, 500, 0)

	second, err := e.run(day1)
	if err != nil {
		t.Fatal(err)
	}
	if e.dump() != before {
		t.Fatalf("second import changed the catalog:\n%s\nvs\n%s", before, e.dump())
	}
	c := second.Counters
	if c.Created+c.Updated+c.Withdrawn+c.Relisted+c.Unchanged != 0 {
		t.Fatalf("second import must change nothing, counters: %+v", c)
	}
	if c.Duplicates+c.Stale+c.Invalid+c.Conflicts != c.Received {
		t.Fatalf("second import must only see repeats, stale data and the same bad records: %+v", c)
	}
	if n := len(e.history(second.ID)); n != 0 {
		t.Fatalf("second import wrote %d change-history entries", n)
	}
	if n := len(changesBefore); n != 13 {
		t.Fatalf("first import wrote %d history entries, want 13", n)
	}
}

func TestOutOfOrderDeliveryGivesTheSameResultAsInOrderDelivery(t *testing.T) {
	// The same information for one product, delivered in two different orders
	// and split across batches differently.
	inOrder := [][]string{
		{up("A", 1, "Desk", 100), up("A", 2, "Desk", 120)},
		{wd("A", 3), up("A", 4, "Desk v4", 140)},
	}
	scrambled := [][]string{
		{up("A", 4, "Desk v4", 140), up("A", 1, "Desk", 100)},
		{wd("A", 3), up("A", 2, "Desk", 120), up("A", 2, "Desk", 120)},
	}

	a, b := newEnv(t), newEnv(t)
	a.src.SetFeed("f", inOrder...)
	b.src.SetFeed("f", scrambled...)
	if _, err := a.run("f"); err != nil {
		t.Fatal(err)
	}
	jb, err := b.run("f")
	if err != nil {
		t.Fatal(err)
	}
	if a.dump() != b.dump() {
		t.Fatalf("order changed the outcome:\nin order:\n%s\nscrambled:\n%s", a.dump(), b.dump())
	}
	if name, price, status, version, _ := b.product("A"); name != "Desk v4" || price != 140 || status != "active" || version != 4 {
		t.Fatalf("final state: %s %d %s v%d", name, price, status, version)
	}
	if jb.Counters.Stale != 4 {
		t.Fatalf("v1, the v3 withdrawal, v2 and the repeated v2 all arrive after v4 and are stale, got %+v", jb.Counters)
	}
}

func TestWithdrawalArrivingBeforeOlderUpsertIsNotUndone(t *testing.T) {
	e := newEnv(t)
	// The product is unknown locally; the withdrawal arrives first, then the
	// older upsert in the next batch.
	e.src.SetFeed("f", []string{wd("A", 5)}, []string{up("A", 3, "Desk", 100)})
	job, err := e.run("f")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, status, version, found := e.product("A"); !found || status != "withdrawn" || version != 5 {
		t.Fatalf("product must stay withdrawn at v5, got %s v%d (found=%v)", status, version, found)
	}
	if job.Counters.Withdrawn != 1 || job.Counters.Stale != 1 {
		t.Fatalf("counters: %+v", job.Counters)
	}
}

func TestOlderInformationFromALaterUpdateNeverOverwritesNewerData(t *testing.T) {
	e := newEnv(t)
	e.src.SetFeed("new", []string{up("A", 5, "Newest", 500), up("B", 2, "B", 20)})
	e.src.SetFeed("old", []string{up("A", 3, "Old", 300), wd("B", 1)})

	if _, err := e.run("new"); err != nil {
		t.Fatal(err)
	}
	before := e.dump()
	job, err := e.run("old")
	if err != nil {
		t.Fatal(err)
	}
	if e.dump() != before {
		t.Fatalf("stale update changed the catalog:\n%s", e.dump())
	}
	if job.Counters.Stale != 2 {
		t.Fatalf("counters: %+v", job.Counters)
	}
}

func TestWithdrawnProductReturnsOnlyThroughAStrictlyNewerVersion(t *testing.T) {
	e := newEnv(t)
	e.src.SetFeed("w", []string{up("A", 1, "Desk", 100), wd("A", 4)})
	e.src.SetFeed("older", []string{up("A", 3, "Desk", 100), up("A", 4, "Desk", 100)}) // older, and same version
	e.src.SetFeed("newer", []string{up("A", 5, "Desk again", 110)})

	e.run("w")
	j, _ := e.run("older")
	if _, _, status, _, _ := e.product("A"); status != "withdrawn" {
		t.Fatalf("older/equal information restored a withdrawn product")
	}
	if j.Counters.Stale != 1 || j.Counters.Conflicts != 1 {
		t.Fatalf("v3 is stale and the same-version upsert is a conflict for staff: %+v", j.Counters)
	}

	j, _ = e.run("newer")
	if _, _, status, version, _ := e.product("A"); status != "active" || version != 5 {
		t.Fatalf("a strictly newer upsert must be able to relist, got %s v%d", status, version)
	}
	if j.Counters.Relisted != 1 {
		t.Fatalf("relisting must be reported as such: %+v", j.Counters)
	}
}

func TestSameVersionWithDifferentContentIsAnIssueNotAnOverwrite(t *testing.T) {
	e := newEnv(t)
	e.src.SetFeed("f", []string{up("A", 1, "Desk", 100)}, []string{up("A", 1, "Desk", 999)})
	job, _ := e.run("f")
	if _, price, _, _, _ := e.product("A"); price != 100 {
		t.Fatalf("conflicting record overwrote the product: %d", price)
	}
	issues, _ := e.st.ListIssues(bg, store.IssueFilter{JobID: job.ID})
	if len(issues) != 1 || issues[0].Kind != store.IssueVersionConflict || issues[0].SKU != "A" ||
		!strings.Contains(issues[0].Message, "price_cents") || issues[0].Status != "open" {
		t.Fatalf("issues: %+v", issues)
	}
}

func TestInvalidRecordsAreRecordedNotSilentlyDropped(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	job, _ := e.run(day1)

	issues, err := e.st.ListIssues(bg, store.IssueFilter{JobID: job.ID, Kind: string(store.IssueInvalidRecord)})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(issues)) != job.Counters.Invalid || len(issues) != 4 {
		t.Fatalf("%d invalid issues for %d invalid records", len(issues), job.Counters.Invalid)
	}
	bySKU := map[string]store.Issue{}
	for _, i := range issues {
		bySKU[i.SKU] = i
		if i.RawRecord == "" || i.Message == "" || i.Status != "open" {
			t.Errorf("issue must keep the original record and say what is wrong: %+v", i)
		}
	}
	for sku, want := range map[string]string{
		"HP-1009": "name is required",
		"HP-1010": "must not be negative",
		"HP-1012": "expected field types",
		"":        "sku is required",
	} {
		i, ok := bySKU[sku]
		if !ok || !strings.Contains(i.Message, want) {
			t.Errorf("issue for %q: %+v, want message containing %q", sku, i, want)
		}
	}

	// Staff can work through them.
	resolved, err := e.st.ResolveIssue(bg, issues[0].ID, "asked supplier to resend", time.Now())
	if err != nil || resolved.Status != "resolved" {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	if n := e.openIssues(job.ID); n != 4 { // 5 open - 1 resolved
		t.Fatalf("open issues = %d", n)
	}
}

// ---------------------------------------------------------------------------
// Supplier outage
// ---------------------------------------------------------------------------

func TestSupplierOutageNeverErasesTheExistingCatalog(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.loadDemo(day2)
	if _, err := e.run(day1); err != nil {
		t.Fatal(err)
	}
	before := e.dump()

	e.src.OutageFrom(0) // supplier is completely down
	job, err := e.run(day2)
	if err == nil {
		t.Fatal("expected an error while the supplier is down")
	}
	if e.dump() != before {
		t.Fatalf("outage changed the catalog")
	}
	if job.Status != store.JobInterrupted || job.BatchesDone != 0 || !strings.Contains(job.LastError, "supplier") {
		t.Fatalf("job: %+v", job)
	}
}

func TestOutageMidwayKeepsCompletedBatchesAndNeverReportsComplete(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.src.OutageFrom(2) // batches 0 and 1 succeed, batch 2 fails

	job, err := e.run(day1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if job.Status != store.JobInterrupted || job.SupplierDone || job.FinishedAt != nil || job.BatchesDone != 2 || job.Cursor != "2" {
		t.Fatalf("partial update must be interrupted, not complete: %+v", job)
	}
	if job.Counters.Received != 10 {
		t.Fatalf("batches 0 and 1 (10 records) were applied and must be counted: %+v", job.Counters)
	}
	if e.productCount() != 8 { // 5 + 1006 + 1007 + tombstone 1008
		t.Fatalf("work from completed batches must be kept; products = %d\n%s", e.productCount(), e.dump())
	}
	if done, _ := e.st.ListJobs(bg, string(store.JobCompleted), 10, 0); len(done) != 0 {
		t.Fatalf("no job may be reported as completed: %+v", done)
	}
}

func TestResumeContinuesFromTheStoredCursorWithoutRefetching(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.src.OutageFrom(2)
	job, _ := e.run(day1)

	if got, want := e.src.Calls(), []string{day1 + "@", day1 + "@1", day1 + "@2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first run calls = %v, want %v", got, want)
	}

	e.src.ClearOutage()
	e.src.ResetCalls()
	resumed, err := e.resume(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.src.Calls(), []string{day1 + "@2", day1 + "@3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resume must start at batch 2 and not repeat finished work; calls = %v, want %v", got, want)
	}
	if resumed.Status != store.JobCompleted || resumed.Resumes != 1 || resumed.LastError != "" || resumed.BatchesDone != 4 {
		t.Fatalf("resumed job: %+v", resumed)
	}
}

// The strongest recovery property: stopping at ANY batch boundary and resuming
// gives exactly the same catalog, counters and history as an uninterrupted run.
func TestInterruptedAndResumedEqualsUninterrupted(t *testing.T) {
	ref := newEnv(t)
	ref.loadDemo(day1)
	refJob, err := ref.run(day1)
	if err != nil {
		t.Fatal(err)
	}

	for k := 0; k < 4; k++ {
		t.Run(fmt.Sprintf("outage_at_batch_%d", k), func(t *testing.T) {
			e := newEnv(t)
			e.loadDemo(day1)
			e.src.OutageFrom(k)
			job, err := e.run(day1)
			if err == nil || job.Status != store.JobInterrupted || job.Status == store.JobCompleted || job.BatchesDone != int64(k) {
				t.Fatalf("outage at %d: job %+v err %v", k, job, err)
			}
			e.src.ClearOutage()
			done, err := e.resume(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if done.Status != store.JobCompleted || done.Counters != refJob.Counters {
				t.Fatalf("counters differ:\n got %+v\nwant %+v", done.Counters, refJob.Counters)
			}
			if e.dump() != ref.dump() {
				t.Fatalf("catalog differs from the uninterrupted run:\n%s\nvs\n%s", e.dump(), ref.dump())
			}
			if !reflect.DeepEqual(e.history(job.ID), ref.history(refJob.ID)) {
				t.Fatalf("history differs:\n%v\nvs\n%v", e.history(job.ID), ref.history(refJob.ID))
			}
		})
	}
}

func TestPartialUpdateIsNeverReportedCompleteAtAnyFailurePoint(t *testing.T) {
	for k := 0; k < 4; k++ {
		e := newEnv(t)
		e.loadDemo(day1)
		e.src.OutageFrom(k)
		job, _ := e.run(day1)
		if job.Status == store.JobCompleted || job.SupplierDone || job.FinishedAt != nil {
			t.Fatalf("outage at batch %d: reported as complete: %+v", k, job)
		}
	}
}

func TestTransientSupplierErrorsAreRetried(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.svc = updater.New(e.st, e.src, updater.Config{MaxFetchAttempts: 3}) // no backoff pause in tests
	e.src.FailNext(2)

	job, err := e.run(day1)
	if err != nil || job.Status != store.JobCompleted || job.Resumes != 0 {
		t.Fatalf("two glitches must be absorbed by retries: %+v err %v", job, err)
	}
	if n := len(e.src.Calls()); n != 4+2 {
		t.Fatalf("calls = %d, want 6", n)
	}
}

func TestRetriesGiveUpAndInterruptTheUpdate(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.svc = updater.New(e.st, e.src, updater.Config{MaxFetchAttempts: 3})
	e.src.FailNext(10)

	job, err := e.run(day1)
	if err == nil || job.Status != store.JobInterrupted || !strings.Contains(job.LastError, "after 3 attempts") {
		t.Fatalf("job %+v err %v", job, err)
	}
	if n := len(e.src.Calls()); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestUnknownFeedIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.svc = updater.New(e.st, e.src, updater.Config{MaxFetchAttempts: 3})
	job, err := e.run("no-such-feed")
	if !errors.Is(err, supplier.ErrFeedNotFound) || job.Status != store.JobInterrupted {
		t.Fatalf("job %+v err %v", job, err)
	}
	if n := len(e.src.Calls()); n != 1 {
		t.Fatalf("a missing feed must not be retried; calls = %d", n)
	}
}

type sourceFunc func(ctx context.Context, feed, cursor string) (supplier.Batch, error)

func (f sourceFunc) FetchBatch(ctx context.Context, feed, cursor string) (supplier.Batch, error) {
	return f(ctx, feed, cursor)
}

func TestSupplierThatNeverFinishesCannotHangAnUpdate(t *testing.T) {
	e := newEnv(t)
	e.svc = updater.New(e.st, sourceFunc(func(ctx context.Context, feed, cursor string) (supplier.Batch, error) {
		return supplier.Batch{Done: false, NextCursor: cursor}, nil // never advances
	}), updater.Config{})
	job, err := e.run("f")
	if !errors.Is(err, updater.ErrNoProgress) || job.Status != store.JobInterrupted {
		t.Fatalf("job %+v err %v", job, err)
	}
}

// ---------------------------------------------------------------------------
// Atomic batches, crashes and restarts
// ---------------------------------------------------------------------------

func TestABatchIsAppliedCompletelyOrNotAtAll(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.svc.Hooks.BeforeCommit = func(jobID, batch int64) error {
		if batch == 1 {
			return errors.New("simulated failure just before commit")
		}
		return nil
	}
	job, err := e.run(day1)
	if err == nil {
		t.Fatal("expected failure")
	}
	if job.Status != store.JobInterrupted || job.BatchesDone != 1 || job.Cursor != "1" || job.Counters.Received != 5 {
		t.Fatalf("progress must stop exactly at the last committed batch: %+v", job)
	}
	// Batch 1 contained HP-1006, HP-1007 and a change to HP-1003; none of it may be visible.
	if e.productCount() != 5 {
		t.Fatalf("half of a batch leaked into the catalog:\n%s", e.dump())
	}
	if name, price, _, version, _ := e.product("HP-1003"); name != "Linen Apron" || price != 2800 || version != 1 {
		t.Fatalf("HP-1003 changed by an uncommitted batch: %d v%d", price, version)
	}
	for _, line := range e.history(job.ID) {
		if !strings.HasPrefix(line, "b0 ") {
			t.Fatalf("history entry from the rolled-back batch: %s", line)
		}
	}

	// Remove the fault; the job resumes and re-applies batch 1 cleanly.
	e.svc.Hooks.BeforeCommit = nil
	done, err := e.resume(job.ID)
	if err != nil || done.Status != store.JobCompleted || done.Counters.Received != 22 {
		t.Fatalf("resume after rollback: %+v err %v", done, err)
	}
}

// crash simulates the process dying: the hook panics, so nothing after it
// (not even marking the job interrupted) runs.
func crash(t *testing.T, e *env, jobID int64) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected the simulated crash")
		}
	}()
	e.svc.Run(bg, jobID)
}

func TestApplicationCrashAtAnyPointThenRestartAndResume(t *testing.T) {
	ref := newEnv(t)
	ref.loadDemo(day1)
	refJob, _ := ref.run(day1)

	type point struct {
		name  string
		hook  func(s *updater.Service, k int64)
		fresh func(k int64) (batchesCommitted int64)
	}
	points := []point{
		{"during_batch", func(s *updater.Service, k int64) {
			s.Hooks.BeforeCommit = func(_, b int64) error {
				if b == k {
					panic("simulated crash inside the batch transaction")
				}
				return nil
			}
		}, func(k int64) int64 { return k }},
		{"right_after_commit", func(s *updater.Service, k int64) {
			s.Hooks.AfterCommit = func(_, b int64) {
				if b == k {
					panic("simulated crash right after commit")
				}
			}
		}, func(k int64) int64 { return k + 1 }},
	}

	for _, p := range points {
		for k := int64(0); k < 4; k++ {
			t.Run(fmt.Sprintf("%s_batch_%d", p.name, k), func(t *testing.T) {
				path := t.TempDir() + "/catalog.db"
				first := openEnv(t, path, supplier.NewMemory())
				first.loadDemo(day1)
				p.hook(first.svc, k)

				job, err := first.svc.CreateJob(bg, day1)
				if err != nil {
					t.Fatal(err)
				}
				crash(t, first, job.ID)
				first.st.Close() // the process is gone

				// ---- application restarts: new store, new service, new supplier connection ----
				second := openEnv(t, path, supplier.NewMemory())
				second.loadDemo(day1)

				after := second.job(job.ID)
				if after.BatchesDone != p.fresh(k) {
					t.Fatalf("already committed work must survive the crash: batches_done = %d, want %d", after.BatchesDone, p.fresh(k))
				}

				recovered, err := second.svc.Recover(bg)
				if err != nil {
					t.Fatal(err)
				}
				after = second.job(job.ID)
				if after.Status == store.JobCompleted {
					// The crash happened after the supplier's final batch was committed.
					if recovered != 0 || p.fresh(k) != 4 {
						t.Fatalf("unexpected completed state: recovered=%d %+v", recovered, after)
					}
				} else {
					if recovered != 1 || after.Status != store.JobInterrupted || !strings.Contains(after.LastError, "application stopped") {
						t.Fatalf("recovery must mark the job interrupted: recovered=%d %+v", recovered, after)
					}
					if after.SupplierDone || after.FinishedAt != nil {
						t.Fatalf("an unfinished job must not look finished: %+v", after)
					}
					if after, err = second.resume(job.ID); err != nil {
						t.Fatalf("resume after restart: %v", err)
					}
				}

				if after.Status != store.JobCompleted || after.Counters != refJob.Counters {
					t.Fatalf("final job differs from an uninterrupted run:\n got %+v\nwant %+v", after.Counters, refJob.Counters)
				}
				if second.dump() != ref.dump() {
					t.Fatalf("catalog differs from an uninterrupted run:\n%s\nvs\n%s", second.dump(), ref.dump())
				}
				if !reflect.DeepEqual(second.history(job.ID), ref.history(refJob.ID)) {
					t.Fatalf("history differs (duplicate or missing entries):\n%v\nvs\n%v", second.history(job.ID), ref.history(refJob.ID))
				}
				if n := second.openIssues(job.ID); n != 5 {
					t.Fatalf("issues must not be duplicated or lost; open = %d", n)
				}
			})
		}
	}
}

func TestShutdownInterruptsARunningUpdateAndItCanBeResumed(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	started := make(chan struct{}, 1)
	e.src.BeforeFetch(func(ctx context.Context, feed, cursor string) error {
		if cursor == "1" { // block on the second batch until shut down
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})

	job, err := e.svc.Start(bg, day1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached batch 1")
	}

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	got := e.job(job.ID)
	if got.Status != store.JobInterrupted || got.BatchesDone != 1 || !strings.Contains(got.LastError, "shutting down") {
		t.Fatalf("job after shutdown: %+v", got)
	}

	// New process, same database.
	e2 := openEnv(t, e.path, supplier.NewMemory())
	e2.loadDemo(day1)
	if n, _ := e2.svc.Recover(bg); n != 0 {
		t.Fatalf("a gracefully interrupted job needs no crash recovery; got %d", n)
	}
	done, err := e2.resume(job.ID)
	if err != nil || done.Status != store.JobCompleted || done.Counters.Received != 22 {
		t.Fatalf("resume: %+v err %v", done, err)
	}
}

// ---------------------------------------------------------------------------
// Job rules and concurrency
// ---------------------------------------------------------------------------

func TestJobLifecycleRules(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)

	if _, err := e.svc.CreateJob(bg, "../etc/passwd"); !errors.Is(err, updater.ErrInvalidFeed) {
		t.Fatalf("bad feed names must be rejected: %v", err)
	}

	e.src.OutageFrom(1)
	job, _ := e.run(day1)

	if _, err := e.svc.CreateJob(bg, day1); !errors.Is(err, store.ErrOpenJobExists) {
		t.Fatalf("a second update for the same feed must be refused while one is unfinished: %v", err)
	}
	if _, err := e.svc.Resume(bg, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}

	// Cancelling abandons it: it stays "cancelled" and is never "completed".
	cancelled, err := e.svc.Cancel(bg, job.ID)
	if err != nil || cancelled.Status != store.JobCancelled || cancelled.SupplierDone || cancelled.FinishedAt == nil {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if _, err := e.svc.Resume(bg, job.ID); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("a cancelled update cannot be resumed: %v", err)
	}

	// A fresh update is now possible and completes; the old one stays cancelled.
	e.src.ClearOutage()
	fresh, err := e.run(day1)
	if err != nil || fresh.Status != store.JobCompleted {
		t.Fatalf("fresh job: %+v %v", fresh, err)
	}
	if _, err := e.svc.Resume(bg, fresh.ID); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("a completed update cannot be resumed: %v", err)
	}
	if _, err := e.svc.Cancel(bg, fresh.ID); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("a completed update cannot be cancelled: %v", err)
	}
	if e.job(job.ID).Status != store.JobCancelled {
		t.Fatal("cancelled job changed state")
	}
}

func TestBackgroundStartResumeAndConcurrentRequests(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	e.src.OutageFrom(2)

	// Eight staff members click "start" at the same moment: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 8)
	var winner store.Job
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := e.svc.Start(bg, day1)
			if err == nil {
				mu.Lock()
				winner = j
				mu.Unlock()
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrOpenJobExists):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d updates started, want exactly 1", wins)
	}

	waitFor(t, "job to be interrupted by the outage", func() bool { return e.job(winner.ID).Status == store.JobInterrupted })

	// Readers keep working while the writer resumes the job.
	e.src.ClearOutage()
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := e.st.ListProducts(bg, "", 100, 0); err != nil {
					t.Errorf("read during import: %v", err)
					return
				}
				if _, err := e.st.GetJob(bg, winner.ID); err != nil {
					t.Errorf("read job during import: %v", err)
					return
				}
			}
		}()
	}

	// Resuming twice at once: one succeeds, the other is told the job is running or finished.
	var resumeWins int
	var rmu sync.Mutex
	var rwg sync.WaitGroup
	for i := 0; i < 4; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			if _, err := e.svc.Resume(bg, winner.ID); err == nil {
				rmu.Lock()
				resumeWins++
				rmu.Unlock()
			} else if !errors.Is(err, store.ErrInvalidState) {
				t.Errorf("unexpected resume error: %v", err)
			}
		}()
	}
	rwg.Wait()
	if resumeWins != 1 {
		t.Fatalf("%d resumes succeeded, want exactly 1", resumeWins)
	}

	waitFor(t, "job to complete", func() bool { return e.job(winner.ID).Status == store.JobCompleted })
	close(stop)
	readers.Wait()

	done := e.job(winner.ID)
	if done.Counters.Received != 22 || done.Resumes != 1 {
		t.Fatalf("job: %+v", done)
	}
}

func TestEmptyFeedCompletesImmediately(t *testing.T) {
	e := newEnv(t)
	e.src.SetFeed("empty")
	job, err := e.run("empty")
	if err != nil || job.Status != store.JobCompleted || job.Counters.Received != 0 {
		t.Fatalf("job %+v err %v", job, err)
	}
}

func TestRepeatedProblemsAreOneOpenIssueNotOnePerImport(t *testing.T) {
	e := newEnv(t)
	e.loadDemo(day1)
	first, _ := e.run(day1)
	second, _ := e.run(day1) // the same feed again: the same 4 invalid records and 1 conflict

	if n := e.openIssues(0); n != 5 {
		t.Fatalf("staff must see 5 open issues, not one set per import; got %d", n)
	}
	if n := e.openIssues(first.ID); n != 5 {
		t.Fatalf("first update ran into 5 problems, got %d", n)
	}
	if n := e.openIssues(second.ID); n != 5 {
		t.Fatalf("the second update ran into the same 5 problems, got %d", n)
	}
	issues, _ := e.st.ListIssues(bg, store.IssueFilter{Status: "open"})
	if len(issues) != 5 {
		t.Fatalf("open issues listed: %d", len(issues))
	}
	for _, i := range issues {
		if i.Occurrences != 2 || i.JobID != first.ID {
			t.Errorf("issue %d should be first seen by update %d and seen twice: %+v", i.ID, first.ID, i)
		}
	}
	// The job-level counters still describe what each import itself saw.
	if second.Counters.Invalid != 4 || second.Counters.Conflicts != 1 {
		t.Fatalf("counters: %+v", second.Counters)
	}

	// Once staff resolve a problem, the same record appearing again needs attention again.
	if _, err := e.st.ResolveIssue(bg, issues[0].ID, "checked", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := e.openIssues(0); n != 4 {
		t.Fatalf("after resolving one: %d", n)
	}
	e.run(day1)
	if n := e.openIssues(0); n != 5 {
		t.Fatalf("a resolved problem that comes back must reopen as a new issue; open = %d", n)
	}
}
