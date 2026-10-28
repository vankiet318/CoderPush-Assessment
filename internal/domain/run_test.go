package domain

import (
	"errors"
	"testing"
)

func TestRunCompletesOnlyOnLastBatch(t *testing.T) {
	run := NewRun("feed", at(1))
	if err := run.AdvanceBatch("1", false, at(2)); err != nil {
		t.Fatal(err)
	}
	if run.IsCompleted() || !run.FinishedAt.IsZero() {
		t.Fatalf("run completed before the last batch: %+v", run)
	}
	if err := run.AdvanceBatch("2", true, at(3)); err != nil {
		t.Fatal(err)
	}
	if !run.IsCompleted() || !run.FinishedAt.Equal(at(3)) || run.Counters.Batches != 2 || run.Cursor != "2" {
		t.Fatalf("unexpected run after last batch: %+v", run)
	}
}

func TestRunTransitions(t *testing.T) {
	run := NewRun("feed", at(1))
	if err := run.Resume(at(2)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resuming a running update: got %v, want ErrInvalidTransition", err)
	}
	if err := run.Fail("supplier down", at(2)); err != nil {
		t.Fatal(err)
	}
	if !run.Resumable() || run.LastError != "supplier down" {
		t.Fatalf("failed run should be resumable with its error: %+v", run)
	}
	if err := run.Resume(at(3)); err != nil {
		t.Fatal(err)
	}
	if !run.IsRunning() || run.LastError != "" {
		t.Fatalf("resumed run: %+v", run)
	}
	if err := run.Interrupt("stopped", at(4)); err != nil {
		t.Fatal(err)
	}
	if err := run.Resume(at(5)); err != nil {
		t.Fatal(err)
	}
	if err := run.AdvanceBatch("1", true, at(6)); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"fail":      run.Fail("x", at(7)),
		"interrupt": run.Interrupt("x", at(7)),
		"resume":    run.Resume(at(7)),
		"advance":   run.AdvanceBatch("2", true, at(7)),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s on a completed run: got %v, want ErrInvalidTransition", name, err)
		}
	}
}

func TestCountersAddUp(t *testing.T) {
	var c RunCounters
	for _, k := range []DecisionKind{DecisionCreated, DecisionUpdated, DecisionWithdrawn, DecisionReactivated,
		DecisionUnchanged, DecisionIgnoredStale, DecisionInvalid} {
		c.Count(k)
	}
	want := RunCounters{Records: 7, Created: 1, Updated: 1, Withdrawn: 1, Reactivated: 1, Unchanged: 1, Stale: 1, Invalid: 1}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestIssueFingerprintIdentifiesTheRecord(t *testing.T) {
	rec := activeRec("A1", "Can", -1, 5)
	a := NewInvalidRecordIssue(1, "feed", "b1", 0, rec, "negative price", at(6))
	b := NewInvalidRecordIssue(2, "feed", "b9", 3, rec, "negative price", at(7))
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("same record in another run must have the same fingerprint: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
	newer := NewInvalidRecordIssue(1, "feed", "b1", 0, activeRec("A1", "Can", -1, 6), "negative price", at(6))
	if newer.Fingerprint == a.Fingerprint {
		t.Fatal("a newer bad version is a new problem and needs its own fingerprint")
	}
	if err := a.Resolve(at(8)); err != nil {
		t.Fatal(err)
	}
	if err := a.Resolve(at(9)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("closing twice: got %v", err)
	}
}
