package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func at(day int) time.Time { return time.Date(2026, 10, day, 8, 0, 0, 0, time.UTC) }

func activeRec(sku, name string, price int64, day int) SupplierRecord {
	return SupplierRecord{SKU: sku, Name: name, Category: "Tools", PriceCents: price, Status: "active", UpdatedAt: at(day)}
}

func withdrawnRec(sku string, day int) SupplierRecord {
	return SupplierRecord{SKU: sku, Status: "withdrawn", UpdatedAt: at(day)}
}

func product(status ProductStatus, name string, price int64, day int) *Product {
	return &Product{SKU: "A1", Name: name, Category: "Tools", PriceCents: price, Status: status, SourceUpdatedAt: at(day)}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		stored     *Product
		rec        SupplierRecord
		wantKind   DecisionKind
		wantSave   bool
		wantStatus ProductStatus
		wantPrice  int64
		wantDay    int
	}{
		{"new product is created", nil, activeRec("A1", "Can", 100, 10),
			DecisionCreated, true, StatusActive, 100, 10},
		{"newer data updates", product(StatusActive, "Can", 100, 10), activeRec("A1", "Can", 90, 20),
			DecisionUpdated, true, StatusActive, 90, 20},
		{"older data is ignored", product(StatusActive, "Can", 100, 20), activeRec("A1", "Can", 90, 10),
			DecisionIgnoredStale, false, "", 0, 0},
		{"repeated data is a no-op", product(StatusActive, "Can", 100, 10), activeRec("A1", "Can", 100, 10),
			DecisionUnchanged, false, "", 0, 0},
		{"same timestamp with other content is a no-op", product(StatusActive, "Can", 100, 10), activeRec("A1", "Can", 1, 10),
			DecisionUnchanged, false, "", 0, 0},
		{"newer identical data only moves the version", product(StatusActive, "Can", 100, 10), activeRec("A1", "Can", 100, 20),
			DecisionUnchanged, true, StatusActive, 100, 20},
		{"withdrawal withdraws", product(StatusActive, "Can", 100, 10), withdrawnRec("A1", 20),
			DecisionWithdrawn, true, StatusWithdrawn, 100, 20},
		{"older data does not restore a withdrawn product", product(StatusWithdrawn, "Can", 100, 20), activeRec("A1", "Can", 100, 10),
			DecisionIgnoredStale, false, "", 0, 0},
		{"newer data re-lists a withdrawn product", product(StatusWithdrawn, "Can", 100, 20), activeRec("A1", "Can", 120, 30),
			DecisionReactivated, true, StatusActive, 120, 30},
		{"newer withdrawal of a withdrawn product moves the version", product(StatusWithdrawn, "Can", 100, 20), withdrawnRec("A1", 30),
			DecisionUnchanged, true, StatusWithdrawn, 100, 30},
		{"withdrawal of an unknown product creates a tombstone", nil, withdrawnRec("A1", 10),
			DecisionWithdrawn, true, StatusWithdrawn, 0, 10},
		{"invalid record is rejected", product(StatusActive, "Can", 100, 10), activeRec("A1", "Can", -1, 20),
			DecisionInvalid, false, "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.stored, tt.rec)
			if d.Kind != tt.wantKind || d.Save != tt.wantSave {
				t.Fatalf("got kind=%s save=%v, want kind=%s save=%v (reason %q)", d.Kind, d.Save, tt.wantKind, tt.wantSave, d.Reason)
			}
			if !tt.wantSave {
				return
			}
			if d.Product.Status != tt.wantStatus || d.Product.PriceCents != tt.wantPrice || !d.Product.SourceUpdatedAt.Equal(at(tt.wantDay)) {
				t.Fatalf("got product %+v, want status=%s price=%d day=%d", d.Product, tt.wantStatus, tt.wantPrice, tt.wantDay)
			}
		})
	}
}

func TestDecideReportsFieldChanges(t *testing.T) {
	d := Decide(product(StatusActive, "Can", 100, 10), activeRec("A1", "Big Can", 150, 20))
	want := []FieldChange{{"name", "Can", "Big Can"}, {"price", "1.00", "1.50"}}
	if len(d.Changes) != len(want) {
		t.Fatalf("got %+v, want %+v", d.Changes, want)
	}
	for i := range want {
		if d.Changes[i] != want[i] {
			t.Fatalf("change %d: got %+v, want %+v", i, d.Changes[i], want[i])
		}
	}
}

func TestTombstoneReportsOnlyStatus(t *testing.T) {
	d := Decide(nil, withdrawnRec("A1", 10))
	want := FieldChange{Field: "status", New: "WITHDRAWN"}
	if len(d.Changes) != 1 || d.Changes[0] != want {
		t.Fatalf("got %+v, want only %+v", d.Changes, want)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		rec  SupplierRecord
		want []string // substrings expected in the error; nil = valid
	}{
		{"valid active", activeRec("A1", "Can", 100, 1), nil},
		{"valid withdrawal without details", withdrawnRec("A1", 1), nil},
		{"status is case-insensitive", SupplierRecord{SKU: "A1", Name: "Can", Status: "ACTIVE", UpdatedAt: at(1)}, nil},
		{"missing sku", activeRec("", "Can", 100, 1), []string{"missing sku"}},
		{"missing timestamp", SupplierRecord{SKU: "A1", Name: "Can", Status: "active"}, []string{"updated_at"}},
		{"unknown status", SupplierRecord{SKU: "A1", Status: "discontinued", UpdatedAt: at(1)}, []string{"unknown status"}},
		{"active needs a name and a non-negative price", SupplierRecord{SKU: "A1", Status: "active", PriceCents: -1, UpdatedAt: at(1)},
			[]string{"missing name", "negative price"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rec.Validate()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("got %v, want ValidationError", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestFormatPrice(t *testing.T) {
	for cents, want := range map[int64]string{0: "0.00", 5: "0.05", 1299: "12.99", -50: "-0.50", -150: "-1.50"} {
		if got := FormatPrice(cents); got != want {
			t.Errorf("FormatPrice(%d) = %q, want %q", cents, got, want)
		}
	}
}
