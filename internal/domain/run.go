package domain

import (
	"fmt"
	"slices"
	"time"
)

type RunStatus string

const (
	RunRunning     RunStatus = "RUNNING"
	RunFailed      RunStatus = "FAILED"      // supplier or storage error; resumable
	RunInterrupted RunStatus = "INTERRUPTED" // application stopped mid-update; resumable
	RunCompleted   RunStatus = "COMPLETED"
)

// RunCounters summarise what an update did, record by record.
type RunCounters struct {
	Batches     int `json:"batches"`
	Records     int `json:"records"`
	Created     int `json:"created"`
	Updated     int `json:"updated"`
	Withdrawn   int `json:"withdrawn"`
	Reactivated int `json:"reactivated"`
	Unchanged   int `json:"unchanged"`
	Stale       int `json:"stale"`
	Invalid     int `json:"invalid"`
}

func (c *RunCounters) Count(k DecisionKind) {
	c.Records++
	switch k {
	case DecisionCreated:
		c.Created++
	case DecisionUpdated:
		c.Updated++
	case DecisionWithdrawn:
		c.Withdrawn++
	case DecisionReactivated:
		c.Reactivated++
	case DecisionUnchanged:
		c.Unchanged++
	case DecisionIgnoredStale:
		c.Stale++
	case DecisionInvalid:
		c.Invalid++
	}
}

// ImportRun is one staff-initiated update from the supplier. Cursor is the
// checkpoint: the supplier position right after the last committed batch.
type ImportRun struct {
	ID         int64
	Feed       string
	Status     RunStatus
	Cursor     string
	Counters   RunCounters
	LastError  string
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time // zero until completed
}

func NewRun(feed string, now time.Time) ImportRun {
	return ImportRun{Feed: feed, Status: RunRunning, StartedAt: now, UpdatedAt: now}
}

func (r ImportRun) IsRunning() bool   { return r.Status == RunRunning }
func (r ImportRun) IsCompleted() bool { return r.Status == RunCompleted }
func (r ImportRun) Resumable() bool   { return r.Status == RunFailed || r.Status == RunInterrupted }

// AdvanceBatch records a fully applied batch. A run is only complete when the
// supplier marks the batch as the last one.
func (r *ImportRun) AdvanceBatch(nextCursor string, last bool, now time.Time) error {
	if r.Status != RunRunning {
		return fmt.Errorf("%w: update #%d is %s, not RUNNING", ErrInvalidTransition, r.ID, r.Status)
	}
	r.Cursor = nextCursor
	r.Counters.Batches++
	r.UpdatedAt = now
	if last {
		r.Status = RunCompleted
		r.FinishedAt = now
	}
	return nil
}

func (r *ImportRun) Fail(reason string, now time.Time) error {
	if err := r.transition(RunFailed, now, RunRunning); err != nil {
		return err
	}
	r.LastError = reason
	return nil
}

func (r *ImportRun) Interrupt(reason string, now time.Time) error {
	if err := r.transition(RunInterrupted, now, RunRunning); err != nil {
		return err
	}
	r.LastError = reason
	return nil
}

func (r *ImportRun) Resume(now time.Time) error {
	if err := r.transition(RunRunning, now, RunFailed, RunInterrupted); err != nil {
		return err
	}
	r.LastError = ""
	return nil
}

func (r *ImportRun) transition(to RunStatus, now time.Time, from ...RunStatus) error {
	if !slices.Contains(from, r.Status) {
		return fmt.Errorf("%w: update #%d cannot go from %s to %s", ErrInvalidTransition, r.ID, r.Status, to)
	}
	r.Status = to
	r.UpdatedAt = now
	return nil
}
