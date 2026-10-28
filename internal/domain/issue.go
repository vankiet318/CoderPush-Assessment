package domain

import (
	"fmt"
	"time"
)

type IssueKind string

const IssueInvalidRecord IssueKind = "INVALID_RECORD"

type IssueStatus string

const (
	IssueOpen     IssueStatus = "OPEN"
	IssueResolved IssueStatus = "RESOLVED"
)

// Issue is a problem that needs staff attention. Issues stay open until a
// staff member closes them.
type Issue struct {
	ID          int64
	RunID       int64
	SKU         string
	Kind        IssueKind
	Message     string
	Fingerprint string // identifies the underlying problem across re-imports
	Status      IssueStatus
	CreatedAt   time.Time
	ResolvedAt  time.Time
}

// NewInvalidRecordIssue builds an issue for a rejected supplier record. The
// fingerprint names the exact supplier record, so re-importing the same bad
// data neither duplicates the issue nor reopens a closed one.
func NewInvalidRecordIssue(runID int64, feed, batchID string, index int, rec SupplierRecord, reason string, now time.Time) Issue {
	fp := fmt.Sprintf("invalid:%s@%s", rec.SKU, rec.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if rec.SKU == "" || rec.UpdatedAt.IsZero() {
		fp = fmt.Sprintf("invalid:%s/%s#%d", feed, batchID, index)
	}
	return Issue{
		RunID:       runID,
		SKU:         rec.SKU,
		Kind:        IssueInvalidRecord,
		Message:     fmt.Sprintf("Batch %s, record %d rejected: %s", batchID, index+1, reason),
		Fingerprint: fp,
		Status:      IssueOpen,
		CreatedAt:   now,
	}
}

func (i *Issue) Resolve(now time.Time) error {
	if i.Status != IssueOpen {
		return fmt.Errorf("%w: issue #%d is already %s", ErrInvalidTransition, i.ID, i.Status)
	}
	i.Status = IssueResolved
	i.ResolvedAt = now
	return nil
}
