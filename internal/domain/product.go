package domain

import (
	"fmt"
	"strings"
	"time"
)

type ProductStatus string

const (
	StatusActive    ProductStatus = "ACTIVE"
	StatusWithdrawn ProductStatus = "WITHDRAWN"
)

// Product is the shop's local copy of one supplier product. Withdrawn
// products are kept (as tombstones) so their timestamp keeps blocking older
// supplier data from bringing them back.
type Product struct {
	SKU             string
	Name            string
	Category        string
	PriceCents      int64
	Status          ProductStatus
	SourceUpdatedAt time.Time // supplier's updated_at of the version we hold
}

// SupplierRecord is one product entry exactly as delivered by the supplier.
type SupplierRecord struct {
	SKU        string
	Name       string
	Category   string
	PriceCents int64
	Status     string // "active" or "withdrawn"
	UpdatedAt  time.Time
}

// ParseSupplierStatus maps the supplier's status text to a product status.
func ParseSupplierStatus(s string) (ProductStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return StatusActive, true
	case "withdrawn":
		return StatusWithdrawn, true
	}
	return "", false
}

// ValidationError lists every reason a supplier record cannot be applied.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Validate checks a record before it may touch the catalog. Withdrawal notices
// only need an SKU and a timestamp; active records need full details.
func (r SupplierRecord) Validate() error {
	var problems []string
	if strings.TrimSpace(r.SKU) == "" {
		problems = append(problems, "missing sku")
	}
	if r.UpdatedAt.IsZero() {
		problems = append(problems, "missing or invalid updated_at")
	}
	status, ok := ParseSupplierStatus(r.Status)
	if !ok {
		problems = append(problems, fmt.Sprintf("unknown status %q", r.Status))
	}
	if ok && status == StatusActive {
		if strings.TrimSpace(r.Name) == "" {
			problems = append(problems, "missing name")
		}
		if r.PriceCents < 0 {
			problems = append(problems, "negative price")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// FormatPrice renders a price in cents as "12.99".
func FormatPrice(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
