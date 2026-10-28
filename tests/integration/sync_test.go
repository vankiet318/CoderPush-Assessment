package integration

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

func TestInitialImportCreatesProducts(t *testing.T) {
	e := newEnv(t, initialFeed())

	run := e.runFeed("initial")

	requireStatus(t, run, domain.RunCompleted)
	if run.Counters.Batches != 3 || run.Counters.Created != 6 {
		t.Fatalf("counters: %+v", run.Counters)
	}
	if len(e.products()) != 6 || len(e.changes(run.ID)) != 6 {
		t.Fatalf("got %d products and %d changes, want 6 and 6", len(e.products()), len(e.changes(run.ID)))
	}
	e.assertInvariants()
}

func TestReimportingTheSameUpdateChangesNothing(t *testing.T) {
	e := newEnv(t, initialFeed())
	e.runFeed("initial")
	before := e.snapshot()

	again := e.runFeed("initial")

	requireStatus(t, again, domain.RunCompleted)
	if again.Counters.Unchanged != 6 || again.Counters.Created != 0 || len(e.changes(again.ID)) != 0 {
		t.Fatalf("re-import should only see repeats: %+v", again.Counters)
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("catalog changed on re-import:\n%s\nvs\n%s", before, after)
	}
	e.assertInvariants()
}

func TestRepeatedRecordsWithinAndAcrossBatches(t *testing.T) {
	e := newEnv(t, feed("repeats",
		batch("r-1", active("ACM-1", "Can", 100, 1), active("ACM-1", "Can", 100, 1)),
		batch("r-2", active("ACM-1", "Can", 100, 1)),
	))

	run := e.runFeed("repeats")

	if len(e.products()) != 1 || run.Counters.Created != 1 || run.Counters.Unchanged != 2 {
		t.Fatalf("products=%d counters=%+v", len(e.products()), run.Counters)
	}
	e.assertInvariants()
}

func TestOlderDataNeverOverwritesNewer(t *testing.T) {
	e := newEnv(t,
		feed("out-of-order",
			batch("o-1", active("ACM-1", "Can v3", 300, 3)),
			batch("o-2", active("ACM-1", "Can v1", 100, 1), active("ACM-1", "Can v2", 200, 2)),
		),
		feed("late", batch("l-1", active("ACM-1", "Can v2", 200, 2))),
	)

	run := e.runFeed("out-of-order")
	late := e.runFeed("late")

	if p := e.product("ACM-1"); p.Name != "Can v3" || p.PriceCents != 300 {
		t.Fatalf("older data overwrote newer: %+v", p)
	}
	if run.Counters.Stale != 2 || late.Counters.Stale != 1 {
		t.Fatalf("stale counts: run=%+v late=%+v", run.Counters, late.Counters)
	}
	e.assertInvariants()
}

func TestOlderDataNeverRestoresWithdrawnProduct(t *testing.T) {
	e := newEnv(t,
		initialFeed(),
		feed("withdraw", batch("w-1", withdrawn("ACM-1", 5))),
		feed("late", batch("l-1", active("ACM-1", "Watering Can", 1299, 3))),
	)
	e.runFeed("initial")
	e.runFeed("withdraw")

	e.runFeed("late")

	if p := e.product("ACM-1"); p.Status != domain.StatusWithdrawn {
		t.Fatalf("late data restored a withdrawn product: %+v", p)
	}
	e.assertInvariants()
}

func TestNewerDataReactivatesWithdrawnProduct(t *testing.T) {
	e := newEnv(t,
		initialFeed(),
		feed("withdraw", batch("w-1", withdrawn("ACM-1", 5))),
		feed("relist", batch("r-1", active("ACM-1", "Watering Can", 1399, 9))),
	)
	e.runFeed("initial")
	e.runFeed("withdraw")

	run := e.runFeed("relist")

	if p := e.product("ACM-1"); p.Status != domain.StatusActive || p.PriceCents != 1399 {
		t.Fatalf("product was not re-listed: %+v", p)
	}
	if run.Counters.Reactivated != 1 {
		t.Fatalf("counters: %+v", run.Counters)
	}
	e.assertInvariants()
}

func TestInvalidRecordBecomesIssueAndRestOfBatchIsApplied(t *testing.T) {
	e := newEnv(t, feed("bad",
		batch("b-1",
			active("ACM-1", "Can", 100, 1),
			active("ACM-2", "Hose", -5, 1),
			active("", "No SKU", 100, 1),
			active("ACM-3", "Shears", 300, 1),
		),
	))

	run := e.runFeed("bad")

	requireStatus(t, run, domain.RunCompleted)
	products := e.products()
	if _, ok := products["ACM-2"]; ok || len(products) != 2 {
		t.Fatalf("catalog: %+v", products)
	}
	if open := e.issueList(domain.IssueOpen); len(open) != 2 || run.Counters.Invalid != 2 {
		t.Fatalf("open issues=%d counters=%+v", len(open), run.Counters)
	}
	e.assertInvariants()
}

func TestClosedIssueStaysClosedOnReimport(t *testing.T) {
	e := newEnv(t, feed("bad", batch("b-1", active("ACM-1", "Can", -1, 1))))
	e.runFeed("bad")
	open := e.issueList(domain.IssueOpen)
	if len(open) != 1 {
		t.Fatalf("want 1 open issue, got %d", len(open))
	}
	if _, err := e.issues.Close(ctx, open[0].ID); err != nil {
		t.Fatal(err)
	}

	e.runFeed("bad")

	if n := len(e.issueList(domain.IssueOpen)); n != 0 {
		t.Fatalf("same bad record reopened the issue: %d open", n)
	}
	if n := len(e.issueList("")); n != 1 {
		t.Fatalf("same bad record duplicated the issue: %d total", n)
	}
}

func TestSupplierOutageKeepsCatalogAndResumeFinishesTheUpdate(t *testing.T) {
	changes := feed("changes",
		batch("c-1", active("ACM-1", "Can", 1100, 5)),
		batch("c-2", active("ACM-2", "Hose", 2200, 5)),
		batch("c-3", active("ACM-3", "Shears", 3300, 5)),
	)
	reference := newEnv(t, initialFeed(), changes)
	reference.runFeed("initial")
	reference.runFeed("changes")

	e := newEnv(t, initialFeed(), changes)
	e.runFeed("initial")
	e.supplier.SetOutage(2)

	failed := e.runFeed("changes")

	requireStatus(t, failed, domain.RunFailed)
	if failed.Counters.Batches != 1 || len(e.products()) != 6 {
		t.Fatalf("after outage: batches=%d products=%d", failed.Counters.Batches, len(e.products()))
	}
	if e.product("ACM-1").PriceCents != 1100 || e.product("ACM-2").PriceCents != 2499 {
		t.Fatal("finished batch must be kept and unfinished batch must not be applied")
	}

	e.supplier.SetOutage(0)
	resumed := e.resume(failed.ID)

	requireStatus(t, resumed, domain.RunCompleted)
	if resumed.Counters.Batches != 3 || resumed.Counters.Updated != 3 {
		t.Fatalf("resumed counters: %+v", resumed.Counters)
	}
	if e.snapshot() != reference.snapshot() {
		t.Fatalf("resumed catalog differs from uninterrupted one:\n%s\nvs\n%s", e.snapshot(), reference.snapshot())
	}
	e.assertInvariants()
}

// crashAt simulates a crash inside the batch transaction: the update
// goroutine dies before commit and nothing after that point runs.
func crashAt(batchNo, recordNo int) usecase.SyncOptions {
	return usecase.SyncOptions{FaultHook: func(b, r int) {
		if b == batchNo && r == recordNo {
			runtime.Goexit()
		}
	}}
}

func TestCrashMidBatchDiscardsHalfBatchAndResumeCompletes(t *testing.T) {
	reference := newEnv(t, initialFeed())
	reference.runFeed("initial")

	e := newEnvWith(t, crashAt(2, 1), initialFeed())
	crashed := e.runFeed("initial")
	requireStatus(t, crashed, domain.RunRunning) // the dead process could not record anything

	e.restart(usecase.SyncOptions{})

	interrupted := e.run(crashed.ID)
	requireStatus(t, interrupted, domain.RunInterrupted)
	products := e.products()
	if len(products) != 2 || interrupted.Counters.Batches != 1 {
		t.Fatalf("after crash: products=%d batches=%d, want batch 1 only", len(products), interrupted.Counters.Batches)
	}
	if _, ok := products["ACM-3"]; ok {
		t.Fatal("record written before the crash in batch 2 must have been rolled back")
	}

	resumed := e.resume(crashed.ID)

	requireStatus(t, resumed, domain.RunCompleted)
	if e.snapshot() != reference.snapshot() {
		t.Fatalf("resumed catalog differs:\n%s\nvs\n%s", e.snapshot(), reference.snapshot())
	}
	if resumed.Counters.Created != 6 || resumed.Counters.Records != 6 {
		t.Fatalf("records of the rolled-back batch were counted twice: %+v", resumed.Counters)
	}
	e.assertInvariants()
}

func TestOnlyOneUpdateRunsAtATime(t *testing.T) {
	e := newEnvWith(t, usecase.SyncOptions{BatchDelay: time.Hour}, initialFeed())
	first, err := e.sync.StartRun(ctx, "initial")
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.sync.StartRun(ctx, "initial")

	if !errors.Is(err, domain.ErrRunInProgress) {
		t.Fatalf("second start: got %v, want ErrRunInProgress", err)
	}
	e.close() // shutting down marks the running update as interrupted
	e.open(usecase.SyncOptions{})
	requireStatus(t, e.run(first.ID), domain.RunInterrupted)
}

func TestOnlyIncompleteUpdatesCanBeResumed(t *testing.T) {
	e := newEnv(t, initialFeed())
	run := e.runFeed("initial")

	_, err := e.sync.ResumeRun(ctx, run.ID)

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("resume completed update: got %v", err)
	}
	if _, err := e.sync.ResumeRun(ctx, 999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("resume unknown update: got %v", err)
	}
	if _, err := e.sync.StartRun(ctx, "no-such-feed"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("start unknown feed: got %v", err)
	}
}

func TestResetClearsEverythingAndNumberingRestarts(t *testing.T) {
	e := newEnv(t, initialFeed(), feed("bad", batch("b-1", active("ACM-1", "Can", -1, 9))))
	e.runFeed("initial")
	e.runFeed("bad")

	if err := e.sync.Reset(ctx); err != nil {
		t.Fatal(err)
	}

	runs, err := e.store.ListRuns(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.products()) != 0 || len(runs) != 0 || len(e.issueList("")) != 0 {
		t.Fatalf("after reset: products=%d runs=%d issues=%d", len(e.products()), len(runs), len(e.issueList("")))
	}
	again := e.runFeed("initial")
	if again.ID != 1 || again.Counters.Created != 6 {
		t.Fatalf("update after reset: id=%d counters=%+v", again.ID, again.Counters)
	}
	e.assertInvariants()
}

func TestResetIsRefusedWhileAnUpdateRuns(t *testing.T) {
	e := newEnvWith(t, usecase.SyncOptions{BatchDelay: time.Hour}, initialFeed())
	run, err := e.sync.StartRun(ctx, "initial")
	if err != nil {
		t.Fatal(err)
	}

	err = e.sync.Reset(ctx)

	if !errors.Is(err, domain.ErrRunInProgress) {
		t.Fatalf("reset during an update: got %v, want ErrRunInProgress", err)
	}
	requireStatus(t, e.run(run.ID), domain.RunRunning) // nothing was deleted
}

func TestBundledDemoFeeds(t *testing.T) {
	feeds := demoFeeds(t)
	e := newEnv(t, feeds...)

	r1 := e.runFeed("01-initial-catalog")
	r2 := e.runFeed("02-weekly-changes")
	r3 := e.runFeed("03-relist-and-late-data")
	r4 := e.runFeed("01-initial-catalog") // old full catalog sent again

	for _, r := range []domain.ImportRun{r1, r2, r3, r4} {
		requireStatus(t, r, domain.RunCompleted)
	}
	want := map[string]struct {
		status domain.ProductStatus
		price  int64
	}{
		"ACM-0001": {domain.StatusActive, 1199},   // price drop survives the old catalog re-send
		"ACM-0002": {domain.StatusActive, 2499},   // late 09-20 price ignored
		"ACM-0005": {domain.StatusActive, 379},    // withdrawn, late data ignored, then re-listed
		"ACM-0007": {domain.StatusWithdrawn, 799}, // withdrawn in 03
		"ACM-0013": {domain.StatusActive, 8999},   // late 10-01 price ignored
		"ACM-0014": {domain.StatusActive, 6999},   // rejected in 02, valid in 03
		"ACM-0099": {domain.StatusWithdrawn, 0},   // withdrawal for a product never seen
	}
	for sku, w := range want {
		if p := e.product(sku); p.Status != w.status || p.PriceCents != w.price {
			t.Errorf("%s: got %s %d, want %s %d", sku, p.Status, p.PriceCents, w.status, w.price)
		}
	}
	if r2.Counters.Invalid != 3 || len(e.issueList(domain.IssueOpen)) != 3 {
		t.Errorf("want 3 rejected records and open issues, got %+v / %d", r2.Counters, len(e.issueList(domain.IssueOpen)))
	}
	if r4.Counters.Created+r4.Counters.Updated+r4.Counters.Reactivated != 0 {
		t.Errorf("re-sending the old catalog changed products: %+v", r4.Counters)
	}
	e.assertInvariants()
}
