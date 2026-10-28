// Package mock is a fictional supplier ("Acme Garden Supply") that serves
// updates from JSON files and can simulate an outage.
package mock

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"sync"
	"time"

	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

var _ usecase.Supplier = (*Supplier)(nil)

var ErrUnavailable = errors.New("supplier unavailable (simulated outage)")

//go:embed feeds/*.json
var defaultFeeds embed.FS

// Record is a product as it appears in the supplier's JSON.
type Record struct {
	SKU        string `json:"sku"`
	Name       string `json:"name,omitempty"`
	Category   string `json:"category,omitempty"`
	PriceCents int64  `json:"price_cents,omitempty"`
	Status     string `json:"status"`
	UpdatedAt  string `json:"updated_at"`
}

type Batch struct {
	ID       string   `json:"id"`
	Products []Record `json:"products"`
}

// Feed is one update the supplier can deliver, split into batches.
type Feed struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Batches     []Batch `json:"batches"`
}

type Supplier struct {
	mu              sync.RWMutex
	feeds           map[string]Feed
	order           []string
	outageFromBatch int // 0 = online
}

func New(feeds ...Feed) *Supplier {
	s := &Supplier{feeds: make(map[string]Feed)}
	for _, f := range feeds {
		s.feeds[f.Name] = f
		s.order = append(s.order, f.Name)
	}
	return s
}

// DefaultFeeds returns the bundled demonstration updates, sorted by name.
func DefaultFeeds() ([]Feed, error) {
	return LoadFeeds(defaultFeeds, "feeds/*.json")
}

func LoadFeeds(fsys fs.FS, pattern string) ([]Feed, error) {
	paths, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	feeds := make([]Feed, 0, len(paths))
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		var f Feed
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		feeds = append(feeds, f)
	}
	sort.Slice(feeds, func(i, j int) bool { return feeds[i].Name < feeds[j].Name })
	return feeds, nil
}

// SetOutage makes every fetch of batch number fromBatch (1-based) or later
// fail. Zero brings the supplier back online.
func (s *Supplier) SetOutage(fromBatch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outageFromBatch = max(fromBatch, 0)
}

func (s *Supplier) Outage() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outageFromBatch
}

func (s *Supplier) ListFeeds(ctx context.Context) ([]usecase.FeedInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	infos := make([]usecase.FeedInfo, 0, len(s.order))
	for _, name := range s.order {
		f := s.feeds[name]
		infos = append(infos, usecase.FeedInfo{Name: f.Name, Description: f.Description, Batches: len(f.Batches)})
	}
	return infos, nil
}

// FetchBatch returns the batch at cursor. The cursor is the batch index; an
// empty cursor means the first batch.
func (s *Supplier) FetchBatch(ctx context.Context, feedName, cursor string) (usecase.SupplierBatch, error) {
	if err := ctx.Err(); err != nil {
		return usecase.SupplierBatch{}, err
	}
	s.mu.RLock()
	feed, ok := s.feeds[feedName]
	outage := s.outageFromBatch
	s.mu.RUnlock()
	if !ok {
		return usecase.SupplierBatch{}, fmt.Errorf("unknown supplier update %q", feedName)
	}

	idx := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return usecase.SupplierBatch{}, fmt.Errorf("invalid cursor %q", cursor)
		}
		idx = n
	}
	if outage > 0 && idx+1 >= outage {
		return usecase.SupplierBatch{}, ErrUnavailable
	}
	if len(feed.Batches) == 0 {
		return usecase.SupplierBatch{ID: "empty", NextCursor: "0", Last: true}, nil
	}
	if idx >= len(feed.Batches) {
		return usecase.SupplierBatch{}, fmt.Errorf("cursor %q is past the end of %q", cursor, feedName)
	}

	b := feed.Batches[idx]
	records := make([]domain.SupplierRecord, len(b.Products))
	for i, r := range b.Products {
		records[i] = r.toDomain()
	}
	return usecase.SupplierBatch{
		ID:         b.ID,
		Records:    records,
		NextCursor: strconv.Itoa(idx + 1),
		Last:       idx == len(feed.Batches)-1,
	}, nil
}

func (r Record) toDomain() domain.SupplierRecord {
	// An unparsable timestamp becomes the zero time, which validation rejects.
	at, _ := time.Parse(time.RFC3339, r.UpdatedAt)
	return domain.SupplierRecord{
		SKU:        r.SKU,
		Name:       r.Name,
		Category:   r.Category,
		PriceCents: r.PriceCents,
		Status:     r.Status,
		UpdatedAt:  at.UTC(),
	}
}
