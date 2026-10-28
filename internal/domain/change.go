package domain

import "time"

// Change is one entry in an update's "what changed" report.
type Change struct {
	ID      int64
	RunID   int64
	BatchID string
	SKU     string
	Kind    DecisionKind
	Fields  []FieldChange
	Reason  string
	At      time.Time
}
