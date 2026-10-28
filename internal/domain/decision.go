package domain

import (
	"fmt"
	"time"
)

type DecisionKind string

const (
	DecisionCreated      DecisionKind = "CREATED"
	DecisionUpdated      DecisionKind = "UPDATED"
	DecisionWithdrawn    DecisionKind = "WITHDRAWN"
	DecisionReactivated  DecisionKind = "REACTIVATED"
	DecisionUnchanged    DecisionKind = "UNCHANGED"
	DecisionIgnoredStale DecisionKind = "IGNORED_STALE"
	DecisionInvalid      DecisionKind = "INVALID"
)

// Reportable reports whether staff should see the decision in the change list.
func (k DecisionKind) Reportable() bool {
	switch k {
	case DecisionCreated, DecisionUpdated, DecisionWithdrawn, DecisionReactivated, DecisionIgnoredStale:
		return true
	}
	return false
}

type FieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// Decision is what should happen to the catalog for one supplier record.
type Decision struct {
	Kind    DecisionKind
	Save    bool    // Product must be persisted
	Product Product // the new state when Save is true
	Changes []FieldChange
	Reason  string // why a record was ignored or rejected
}

// Decide applies the catalog ordering rules to one supplier record.
//
// The supplier's updated_at is the only ordering signal; arrival order does
// not matter. A record is applied only when it is strictly newer than what is
// stored, so repeated records are no-ops and late records can neither
// overwrite newer data nor restore a withdrawn product. A newer active record
// does re-list a withdrawn product.
func Decide(stored *Product, rec SupplierRecord) Decision {
	if err := rec.Validate(); err != nil {
		return Decision{Kind: DecisionInvalid, Reason: err.Error()}
	}
	status, _ := ParseSupplierStatus(rec.Status)
	updatedAt := rec.UpdatedAt.UTC()

	if stored == nil {
		p := Product{
			SKU:             rec.SKU,
			Name:            rec.Name,
			Category:        rec.Category,
			PriceCents:      rec.PriceCents,
			Status:          status,
			SourceUpdatedAt: updatedAt,
		}
		if status == StatusWithdrawn {
			// Tombstone for a product we never had: only the status is meaningful.
			changes := []FieldChange{{Field: "status", New: string(StatusWithdrawn)}}
			return Decision{Kind: DecisionWithdrawn, Save: true, Product: p, Changes: changes}
		}
		return Decision{Kind: DecisionCreated, Save: true, Product: p, Changes: diff(nil, p)}
	}

	if updatedAt.Equal(stored.SourceUpdatedAt) {
		return Decision{Kind: DecisionUnchanged, Reason: "version already applied"}
	}
	if updatedAt.Before(stored.SourceUpdatedAt) {
		return Decision{
			Kind: DecisionIgnoredStale,
			Reason: fmt.Sprintf("supplier data from %s is older than the stored version from %s",
				updatedAt.Format(time.RFC3339), stored.SourceUpdatedAt.Format(time.RFC3339)),
		}
	}

	next := *stored
	next.SourceUpdatedAt = updatedAt
	if status == StatusWithdrawn {
		// Withdrawal notices may omit details, so keep the ones we know.
		next.Status = StatusWithdrawn
	} else {
		next.Name, next.Category, next.PriceCents, next.Status = rec.Name, rec.Category, rec.PriceCents, StatusActive
	}

	changes := diff(stored, next)
	kind := DecisionUpdated
	switch {
	case len(changes) == 0:
		kind = DecisionUnchanged
	case stored.Status == StatusActive && next.Status == StatusWithdrawn:
		kind = DecisionWithdrawn
	case stored.Status == StatusWithdrawn && next.Status == StatusActive:
		kind = DecisionReactivated
	}
	// A newer version is saved even without visible changes: its timestamp is
	// what guards against older data arriving later.
	return Decision{Kind: kind, Save: true, Product: next, Changes: changes}
}

func diff(old *Product, next Product) []FieldChange {
	var before Product
	oldPrice := ""
	if old != nil {
		before = *old
		oldPrice = FormatPrice(old.PriceCents)
	}
	var out []FieldChange
	add := func(field, a, b string) {
		if a != b {
			out = append(out, FieldChange{Field: field, Old: a, New: b})
		}
	}
	add("name", before.Name, next.Name)
	add("category", before.Category, next.Category)
	add("price", oldPrice, FormatPrice(next.PriceCents))
	add("status", string(before.Status), string(next.Status))
	return out
}
