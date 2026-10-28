// Command catalogsync runs the staff web app that keeps a local copy of the
// (fictional) supplier catalog up to date.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // lets TZ work in minimal container images

	"catalogsync/internal/adapter/sqlite"
	"catalogsync/internal/adapter/supplier/mock"
	"catalogsync/internal/adapter/web"
	"catalogsync/internal/usecase"
)

type config struct {
	addr       string
	dbPath     string
	batchDelay time.Duration
	outageAt   int
	crashAt    string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:8080", "HTTP listen address")
	flag.StringVar(&cfg.dbPath, "db", "catalog.db", "SQLite database file")
	flag.DurationVar(&cfg.batchDelay, "batch-delay", time.Second, "pause between batches so progress is visible")
	flag.IntVar(&cfg.outageAt, "outage-at", 0, "simulate a supplier outage from this batch number (0 = online)")
	flag.StringVar(&cfg.crashAt, "crash-at", "", `kill the process mid-batch, e.g. "2:3" = batch 2, after record 3 (demo only)`)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(cfg, logger); err != nil {
		logger.Error("catalogsync stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfg config, logger *slog.Logger) error {
	// SIGTERM is what container platforms send before stopping a machine.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := sqlite.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	feeds, err := mock.DefaultFeeds()
	if err != nil {
		return err
	}
	supplier := mock.New(feeds...)
	supplier.SetOutage(cfg.outageAt)

	hook, err := crashHook(cfg.crashAt, logger)
	if err != nil {
		return err
	}
	syncSvc := usecase.NewSyncService(store, supplier, time.Now, usecase.SyncOptions{
		BatchDelay: cfg.batchDelay, FaultHook: hook, Logger: logger,
	})
	if n, err := syncSvc.RecoverInterrupted(ctx); err != nil {
		return err
	} else if n > 0 {
		logger.Warn("found updates interrupted by a previous shutdown; they can be resumed", "count", n)
	}

	srv, err := web.NewServer(syncSvc,
		usecase.NewQueryService(store, supplier),
		usecase.NewIssueService(store, time.Now),
		supplier, logger)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: cfg.addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "url", "http://"+cfg.addr)
	err = httpSrv.ListenAndServe()
	// Running updates are saved as INTERRUPTED so staff can resume them.
	syncSvc.Shutdown()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// crashHook simulates a hard crash (no cleanup at all) in the middle of a
// batch transaction, to demonstrate that half-applied batches are discarded.
func crashHook(spec string, logger *slog.Logger) (usecase.FaultHook, error) {
	if spec == "" {
		return nil, nil
	}
	var batchNo, recordNo int
	if _, err := fmt.Sscanf(spec, "%d:%d", &batchNo, &recordNo); err != nil {
		return nil, fmt.Errorf("invalid -crash-at %q, want BATCH:RECORD", spec)
	}
	return func(b, r int) {
		if b == batchNo && r == recordNo {
			logger.Error("simulated crash", "batch", b, "record", r)
			os.Exit(3)
		}
	}, nil
}
