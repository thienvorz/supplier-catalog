package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"catalogsync/internal/supplier"
)

type server struct {
	feeds map[string]supplier.FeedFile

	mu         sync.Mutex
	outageFrom int // -1 = no outage
}

func main() {
	addr := flag.String("addr", ":9090", "address to listen on")
	dir := flag.String("feeds", "data/feeds", "directory with feed files (*.json)")
	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*dir, "*.json"))
	if err != nil || len(files) == 0 {
		log.Fatalf("no feed files found in %q", *dir)
	}
	s := &server{feeds: map[string]supplier.FeedFile{}, outageFrom: -1}
	for _, f := range files {
		feed, err := supplier.ReadFeedFile(f)
		if err != nil {
			log.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		s.feeds[name] = feed
		log.Printf("loaded feed %q: %d batches", name, len(feed.Batches))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/feeds/{feed}/batches", s.batch)
	mux.HandleFunc("POST /admin/outage", s.startOutage)
	mux.HandleFunc("DELETE /admin/outage", s.endOutage)
	log.Printf("fictional supplier listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) batch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("feed")
	feed, ok := s.feeds[name]
	if !ok {
		http.Error(w, "unknown feed", http.StatusNotFound)
		return
	}
	idx := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		idx = n
	}

	s.mu.Lock()
	down := s.outageFrom >= 0 && idx >= s.outageFrom
	s.mu.Unlock()
	if down {
		log.Printf("feed=%s batch=%d -> 503 (simulated outage)", name, idx)
		http.Error(w, "supplier temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	out := supplier.Batch{Records: []json.RawMessage{}}
	switch {
	case len(feed.Batches) == 0:
		out.Done = true
	case idx >= len(feed.Batches):
		http.Error(w, "cursor past the end of the feed", http.StatusBadRequest)
		return
	default:
		out.Records = feed.Batches[idx]
		if idx == len(feed.Batches)-1 {
			out.Done = true
		} else {
			out.NextCursor = strconv.Itoa(idx + 1)
		}
	}
	log.Printf("feed=%s batch=%d -> %d records (done=%v)", name, idx, len(out.Records), out.Done)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) startOutage(w http.ResponseWriter, r *http.Request) {
	from, err := strconv.Atoi(r.URL.Query().Get("from_batch"))
	if err != nil || from < 0 {
		http.Error(w, "from_batch must be a non-negative integer", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.outageFrom = from
	s.mu.Unlock()
	log.Printf("OUTAGE ON: batches >= %d now fail", from)
	fmt.Fprintf(w, "outage on from batch %d\n", from)
}

func (s *server) endOutage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.outageFrom = -1
	s.mu.Unlock()
	log.Print("OUTAGE OFF")
	fmt.Fprintln(w, "outage off")
}
