package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/classify"
)

// served returns events of responses as whoever served them hands them
// over: without an identifier, and with a time finer than a second.
func served(n int) []Event {
	at := time.Date(2026, time.November, 19, 12, 0, 0, 987_000_000, time.UTC)
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{
			LinkID: "spring", WorkspaceID: "acme", OccurredAt: at, Outcome: OutcomeRedirected, Method: "GET",
			Classification: classify.Classification{Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1},
		}
	}
	return events
}

// unsteady is an inbox that does not take the first events it is given, a
// number of times, as a database does when a write runs out of its time.
// It remembers how often it was asked for anything.
type unsteady struct {
	*fakeInbox
	refusals atomic.Int64
	asked    atomic.Int64
	counts   atomic.Int64
}

func (u *unsteady) Count(ctx context.Context, counts []Count) (int, error) {
	u.counts.Add(1)
	return u.fakeInbox.Count(ctx, counts)
}

func (u *unsteady) Accept(ctx context.Context, events []Event) (int, error) {
	u.asked.Add(1)
	if u.refusals.Add(-1) >= 0 {
		return 0, errAway
	}
	return u.fakeInbox.Accept(ctx, events)
}

var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

// A history has more events than a buffer holds at once. Deliver writes
// them all, each as the buffer writes any event: with an identifier, to
// the second, and after the count of its response.
func TestDeliverWritesEveryEventAsABufferDoes(t *testing.T) {
	inbox := &fakeInbox{}
	const total = Capacity + BatchSize + 7
	if err := Deliver(t.Context(), inbox, discard, served(total)); err != nil {
		t.Fatal(err)
	}
	calls, sizes, _ := inbox.said()
	accepted := 0
	for _, size := range sizes {
		accepted += size
		if size > BatchSize {
			t.Fatalf("a batch of %d events was written, over the %d of a batch", size, BatchSize)
		}
	}
	if accepted != total || inbox.counted(key("2026-11-19", "acme", counterServed)) != total {
		t.Errorf("%d events were accepted and %d responses counted as served, want %d of each",
			accepted, inbox.counted(key("2026-11-19", "acme", counterServed)), total)
	}
	if calls[0] != "count" || inbox.ahead {
		t.Errorf("the first write was %q, and events were accepted ahead of the count of their responses: %t", calls[0], inbox.ahead)
	}
	seen := make(map[string]bool, total)
	for _, batch := range inbox.batches {
		for _, event := range batch {
			if event.ID == "" || seen[event.ID] || event.OccurredAt.Nanosecond() != 0 {
				t.Fatalf("an event was written with the identifier %q, twice or not at all, at %v", event.ID, event.OccurredAt)
			}
			seen[event.ID] = true
		}
	}
	// Nothing to deliver is nothing asked of the inbox.
	empty := &fakeInbox{}
	if err := Deliver(t.Context(), empty, discard, nil); err != nil || len(empty.calls) != 0 {
		t.Errorf("delivering nothing = %v, with %d calls to the inbox", err, len(empty.calls))
	}
}

// A write that ran out of its time is asked again, and what it did not
// take is written once: a busy machine does not fail a seed. A database
// that is away fails it, at the fifth try. The number is written here a
// second time on purpose: the test does not take it from what it checks.
func TestDeliverAsksAgainAFewTimesAndThenGivesUp(t *testing.T) {
	const (
		total = 2*BatchSize + 3
		tries = 5
	)
	inbox := &unsteady{fakeInbox: &fakeInbox{}}
	inbox.refusals.Store(tries - 1)
	if err := Deliver(t.Context(), inbox, discard, served(total)); err != nil {
		t.Fatalf("with %d writes refused, Deliver = %v; want it to ask again", tries-1, err)
	}
	_, sizes, _ := inbox.said()
	accepted := 0
	for _, size := range sizes {
		accepted += size
	}
	if accepted != total || inbox.counted(key("2026-11-19", "acme", counterServed)) != total {
		t.Errorf("after the refusals %d events were accepted and %d responses counted, want %d of each, once",
			accepted, inbox.counted(key("2026-11-19", "acme", counterServed)), total)
	}

	away := &unsteady{fakeInbox: &fakeInbox{}}
	away.refusals.Store(1000)
	err := Deliver(t.Context(), away, discard, served(total))
	if !errors.Is(err, errAway) || away.asked.Load() != tries {
		t.Errorf("with the inbox away, Deliver = %v after %d writes; want its error after %d", err, away.asked.Load(), tries)
	}
	if _, sizes, _ := away.said(); len(sizes) != 0 {
		t.Errorf("an inbox that was away accepted %d batches", len(sizes))
	}

	// Whoever no longer waits does not ask again: one write, which fails.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	stopped := &unsteady{fakeInbox: &fakeInbox{}}
	if err := Deliver(gone, stopped, discard, served(total)); !errors.Is(err, context.Canceled) || stopped.counts.Load() != 1 || stopped.asked.Load() != 0 {
		t.Errorf("with its context done, Deliver = %v after %d writes of counts and %d of events; want it to stop at the first",
			err, stopped.counts.Load(), stopped.asked.Load())
	}
}
