package usecase

import (
	"context"
	"time"

	"catalogsync/internal/domain"
)

// Clock returns the current time; injected so tests control it.
type Clock func() time.Time

// FeedInfo describes an update the supplier can deliver.
type FeedInfo struct {
	Name        string
	Description string
	Batches     int
}

// SupplierBatch is one page of an update. NextCursor is where the following
// batch starts; Last is true when the supplier has nothing more to send.
type SupplierBatch struct {
	ID         string
	Records    []domain.SupplierRecord
	NextCursor string
	Last       bool
}

// Supplier delivers updates in batches, addressed by an opaque cursor.
type Supplier interface {
	ListFeeds(ctx context.Context) ([]FeedInfo, error)
	FetchBatch(ctx context.Context, feed, cursor string) (SupplierBatch, error)
}

// RunStore persists update runs. CreateRun and SaveRun return
// domain.ErrRunInProgress when another run is already RUNNING.
type RunStore interface {
	CreateRun(ctx context.Context, run domain.ImportRun) (domain.ImportRun, error)
	GetRun(ctx context.Context, id int64) (domain.ImportRun, error)
	ListRuns(ctx context.Context, limit int) ([]domain.ImportRun, error)
	SaveRun(ctx context.Context, run domain.ImportRun) error
	// InTx runs fn in one transaction: everything fn writes is committed
	// together, or nothing is.
	InTx(ctx context.Context, fn func(CatalogTx) error) error
}

// CatalogTx is the set of writes allowed inside a batch transaction.
type CatalogTx interface {
	// GetProduct returns nil when the product is unknown.
	GetProduct(ctx context.Context, sku string) (*domain.Product, error)
	SaveProduct(ctx context.Context, p domain.Product, runID int64, now time.Time) error
	AddChange(ctx context.Context, c domain.Change) error
	// AddIssueIfNew ignores issues whose fingerprint is already recorded.
	AddIssueIfNew(ctx context.Context, issue domain.Issue) (bool, error)
	SaveRun(ctx context.Context, run domain.ImportRun) error
}

type IssueFilter struct {
	Status domain.IssueStatus // empty means any status
	RunID  int64              // zero means any run
}

// ReadStore serves the staff-facing views.
type ReadStore interface {
	ListRuns(ctx context.Context, limit int) ([]domain.ImportRun, error)
	GetRun(ctx context.Context, id int64) (domain.ImportRun, error)
	ListChanges(ctx context.Context, runID int64) ([]domain.Change, error)
	ListIssues(ctx context.Context, filter IssueFilter) ([]domain.Issue, error)
	ListProducts(ctx context.Context) ([]domain.Product, error)
}

type IssueStore interface {
	GetIssue(ctx context.Context, id int64) (domain.Issue, error)
	SaveIssue(ctx context.Context, issue domain.Issue) error
}
