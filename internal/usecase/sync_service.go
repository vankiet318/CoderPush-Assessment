package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"catalogsync/internal/domain"
)

// FaultHook runs after each record is written inside a batch transaction,
// before the commit. Tests and demos use it to simulate a crash mid-batch.
type FaultHook func(batchNo, recordNo int)

type SyncOptions struct {
	// BatchDelay pauses between batches so staff can watch progress.
	BatchDelay time.Duration
	FaultHook  FaultHook
	Logger     *slog.Logger
}

// SyncService runs catalog updates in the background.
//
// Each supplier batch is applied in a single transaction together with the
// run's checkpoint. An interrupted update therefore keeps every batch it
// finished, never keeps half a batch, and resumes from the next batch.
type SyncService struct {
	runs     RunStore
	supplier Supplier
	now      Clock
	opts     SyncOptions

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex // serialises Start and Resume within this process
	wg     sync.WaitGroup
}

func NewSyncService(runs RunStore, supplier Supplier, now Clock, opts SyncOptions) *SyncService {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncService{runs: runs, supplier: supplier, now: now, opts: opts, ctx: ctx, cancel: cancel}
}

// RecoverInterrupted marks updates left RUNNING by a previous process as
// INTERRUPTED so staff can resume them. Call it once at startup.
func (s *SyncService) RecoverInterrupted(ctx context.Context) (int, error) {
	runs, err := s.runs.ListRuns(ctx, 0)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, run := range runs {
		if !run.IsRunning() {
			continue
		}
		if err := run.Interrupt("the application stopped before the update finished", s.now()); err != nil {
			return recovered, err
		}
		if err := s.runs.SaveRun(ctx, run); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func (s *SyncService) StartRun(ctx context.Context, feed string) (domain.ImportRun, error) {
	if err := s.checkFeed(ctx, feed); err != nil {
		return domain.ImportRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.runs.CreateRun(ctx, domain.NewRun(feed, s.now()))
	if err != nil {
		return domain.ImportRun{}, err
	}
	s.launch(run)
	return run, nil
}

// ResumeRun continues a FAILED or INTERRUPTED update from its checkpoint.
func (s *SyncService) ResumeRun(ctx context.Context, id int64) (domain.ImportRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.runs.GetRun(ctx, id)
	if err != nil {
		return domain.ImportRun{}, err
	}
	if err := run.Resume(s.now()); err != nil {
		return domain.ImportRun{}, err
	}
	if err := s.runs.SaveRun(ctx, run); err != nil {
		return domain.ImportRun{}, err
	}
	s.launch(run)
	return run, nil
}

// Wait blocks until every update started by this service has stopped.
func (s *SyncService) Wait() { s.wg.Wait() }

// Shutdown stops running updates; they are saved as INTERRUPTED.
func (s *SyncService) Shutdown() {
	s.cancel()
	s.wg.Wait()
}

func (s *SyncService) checkFeed(ctx context.Context, feed string) error {
	feeds, err := s.supplier.ListFeeds(ctx)
	if err != nil {
		return fmt.Errorf("list supplier updates: %w", err)
	}
	for _, f := range feeds {
		if f.Name == feed {
			return nil
		}
	}
	return fmt.Errorf("%w: supplier update %q", domain.ErrNotFound, feed)
}

func (s *SyncService) launch(run domain.ImportRun) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(s.ctx, run)
	}()
}

func (s *SyncService) execute(ctx context.Context, run domain.ImportRun) {
	log := s.opts.Logger.With("run", run.ID, "feed", run.Feed)
	for run.IsRunning() {
		if ctx.Err() != nil {
			s.stop(run, (*domain.ImportRun).Interrupt, "the application shut down during the update", log)
			return
		}
		batch, err := s.supplier.FetchBatch(ctx, run.Feed, run.Cursor)
		if err != nil {
			s.stopOnError(ctx, run, fmt.Sprintf("supplier error: %v", err), log)
			return
		}
		next, err := s.applyBatch(ctx, run, batch)
		if err != nil {
			s.stopOnError(ctx, run, fmt.Sprintf("could not save batch %s: %v", batch.ID, err), log)
			return
		}
		run = next
		log.Info("batch applied", "batch", batch.ID, "status", run.Status)

		if run.IsRunning() && s.opts.BatchDelay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(s.opts.BatchDelay):
			}
		}
	}
}

// applyBatch writes every record of the batch and the advanced checkpoint in
// one transaction. On error the returned run is the unchanged input.
func (s *SyncService) applyBatch(ctx context.Context, run domain.ImportRun, batch SupplierBatch) (domain.ImportRun, error) {
	next := run
	now := s.now()
	batchNo := run.Counters.Batches + 1
	err := s.runs.InTx(ctx, func(tx CatalogTx) error {
		for i, rec := range batch.Records {
			if err := s.applyRecord(ctx, tx, &next, batch.ID, i, rec, now); err != nil {
				return fmt.Errorf("record %d: %w", i+1, err)
			}
			if s.opts.FaultHook != nil {
				s.opts.FaultHook(batchNo, i+1)
			}
		}
		if err := next.AdvanceBatch(batch.NextCursor, batch.Last, now); err != nil {
			return err
		}
		return tx.SaveRun(ctx, next)
	})
	if err != nil {
		return run, err
	}
	return next, nil
}

func (s *SyncService) applyRecord(ctx context.Context, tx CatalogTx, run *domain.ImportRun, batchID string, index int, rec domain.SupplierRecord, now time.Time) error {
	var stored *domain.Product
	if rec.SKU != "" {
		p, err := tx.GetProduct(ctx, rec.SKU)
		if err != nil {
			return err
		}
		stored = p
	}

	d := domain.Decide(stored, rec)
	run.Counters.Count(d.Kind)

	if d.Kind == domain.DecisionInvalid {
		_, err := tx.AddIssueIfNew(ctx, domain.NewInvalidRecordIssue(run.ID, run.Feed, batchID, index, rec, d.Reason, now))
		return err
	}
	if d.Save {
		if err := tx.SaveProduct(ctx, d.Product, run.ID, now); err != nil {
			return err
		}
	}
	if d.Kind.Reportable() {
		return tx.AddChange(ctx, domain.Change{
			RunID: run.ID, BatchID: batchID, SKU: rec.SKU, Kind: d.Kind, Fields: d.Changes, Reason: d.Reason, At: now,
		})
	}
	return nil
}

func (s *SyncService) stopOnError(ctx context.Context, run domain.ImportRun, reason string, log *slog.Logger) {
	if ctx.Err() != nil {
		s.stop(run, (*domain.ImportRun).Interrupt, "the application shut down during the update", log)
		return
	}
	s.stop(run, (*domain.ImportRun).Fail, reason, log)
}

func (s *SyncService) stop(run domain.ImportRun, transition func(*domain.ImportRun, string, time.Time) error, reason string, log *slog.Logger) {
	if err := transition(&run, reason, s.now()); err != nil {
		log.Error("cannot stop update", "err", err)
		return
	}
	// The run context may already be cancelled; the status must still be saved.
	if err := s.runs.SaveRun(context.Background(), run); err != nil {
		log.Error("cannot save update status", "err", err)
		return
	}
	log.Warn("update stopped", "status", run.Status, "reason", reason)
}
