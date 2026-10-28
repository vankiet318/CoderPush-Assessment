package usecase

import (
	"context"

	"catalogsync/internal/domain"
)

const recentRunLimit = 20

// QueryService answers the staff questions: did the update complete, what
// changed, and which problems still need attention.
type QueryService struct {
	store    ReadStore
	supplier Supplier
}

func NewQueryService(store ReadStore, supplier Supplier) *QueryService {
	return &QueryService{store: store, supplier: supplier}
}

type Overview struct {
	Feeds      []FeedInfo
	Runs       []domain.ImportRun // most recent first
	Incomplete []domain.ImportRun // FAILED or INTERRUPTED, waiting to be resumed
	OpenIssues int
}

func (q *QueryService) Overview(ctx context.Context) (Overview, error) {
	feeds, err := q.supplier.ListFeeds(ctx)
	if err != nil {
		return Overview{}, err
	}
	runs, err := q.store.ListRuns(ctx, 0)
	if err != nil {
		return Overview{}, err
	}
	open, err := q.store.ListIssues(ctx, IssueFilter{Status: domain.IssueOpen})
	if err != nil {
		return Overview{}, err
	}

	ov := Overview{Feeds: feeds, OpenIssues: len(open)}
	for _, run := range runs {
		if run.Resumable() {
			ov.Incomplete = append(ov.Incomplete, run)
		}
	}
	if len(runs) > recentRunLimit {
		runs = runs[:recentRunLimit]
	}
	ov.Runs = runs
	return ov, nil
}

type RunDetail struct {
	Run     domain.ImportRun
	Changes []domain.Change
	Issues  []domain.Issue
}

func (q *QueryService) RunDetail(ctx context.Context, id int64) (RunDetail, error) {
	run, err := q.store.GetRun(ctx, id)
	if err != nil {
		return RunDetail{}, err
	}
	changes, err := q.store.ListChanges(ctx, id)
	if err != nil {
		return RunDetail{}, err
	}
	issues, err := q.store.ListIssues(ctx, IssueFilter{RunID: id})
	if err != nil {
		return RunDetail{}, err
	}
	return RunDetail{Run: run, Changes: changes, Issues: issues}, nil
}

func (q *QueryService) Issues(ctx context.Context, status domain.IssueStatus) ([]domain.Issue, error) {
	return q.store.ListIssues(ctx, IssueFilter{Status: status})
}

func (q *QueryService) Products(ctx context.Context) ([]domain.Product, error) {
	return q.store.ListProducts(ctx)
}
