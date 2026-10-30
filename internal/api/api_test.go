package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"catalogsync/internal/api"
	"catalogsync/internal/store"
	"catalogsync/internal/supplier"
	"catalogsync/internal/updater"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	src *supplier.Memory
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := supplier.NewMemory()
	for _, name := range []string{"day1-initial", "day2-changes"} {
		f, err := supplier.ReadFeedFile(filepath.Join("../../data/feeds", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		src.SetFeedBatches(name, f.Batches)
	}
	svc := updater.New(st, src, updater.Config{MaxFetchAttempts: 1})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.New(st, svc, log))
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		svc.Shutdown(ctx)
		st.Close()
	})
	return &harness{t: t, srv: srv, src: src}
}

// call performs a request and decodes the JSON response into a generic map.
func (h *harness) call(method, path string, body any) (int, map[string]any, http.Header) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func (h *harness) waitStatus(id, want string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, job, _ := h.call("GET", "/v1/updates/"+id, nil)
		if job["status"] == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("update %s never reached %q", id, want)
	return nil
}

func TestStartUpdateAndWatchItComplete(t *testing.T) {
	h := newHarness(t)
	code, job, hdr := h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	if code != http.StatusAccepted || hdr.Get("Location") != "/v1/updates/1" {
		t.Fatalf("start: %d %v %v", code, job, hdr)
	}

	done := h.waitStatus("1", "completed")
	if done["complete"] != true || done["needs_attention"] != true || done["open_issues"].(float64) != 5 {
		t.Fatalf("job: %v", done)
	}
	if !strings.Contains(done["summary"].(string), "Completed") || !strings.Contains(done["summary"].(string), "5 open issue") {
		t.Fatalf("summary: %v", done["summary"])
	}
	counters := done["counters"].(map[string]any)
	if counters["received"].(float64) != 22 || counters["created"].(float64) != 10 {
		t.Fatalf("counters: %v", counters)
	}

	// What changed
	_, changes, _ := h.call("GET", "/v1/updates/1/changes?limit=500", nil)
	if changes["count"].(float64) != 13 {
		t.Fatalf("changes: %v", changes["count"])
	}
	// Which problems need attention
	_, issues, _ := h.call("GET", "/v1/updates/1/issues?status=open", nil)
	if issues["count"].(float64) != 5 {
		t.Fatalf("issues: %v", issues["count"])
	}
}

func TestInterruptedUpdateIsClearlyNotCompleteAndCanBeResumed(t *testing.T) {
	h := newHarness(t)
	h.src.OutageFrom(2)
	h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	job := h.waitStatus("1", "interrupted")

	if job["complete"] != false || job["needs_attention"] != true || job["batches_done"].(float64) != 2 ||
		!strings.Contains(job["summary"].(string), "NOT COMPLETE") || !strings.Contains(job["next_action"].(string), "/resume") {
		t.Fatalf("job: %v", job)
	}

	// The same feed cannot be started again meanwhile; the response names the blocking update.
	code, body, _ := h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	if code != http.StatusConflict || body["existing_update_id"].(float64) != 1 || body["code"] != "update_already_open" {
		t.Fatalf("second start: %d %v", code, body)
	}

	// Products from the completed batches are already there.
	_, summary, _ := h.call("GET", "/v1/catalog/summary", nil)
	if summary["products"].(map[string]any)["active"].(float64) != 7 {
		t.Fatalf("summary: %v", summary)
	}

	h.src.ClearOutage()
	code, _, _ = h.call("POST", "/v1/updates/1/resume", nil)
	if code != http.StatusAccepted {
		t.Fatalf("resume: %d", code)
	}
	done := h.waitStatus("1", "completed")
	if done["complete"] != true || done["resumes"].(float64) != 1 {
		t.Fatalf("job: %v", done)
	}
	if code, _, _ = h.call("POST", "/v1/updates/1/resume", nil); code != http.StatusConflict {
		t.Fatalf("resuming a completed update must be refused: %d", code)
	}
}

func TestCancelledUpdateIsNeverComplete(t *testing.T) {
	h := newHarness(t)
	h.src.OutageFrom(1)
	h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	h.waitStatus("1", "interrupted")

	code, job, _ := h.call("POST", "/v1/updates/1/cancel", nil)
	if code != http.StatusOK || job["status"] != "cancelled" || job["complete"] != false ||
		!strings.Contains(job["summary"].(string), "NOT COMPLETE") {
		t.Fatalf("cancel: %d %v", code, job)
	}
	if code, _, _ = h.call("POST", "/v1/updates/1/cancel", nil); code != http.StatusConflict {
		t.Fatalf("second cancel: %d", code)
	}
}

func TestProductsAndHistory(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	h.waitStatus("1", "completed")
	h.call("POST", "/v1/updates", map[string]string{"feed": "day2-changes"})
	h.waitStatus("2", "completed")

	_, active, _ := h.call("GET", "/v1/products", nil) // default: what is on sale
	if active["count"].(float64) != 11 {
		t.Fatalf("active: %v", active["count"])
	}
	_, withdrawn, _ := h.call("GET", "/v1/products?status=withdrawn", nil)
	if withdrawn["count"].(float64) != 2 {
		t.Fatalf("withdrawn: %v", withdrawn["count"])
	}
	_, all, _ := h.call("GET", "/v1/products?status=all&limit=5&offset=10", nil)
	if all["count"].(float64) != 3 {
		t.Fatalf("paging: %v", all["count"])
	}

	// The lookup normalises the SKU like the importer does.
	code, p, _ := h.call("GET", "/v1/products/hp-1008", nil)
	if code != 200 || p["status"] != "active" || p["version"].(float64) != 4 {
		t.Fatalf("product: %d %v", code, p)
	}
	_, hist, _ := h.call("GET", "/v1/products/HP-1008/history", nil)
	items := hist["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["change_type"] != "withdrawn" || items[1].(map[string]any)["change_type"] != "relisted" {
		t.Fatalf("history: %v", items)
	}
}

func TestIssuesCanBeResolved(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/v1/updates", map[string]string{"feed": "day1-initial"})
	h.waitStatus("1", "completed")

	_, open, _ := h.call("GET", "/v1/issues?status=open&kind=version_conflict", nil)
	if open["count"].(float64) != 1 {
		t.Fatalf("conflicts: %v", open)
	}
	id := open["items"].([]any)[0].(map[string]any)["id"].(float64)
	code, issue, _ := h.call("POST", "/v1/issues/"+jsonNum(id)+"/resolve", map[string]string{"note": "supplier confirmed 2200 is right"})
	if code != 200 || issue["status"] != "resolved" {
		t.Fatalf("resolve: %d %v", code, issue)
	}
	_, job, _ := h.call("GET", "/v1/updates/1", nil)
	if job["open_issues"].(float64) != 4 {
		t.Fatalf("open issues: %v", job["open_issues"])
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(int64(f)); return string(b) }

func TestErrorResponses(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"unknown update", "GET", "/v1/updates/99", nil, 404},
		{"bad id", "GET", "/v1/updates/abc", nil, 400},
		{"resume unknown", "POST", "/v1/updates/99/resume", nil, 404},
		{"unknown product", "GET", "/v1/products/NOPE", nil, 404},
		{"unknown product history", "GET", "/v1/products/NOPE/history", nil, 404},
		{"bad feed name", "POST", "/v1/updates", map[string]string{"feed": "../x"}, 400},
		{"missing feed", "POST", "/v1/updates", map[string]string{}, 400},
		{"unknown json field", "POST", "/v1/updates", map[string]string{"feeed": "x"}, 400},
		{"bad product status", "GET", "/v1/products?status=weird", nil, 400},
		{"bad issue status", "GET", "/v1/issues?status=weird", nil, 400},
		{"resolve unknown issue", "POST", "/v1/issues/99/resolve", nil, 404},
		{"health", "GET", "/healthz", nil, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := h.call(tc.method, tc.path, tc.body)
			if code != tc.want {
				t.Fatalf("got %d (%v), want %d", code, body, tc.want)
			}
		})
	}
}

func TestUnknownFeedLeavesAnInterruptedUpdateWithAClearReason(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/v1/updates", map[string]string{"feed": "does-not-exist"})
	job := h.waitStatus("1", "interrupted")
	if !strings.Contains(job["last_error"].(string), "not found") {
		t.Fatalf("job: %v", job)
	}
}
