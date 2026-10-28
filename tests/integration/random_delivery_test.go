package integration

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"catalogsync/internal/adapter/supplier/mock"
	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

// TestRandomDeliveryConvergesToLatestSupplierState generates a product
// history, delivers it shuffled, repeated, split across random batches, with a
// crash and a supplier outage along the way, and checks that the catalog ends
// up exactly as the newest valid supplier record says.
func TestRandomDeliveryConvergesToLatestSupplierState(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprint("seed=", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			history := randomHistory(rng)
			batches := randomBatches(rng, history)

			crashBatch := 1 + rng.Intn(len(batches))
			e := newEnvWith(t, crashAt(crashBatch, 1), feed("random", batches...))
			run := e.runFeed("random")

			e.restart(usecase.SyncOptions{})
			if crashBatch < len(batches) {
				e.supplier.SetOutage(crashBatch + 1 + rng.Intn(len(batches)-crashBatch))
			}
			for attempt := 0; !e.run(run.ID).IsCompleted(); attempt++ {
				if attempt > 3 {
					t.Fatalf("update never completed: %+v", e.run(run.ID))
				}
				if attempt > 0 {
					e.supplier.SetOutage(0)
				}
				e.resume(run.ID)
			}

			assertLatestState(t, e.products(), history)
			e.assertInvariants()
		})
	}
}

// randomHistory returns several versions of a few products. Timestamps of a
// product are distinct; some versions are withdrawals, a few are invalid.
func randomHistory(rng *rand.Rand) []mock.Record {
	var history []mock.Record
	for p := 1; p <= 5; p++ {
		sku := fmt.Sprintf("ACM-R%d", p)
		days := rng.Perm(28)[:1+rng.Intn(5)]
		for v, d := range days {
			switch r := rng.Intn(10); {
			case r < 2:
				history = append(history, withdrawn(sku, d+1))
			case r < 3:
				history = append(history, active(sku, "Broken", -1, d+1)) // invalid
			default:
				history = append(history, active(sku, fmt.Sprintf("%s v%d", sku, v), int64(100+rng.Intn(900)), d+1))
			}
		}
	}
	return history
}

// randomBatches shuffles the history, repeats some records and splits it.
func randomBatches(rng *rand.Rand, history []mock.Record) []mock.Batch {
	records := append([]mock.Record(nil), history...)
	for _, r := range history {
		if rng.Intn(3) == 0 {
			records = append(records, r)
		}
	}
	rng.Shuffle(len(records), func(i, j int) { records[i], records[j] = records[j], records[i] })

	var batches []mock.Batch
	for len(records) > 0 {
		n := min(1+rng.Intn(4), len(records))
		batches = append(batches, batch(fmt.Sprintf("rb-%d", len(batches)+1), records[:n]...))
		records = records[n:]
	}
	return batches
}

func assertLatestState(t *testing.T, products map[string]domain.Product, history []mock.Record) {
	t.Helper()
	latest := map[string]mock.Record{}
	for _, r := range history {
		if r.Status == "active" && r.PriceCents < 0 {
			continue // rejected record
		}
		if cur, ok := latest[r.SKU]; !ok || r.UpdatedAt > cur.UpdatedAt {
			latest[r.SKU] = r
		}
	}
	if len(products) != len(latest) {
		t.Fatalf("got %d products, want %d", len(products), len(latest))
	}
	for sku, want := range latest {
		got := products[sku]
		wantAt, _ := time.Parse(time.RFC3339, want.UpdatedAt)
		if !got.SourceUpdatedAt.Equal(wantAt) || string(got.Status) != map[string]string{"active": "ACTIVE", "withdrawn": "WITHDRAWN"}[want.Status] {
			t.Errorf("%s: got %s@%s, want %s@%s", sku, got.Status, got.SourceUpdatedAt.Format(time.RFC3339), want.Status, want.UpdatedAt)
			continue
		}
		if want.Status == "active" && (got.Name != want.Name || got.PriceCents != want.PriceCents) {
			t.Errorf("%s: got %q %d, want %q %d", sku, got.Name, got.PriceCents, want.Name, want.PriceCents)
		}
	}
}

func demoFeeds(t *testing.T) []mock.Feed {
	t.Helper()
	feeds, err := mock.DefaultFeeds()
	if err != nil {
		t.Fatal(err)
	}
	return feeds
}
