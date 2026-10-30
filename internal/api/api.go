// Package api exposes the update service and the catalog over HTTP (JSON).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"catalogsync/internal/catalog"
	"catalogsync/internal/store"
	"catalogsync/internal/updater"
)

// API holds the dependencies of the HTTP handlers.
type API struct {
	store *store.Store
	svc   *updater.Service
	log   *slog.Logger
	now   func() time.Time
}

// New returns the HTTP handler for the whole API.
func New(st *store.Store, svc *updater.Service, log *slog.Logger) http.Handler {
	a := &API{store: st, svc: svc, log: log, now: time.Now}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Updates (jobs)
	mux.HandleFunc("POST /v1/updates", a.startUpdate)
	mux.HandleFunc("GET /v1/updates", a.listUpdates)
	mux.HandleFunc("GET /v1/updates/{id}", a.getUpdate)
	mux.HandleFunc("POST /v1/updates/{id}/resume", a.resumeUpdate)
	mux.HandleFunc("POST /v1/updates/{id}/cancel", a.cancelUpdate)
	mux.HandleFunc("GET /v1/updates/{id}/changes", a.updateChanges)
	mux.HandleFunc("GET /v1/updates/{id}/issues", a.updateIssues)

	// Issues
	mux.HandleFunc("GET /v1/issues", a.listIssues)
	mux.HandleFunc("POST /v1/issues/{id}/resolve", a.resolveIssue)

	// Catalog
	mux.HandleFunc("GET /v1/catalog/summary", a.catalogSummary)
	mux.HandleFunc("GET /v1/products", a.listProducts)
	mux.HandleFunc("GET /v1/products/{sku}", a.getProduct)
	mux.HandleFunc("GET /v1/products/{sku}/history", a.productHistory)

	return a.logRequests(mux)
}

// ---------------------------------------------------------------------------
// Updates
// ---------------------------------------------------------------------------

// jobView is a job as staff see it: the stored fields plus what they need to
// decide what to do next.
type jobView struct {
	store.Job
	// Complete is true only when the supplier's final batch was applied.
	Complete       bool   `json:"complete"`
	OpenIssues     int64  `json:"open_issues"`
	NeedsAttention bool   `json:"needs_attention"`
	Summary        string `json:"summary"`
	NextAction     string `json:"next_action,omitempty"`
}

func (a *API) view(r *http.Request, j store.Job) (jobView, error) {
	open, err := a.store.CountOpenIssues(r.Context(), j.ID)
	if err != nil {
		return jobView{}, err
	}
	v := jobView{Job: j, Complete: j.Status == store.JobCompleted, OpenIssues: open}
	c := j.Counters
	switch j.Status {
	case store.JobRunning:
		v.Summary = fmt.Sprintf("Running: %d batch(es) applied so far, %d record(s) processed.", j.BatchesDone, c.Received)
	case store.JobInterrupted:
		v.Summary = fmt.Sprintf("NOT COMPLETE. Interrupted after %d batch(es): %s", j.BatchesDone, j.LastError)
		v.NextAction = fmt.Sprintf("POST /v1/updates/%d/resume (or /cancel)", j.ID)
	case store.JobCancelled:
		v.Summary = fmt.Sprintf("NOT COMPLETE. Cancelled by staff after %d batch(es).", j.BatchesDone)
	case store.JobCompleted:
		v.Summary = fmt.Sprintf("Completed: %d record(s) in %d batch(es): %d created, %d updated, %d withdrawn, %d relisted, %d unchanged, %d repeated, %d stale (ignored), %d invalid, %d conflicts.",
			c.Received, j.BatchesDone, c.Created, c.Updated, c.Withdrawn, c.Relisted, c.Unchanged, c.Duplicates, c.Stale, c.Invalid, c.Conflicts)
	}
	if open > 0 {
		v.Summary += fmt.Sprintf(" %d open issue(s) need attention.", open)
		if v.NextAction == "" {
			v.NextAction = fmt.Sprintf("GET /v1/updates/%d/issues?status=open", j.ID)
		}
	}
	v.NeedsAttention = j.Status == store.JobInterrupted || open > 0
	return v, nil
}

func (a *API) startUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Feed string `json:"feed"`
	}
	if !decodeBody(w, r, &body, false) {
		return
	}
	job, err := a.svc.Start(r.Context(), body.Feed)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.view(r, job)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v1/updates/%d", job.ID))
	writeJSON(w, http.StatusAccepted, v) // processing continues in the background
}

func (a *API) listUpdates(w http.ResponseWriter, r *http.Request) {
	limit, offset := paging(r)
	jobs, err := a.store.ListJobs(r.Context(), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		v, err := a.view(r, j)
		if err != nil {
			a.fail(w, err)
			return
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, list(views, limit, offset))
}

func (a *API) getUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := a.store.GetJob(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.view(r, job)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) resumeUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := a.svc.Resume(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.view(r, job)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (a *API) cancelUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := a.svc.Cancel(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.view(r, job)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) updateChanges(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.store.GetJob(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	limit, offset := paging(r)
	changes, err := a.store.ListJobChanges(r.Context(), id, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list(changes, limit, offset))
}

func (a *API) updateIssues(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.store.GetJob(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.issues(w, r, id)
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func (a *API) listIssues(w http.ResponseWriter, r *http.Request) { a.issues(w, r, 0) }

func (a *API) issues(w http.ResponseWriter, r *http.Request, jobID int64) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && status != "open" && status != "resolved" {
		writeError(w, http.StatusBadRequest, "bad_request", `status must be "open" or "resolved"`)
		return
	}
	limit, offset := paging(r)
	items, err := a.store.ListIssues(r.Context(), store.IssueFilter{
		JobID: jobID, Status: status, Kind: q.Get("kind"), Limit: limit, Offset: offset,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items, limit, offset))
}

func (a *API) resolveIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeBody(w, r, &body, true) {
		return
	}
	issue, err := a.store.ResolveIssue(r.Context(), id, strings.TrimSpace(body.Note), a.now())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

func (a *API) catalogSummary(w http.ResponseWriter, r *http.Request) {
	products, err := a.store.ProductCounts(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	jobs, err := a.store.JobSummary(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	open, err := a.store.CountOpenIssues(r.Context(), 0)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"products":    products,
		"updates":     jobs,
		"open_issues": open,
	})
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		status = "active" // the shop's normal view: what is on sale
	case "all":
		status = ""
	case "active", "withdrawn":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", `status must be "active", "withdrawn" or "all"`)
		return
	}
	limit, offset := paging(r)
	items, err := a.store.ListProducts(r.Context(), status, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items, limit, offset))
}

func (a *API) getProduct(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetProduct(r.Context(), catalog.NormalizeSKU(r.PathValue("sku")))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) productHistory(w http.ResponseWriter, r *http.Request) {
	sku := catalog.NormalizeSKU(r.PathValue("sku"))
	if _, err := a.store.GetProduct(r.Context(), sku); err != nil {
		a.fail(w, err)
		return
	}
	limit, offset := paging(r)
	items, err := a.store.ListProductChanges(r.Context(), sku, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items, limit, offset))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type errorBody struct {
	Error         string `json:"error"`
	Code          string `json:"code"`
	ExistingJobID int64  `json:"existing_update_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// fail maps domain errors to HTTP statuses. Unknown errors become a generic
// 500: details go to the log, not to the client.
func (a *API) fail(w http.ResponseWriter, err error) {
	var open *store.OpenJobError
	switch {
	case errors.As(err, &open):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error(), Code: "update_already_open", ExistingJobID: open.JobID})
	case errors.Is(err, store.ErrOpenJobExists):
		writeError(w, http.StatusConflict, "update_already_open", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, updater.ErrInvalidFeed):
		writeError(w, http.StatusBadRequest, "invalid_feed", err.Error())
	case errors.Is(err, updater.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "shutting_down", err.Error())
	default:
		a.log.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeBody reads a small JSON body strictly (unknown fields are rejected).
// If optional is true an empty body is accepted.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func paging(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// list wraps a slice in the common list envelope; nil becomes [] in JSON.
func list[T any](items []T, limit, offset int) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items, "count": len(items), "limit": limit, "offset": offset}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		a.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}
