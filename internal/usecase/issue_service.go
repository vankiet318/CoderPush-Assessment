package usecase

import (
	"context"

	"catalogsync/internal/domain"
)

type IssueService struct {
	store IssueStore
	now   Clock
}

func NewIssueService(store IssueStore, now Clock) *IssueService {
	return &IssueService{store: store, now: now}
}

// Close marks an issue as handled by staff.
func (s *IssueService) Close(ctx context.Context, id int64) (domain.Issue, error) {
	issue, err := s.store.GetIssue(ctx, id)
	if err != nil {
		return domain.Issue{}, err
	}
	if err := issue.Resolve(s.now()); err != nil {
		return domain.Issue{}, err
	}
	if err := s.store.SaveIssue(ctx, issue); err != nil {
		return domain.Issue{}, err
	}
	return issue, nil
}
