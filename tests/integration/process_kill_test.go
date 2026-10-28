package integration

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"catalogsync/internal/adapter/sqlite"
	"catalogsync/internal/adapter/supplier/mock"
	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

const (
	helperEnv   = "CATALOGSYNC_KILL_HELPER_DB"
	killedExit  = 3
	killBatchNo = 2
)

// TestKilledProcessLosesNoFinishedWork runs an update in a child process that
// exits abruptly in the middle of batch 2: no deferred rollback, no status
// update, no clean close. The parent then opens the same file the way a
// restarted application would.
func TestKilledProcessLosesNoFinishedWork(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	dbPath := filepath.Join(t.TempDir(), "catalog.db")

	cmd := exec.Command(os.Args[0], "-test.run=^TestKillHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+dbPath)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != killedExit {
		t.Fatalf("child process should have been killed mid-batch, got %v", err)
	}

	e := &env{t: t, dbPath: dbPath, supplier: mock.New(initialFeed()), clock: &clock{}}
	e.open(usecase.SyncOptions{})
	t.Cleanup(e.close)

	runs, err := e.store.ListRuns(ctx, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	run := runs[0]
	requireStatus(t, run, domain.RunInterrupted)
	if run.Counters.Batches != killBatchNo-1 || len(e.products()) != 2 {
		t.Fatalf("after kill: batches=%d products=%d, want only batch 1", run.Counters.Batches, len(e.products()))
	}

	resumed := e.resume(run.ID)

	requireStatus(t, resumed, domain.RunCompleted)
	if len(e.products()) != 6 || resumed.Counters.Created != 6 {
		t.Fatalf("after resume: products=%d counters=%+v", len(e.products()), resumed.Counters)
	}
	e.assertInvariants()
}

// TestKillHelperProcess is the child side of TestKilledProcessLosesNoFinishedWork.
func TestKillHelperProcess(t *testing.T) {
	dbPath := os.Getenv(helperEnv)
	if dbPath == "" {
		t.Skip("helper process only")
	}
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := usecase.NewSyncService(store, mock.New(initialFeed()), (&clock{}).Now, usecase.SyncOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FaultHook: func(b, r int) {
			if b == killBatchNo && r == 1 {
				os.Exit(killedExit)
			}
		},
	})
	if _, err := svc.StartRun(ctx, "initial"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	t.Fatal("the process should have been killed")
}
