package supplier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPSourceDecodesABatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/feeds/my feed/batches" || r.URL.Query().Get("cursor") != "3" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		w.Write([]byte(`{"records":[{"sku":"A"},{"sku":"B"}],"next_cursor":"4","done":false}`))
	}))
	defer srv.Close()

	b, err := NewHTTPSource(srv.URL).FetchBatch(context.Background(), "my feed", "3")
	if err != nil || len(b.Records) != 2 || b.NextCursor != "4" || b.Done {
		t.Fatalf("batch %+v err %v", b, err)
	}
}

func TestHTTPSourceErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		check   func(error) bool
	}{
		{"unavailable", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) },
			func(err error) bool { return err != nil && !errors.Is(err, ErrFeedNotFound) }},
		{"unknown feed", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
			func(err error) bool { return errors.Is(err, ErrFeedNotFound) }},
		{"not json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>oops</html>")) },
			func(err error) bool { return err != nil }},
		{"truncated body is never a complete batch", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"records":[{"sku":"A"},{"sku":"B"`)) // connection dropped mid-response
		}, func(err error) bool { return err != nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if _, err := NewHTTPSource(srv.URL).FetchBatch(context.Background(), "f", ""); !tc.check(err) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHTTPSourceHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewHTTPSource(srv.URL).FetchBatch(ctx, "f", ""); err == nil {
		t.Fatal("expected an error when the context expires")
	}
}

func TestHTTPSourceConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more: a real outage
	if _, err := NewHTTPSource(url).FetchBatch(context.Background(), "f", ""); err == nil {
		t.Fatal("expected an error")
	}
}
