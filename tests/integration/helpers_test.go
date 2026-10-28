// Package integration exercises the whole update flow against a real SQLite
// file and the simulated supplier.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"catalogsync/internal/adapter/sqlite"
	"catalogsync/internal/adapter/supplier/mock"
	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

var ctx = context.Background()

// env is one application instance on a database file. restart() simulates
// stopping the application and starting it again on the same file.
type env struct {
	t        *testing.T
	dbPath   string
	supplier *mock.Supplier
	clock    *clock
	store    *sqlite.Store
	sync     *usecase.SyncService
	queries  *usecase.QueryService
	issues   *usecase.IssueService
}

func newEnv(t *testing.T, feeds ...mock.Feed) *env {
	return newEnvWith(t, usecase.SyncOptions{}, feeds...)
}

func newEnvWith(t *testing.T, opts usecase.SyncOptions, feeds ...mock.Feed) *env {
	t.Helper()
	e := &env{
		t:        t,
		dbPath:   filepath.Join(t.TempDir(), "catalog.db"),
		supplier: mock.New(feeds...),
		clock:    &clock{now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)},
	}
	e.open(opts)
	t.Cleanup(e.close)
	return e
}

func (e *env) open(opts usecase.SyncOptions) {
	e.t.Helper()
	store, err := sqlite.Open(e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	e.store = store
	e.sync = usecase.NewSyncService(store, e.supplier, e.clock.Now, opts)
	e.queries = usecase.NewQueryService(store, e.supplier)
	e.issues = usecase.NewIssueService(store, e.clock.Now)
	if _, err := e.sync.RecoverInterrupted(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) close() {
	if e.store == nil {
		return
	}
	e.sync.Shutdown()
	e.store.Close()
	e.store = nil
}

func (e *env) restart(opts usecase.SyncOptions) {
	e.t.Helper()
	e.close()
	e.open(opts)
}

// runFeed starts an update and waits until it stops, for whatever reason.
func (e *env) runFeed(feed string) domain.ImportRun {
	e.t.Helper()
	run, err := e.sync.StartRun(ctx, feed)
	if err != nil {
		e.t.Fatalf("start %s: %v", feed, err)
	}
	e.sync.Wait()
	return e.run(run.ID)
}

func (e *env) resume(id int64) domain.ImportRun {
	e.t.Helper()
	if _, err := e.sync.ResumeRun(ctx, id); err != nil {
		e.t.Fatalf("resume #%d: %v", id, err)
	}
	e.sync.Wait()
	return e.run(id)
}

func (e *env) run(id int64) domain.ImportRun {
	e.t.Helper()
	run, err := e.store.GetRun(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

func (e *env) products() map[string]domain.Product {
	e.t.Helper()
	list, err := e.store.ListProducts(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	m := make(map[string]domain.Product, len(list))
	for _, p := range list {
		if _, dup := m[p.SKU]; dup {
			e.t.Fatalf("duplicate product %s", p.SKU)
		}
		m[p.SKU] = p
	}
	return m
}

func (e *env) product(sku string) domain.Product {
	e.t.Helper()
	p, ok := e.products()[sku]
	if !ok {
		e.t.Fatalf("product %s not in catalog", sku)
	}
	return p
}

func (e *env) changes(runID int64) []domain.Change {
	e.t.Helper()
	changes, err := e.store.ListChanges(ctx, runID)
	if err != nil {
		e.t.Fatal(err)
	}
	return changes
}

func (e *env) issueList(status domain.IssueStatus) []domain.Issue {
	e.t.Helper()
	issues, err := e.store.ListIssues(ctx, usecase.IssueFilter{Status: status})
	if err != nil {
		e.t.Fatal(err)
	}
	return issues
}

// snapshot renders the catalog in a stable form so two catalogs can be compared.
func (e *env) snapshot() string {
	e.t.Helper()
	products := e.products()
	skus := make([]string, 0, len(products))
	for sku := range products {
		skus = append(skus, sku)
	}
	sort.Strings(skus)
	var b strings.Builder
	for _, sku := range skus {
		p := products[sku]
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s\n", p.SKU, p.Name, p.Category, p.PriceCents, p.Status, p.SourceUpdatedAt.Format(time.RFC3339))
	}
	return b.String()
}

// assertInvariants checks properties that must hold after any sequence of
// updates, crashes and resumes.
func (e *env) assertInvariants() {
	e.t.Helper()
	e.products() // fails on duplicate SKUs
	runs, err := e.store.ListRuns(ctx, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	running := 0
	for _, run := range runs {
		c := run.Counters
		if sum := c.Created + c.Updated + c.Withdrawn + c.Reactivated + c.Unchanged + c.Stale + c.Invalid; sum != c.Records {
			e.t.Errorf("update #%d: counters sum to %d but %d records were processed", run.ID, sum, c.Records)
		}
		if reported := c.Created + c.Updated + c.Withdrawn + c.Reactivated + c.Stale; reported != len(e.changes(run.ID)) {
			e.t.Errorf("update #%d: counters report %d changes but %d were saved", run.ID, reported, len(e.changes(run.ID)))
		}
		if run.IsCompleted() == run.FinishedAt.IsZero() {
			e.t.Errorf("update #%d: status %s with finished_at %v", run.ID, run.Status, run.FinishedAt)
		}
		if run.IsRunning() {
			running++
		}
	}
	if running > 1 {
		e.t.Errorf("%d updates are RUNNING at once", running)
	}
}

func requireStatus(t *testing.T, run domain.ImportRun, want domain.RunStatus) {
	t.Helper()
	if run.Status != want {
		t.Fatalf("update #%d is %s (%s), want %s", run.ID, run.Status, run.LastError, want)
	}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

// ---- fictional supplier data builders ----

func day(d int) string { return fmt.Sprintf("2026-10-%02dT08:00:00Z", d) }

func active(sku, name string, price int64, d int) mock.Record {
	return mock.Record{SKU: sku, Name: name, Category: "Garden", PriceCents: price, Status: "active", UpdatedAt: day(d)}
}

func withdrawn(sku string, d int) mock.Record {
	return mock.Record{SKU: sku, Status: "withdrawn", UpdatedAt: day(d)}
}

func batch(id string, recs ...mock.Record) mock.Batch { return mock.Batch{ID: id, Products: recs} }

func feed(name string, batches ...mock.Batch) mock.Feed {
	return mock.Feed{Name: name, Description: name, Batches: batches}
}

// initialFeed is a three-batch catalog of six products.
func initialFeed() mock.Feed {
	return feed("initial",
		batch("i-1", active("ACM-1", "Watering Can", 1299, 1), active("ACM-2", "Garden Hose", 2499, 1)),
		batch("i-2", active("ACM-3", "Pruning Shears", 1850, 1), active("ACM-4", "Terracotta Pot", 899, 1)),
		batch("i-3", active("ACM-5", "Tomato Seeds", 349, 1), active("ACM-6", "Potting Soil", 1199, 1)),
	)
}
