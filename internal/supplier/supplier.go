
package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Batch struct {
	Records []json.RawMessage `json:"records"`
	// NextCursor is an opaque token for the next batch. We store it to resume.
	NextCursor string `json:"next_cursor"`
	// Done is true only on the final batch of the feed.
	Done bool `json:"done"`
}

// Source is anything that can deliver supplier batches.
// cursor is "" for the first batch, otherwise the NextCursor of the previous one.
type Source interface {
	FetchBatch(ctx context.Context, feed, cursor string) (Batch, error)
}

// ErrFeedNotFound means the supplier does not know the feed. Retrying will not help.
var ErrFeedNotFound = errors.New("supplier feed not found")

// HTTPSource fetches batches from a supplier over HTTP:
//
//	GET {BaseURL}/v1/feeds/{feed}/batches?cursor={cursor}
type HTTPSource struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPSource returns an HTTPSource with a sensible request timeout.
func NewHTTPSource(baseURL string) *HTTPSource {
	return &HTTPSource{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

const maxBatchBytes = 20 << 20 // refuse absurdly large responses

// FetchBatch implements Source.
func (s *HTTPSource) FetchBatch(ctx context.Context, feed, cursor string) (Batch, error) {
	u := fmt.Sprintf("%s/v1/feeds/%s/batches?cursor=%s", s.BaseURL, url.PathEscape(feed), url.QueryEscape(cursor))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Batch{}, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return Batch{}, fmt.Errorf("supplier request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Batch{}, fmt.Errorf("%w: %q", ErrFeedNotFound, feed)
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return Batch{}, fmt.Errorf("supplier returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var b Batch
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBatchBytes))
	if err := dec.Decode(&b); err != nil {
		// A truncated body (connection dropped mid-response) lands here, so a
		// half-delivered batch is never mistaken for a complete one.
		return Batch{}, fmt.Errorf("supplier sent an unreadable batch: %w", err)
	}
	return b, nil
}
