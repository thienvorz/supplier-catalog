package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// ErrSimulatedOutage is what Memory returns while an outage is switched on.
var ErrSimulatedOutage = errors.New("simulated supplier outage")

// Memory is an in-memory Source with fault injection. Tests use it so they
// never depend on a real supplier. Cursors are batch numbers ("0", "1", ...).
type Memory struct {
	mu          sync.Mutex
	feeds       map[string][][]json.RawMessage
	outageFrom  int // batches with index >= outageFrom fail; -1 means no outage
	failNext    int // the next N calls fail, whatever the batch
	calls       []string
	beforeFetch func(ctx context.Context, feed, cursor string) error
}

// NewMemory returns an empty Memory source.
func NewMemory() *Memory { return &Memory{feeds: map[string][][]json.RawMessage{}, outageFrom: -1} }

// SetFeed defines a feed. Each element of batches is one batch of raw JSON records.
func (m *Memory) SetFeed(feed string, batches ...[]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]json.RawMessage
	for _, b := range batches {
		recs := make([]json.RawMessage, 0, len(b))
		for _, r := range b {
			recs = append(recs, json.RawMessage(r))
		}
		out = append(out, recs)
	}
	m.feeds[feed] = out
}

// SetFeedBatches defines a feed from already-decoded batches (for example a FeedFile).
func (m *Memory) SetFeedBatches(feed string, batches [][]json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feeds[feed] = batches
}

// OutageFrom makes every batch with index >= n fail until ClearOutage is called.
func (m *Memory) OutageFrom(n int) { m.mu.Lock(); m.outageFrom = n; m.mu.Unlock() }

// ClearOutage ends a simulated outage.
func (m *Memory) ClearOutage() { m.mu.Lock(); m.outageFrom = -1; m.mu.Unlock() }

// FailNext makes the next n calls fail (a transient glitch that retries survive).
func (m *Memory) FailNext(n int) { m.mu.Lock(); m.failNext = n; m.mu.Unlock() }

// BeforeFetch installs a hook that runs at the start of every call. If it
// returns an error, that error is returned to the caller.
func (m *Memory) BeforeFetch(fn func(ctx context.Context, feed, cursor string) error) {
	m.mu.Lock()
	m.beforeFetch = fn
	m.mu.Unlock()
}

// Calls returns every "feed@cursor" requested so far, in order.
func (m *Memory) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// ResetCalls forgets the recorded calls.
func (m *Memory) ResetCalls() { m.mu.Lock(); m.calls = nil; m.mu.Unlock() }

// FetchBatch implements Source.
func (m *Memory) FetchBatch(ctx context.Context, feed, cursor string) (Batch, error) {
	m.mu.Lock()
	hook := m.beforeFetch
	m.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, feed, cursor); err != nil {
			return Batch{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, feed+"@"+cursor)

	if err := ctx.Err(); err != nil {
		return Batch{}, err
	}
	batches, ok := m.feeds[feed]
	if !ok {
		return Batch{}, fmt.Errorf("%w: %q", ErrFeedNotFound, feed)
	}
	idx := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return Batch{}, fmt.Errorf("bad cursor %q", cursor)
		}
		idx = n
	}
	if m.failNext > 0 {
		m.failNext--
		return Batch{}, ErrSimulatedOutage
	}
	if m.outageFrom >= 0 && idx >= m.outageFrom {
		return Batch{}, ErrSimulatedOutage
	}
	if len(batches) == 0 {
		return Batch{Done: true}, nil
	}
	if idx >= len(batches) {
		return Batch{}, fmt.Errorf("cursor %q is past the end of feed %q", cursor, feed)
	}
	b := Batch{Records: batches[idx]}
	if idx == len(batches)-1 {
		b.Done = true
	} else {
		b.NextCursor = strconv.Itoa(idx + 1)
	}
	return b, nil
}
