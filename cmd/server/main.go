package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"catalogsync/internal/api"
	"catalogsync/internal/store"
	"catalogsync/internal/supplier"
	"catalogsync/internal/updater"
)

func main() {
	var (
		addr           = flag.String("addr", ":8080", "address to listen on")
		dbPath         = flag.String("db", "catalog.db", "path of the SQLite database file")
		supplierURL    = flag.String("supplier", "http://localhost:9090", "base URL of the supplier")
		attempts       = flag.Int("fetch-attempts", 3, "attempts per batch before an update is interrupted")
		backoff        = flag.Duration("retry-backoff", 500*time.Millisecond, "pause before the first retry (doubles each time)")
		demoCrashAfter = flag.Int("demo-crash-after-batch", 0,
			"DEMO ONLY: kill this process (like kill -9) right after this many batches were committed; 0 = off")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *addr, *dbPath, *supplierURL, *attempts, *backoff, *demoCrashAfter); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr, dbPath, supplierURL string, attempts int, backoff time.Duration, demoCrashAfter int) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	svc := updater.New(st, supplier.NewHTTPSource(supplierURL), updater.Config{
		MaxFetchAttempts: attempts,
		RetryBackoff:     backoff,
		Logger:           log,
	})

	if demoCrashAfter > 0 {
		var committed atomic.Int64
		svc.Hooks.AfterCommit = func(jobID, batchIndex int64) {
			if committed.Add(1) >= int64(demoCrashAfter) {
				log.Warn("DEMO: simulating a crash (no cleanup, like kill -9)", "job", jobID, "batches_committed_in_this_process", demoCrashAfter)
				os.Exit(137)
			}
		}
	}

	// Jobs still "running" in the database belong to a process that died.
	if _, err := svc.Recover(context.Background()); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(st, svc, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "db", dbPath, "supplier", supplierURL)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down: finishing the current batch of each running update")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	// Running updates stop cleanly and become "interrupted" so they can be resumed.
	return svc.Shutdown(shutdownCtx)
}
