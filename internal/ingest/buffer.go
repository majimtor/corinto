package ingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The parameters of the first hop. docs/delivery-semantics.md states them,
// and a test fails when the two differ.
const (
	// Capacity is how many events wait in the buffer at most. One that does
	// not fit is dropped.
	Capacity = 50_000
	// FlushEvery is how often what waits is written to the inbox.
	FlushEvery = time.Second
	// BatchSize is how many waiting events are written without waiting for
	// FlushEvery, and the most that one insert carries.
	BatchSize = 500

	// writeTimeout bounds one write to the inbox. A database that does not
	// answer must not hold the writer for longer than a few flushes: what
	// it holds is tried again at the next one.
	writeTimeout = 5 * time.Second
)

// Counter names a count taken along the path of an event.
type Counter string

// The counters that are counted in the memory of a process. The count of
// the events accepted is not one of them: the insert counts it.
const (
	// CounterNotFound counts the requests that named no link: a deleted or
	// unknown one, or a path outside the grammar.
	CounterNotFound Counter = "not_found"
	// CounterRateLimited counts the requests over the rate limit.
	CounterRateLimited Counter = "rate_limited"
	// counterServed counts the responses that owe an event. Record counts
	// it, whether or not the event fits in the buffer.
	counterServed Counter = "served"
)

// Count is an amount to add to a counter of a UTC day.
type Count struct {
	// Day is the midnight, in UTC, that begins the day.
	Day time.Time
	// WorkspaceID is empty for a counter that is of no workspace.
	WorkspaceID string
	Name        Counter
	Value       int64
}

// Inbox is where a buffer writes what it holds. *Store is the one of the
// processes.
//
// Both methods return how many of the things they were given, from the
// first, are settled: written, or refused by the database for what they
// hold and dropped. They return an error when the rest could not be tried,
// and the rest is tried again later. Neither keeps nor changes what it is
// given.
type Inbox interface {
	Accept(ctx context.Context, events []Event) (int, error)
	Count(ctx context.Context, counts []Count) (int, error)
}

// tally is the key of a counter in memory.
type tally struct {
	// day is the number of the UTC day, counted from 1970.
	day       int64
	workspace string
	name      Counter
}

const secondsInADay = 24 * 60 * 60

// errLeftUnsaid is the failure of an inbox that settles less than it was
// given and returns no error. The events it left wait like any others.
var errLeftUnsaid = errors.New("the inbox left events unsettled without an error")

// Buffer holds the events between the response that owes them and the
// inbox, and the counts taken in the memory of the process. Taking an
// event never waits for the inbox: the response that owes it was already
// decided, and is not held back.
type Buffer struct {
	inbox  Inbox
	logger *slog.Logger
	// batch is told when BatchSize events wait.
	batch chan struct{}

	mu sync.Mutex
	// waiting holds the events not yet settled, the oldest first. A batch
	// stays in it until the inbox has settled it.
	waiting []Event
	counts  map[tally]int64
	// dropped counts the events that did not fit since the last flush. It
	// is for the log alone: the loss that is published is the difference
	// between what was served and what was accepted.
	dropped int64

	// failing is whether the last flush failed. Only Run touches it.
	failing bool
}

// NewBuffer returns an empty buffer that writes to an inbox. Run empties
// it.
func NewBuffer(inbox Inbox, logger *slog.Logger) *Buffer {
	return &Buffer{inbox: inbox, logger: logger, batch: make(chan struct{}, 1), counts: make(map[tally]int64)}
}

// Record takes the event of a response that was served. The response is
// counted as served whatever happens next; the event waits for the next
// flush, or is dropped when Capacity events already wait. Record performs
// no input or output, and does not wait for whoever writes.
func (b *Buffer) Record(event Event) {
	event.OccurredAt = event.OccurredAt.Truncate(time.Second)
	event.ID = newID(event.OccurredAt)

	b.mu.Lock()
	b.counts[tally{event.OccurredAt.Unix() / secondsInADay, event.WorkspaceID, counterServed}]++
	if len(b.waiting) >= Capacity {
		b.dropped++
		b.mu.Unlock()
		return
	}
	b.waiting = append(b.waiting, event)
	whole := len(b.waiting) == BatchSize
	b.mu.Unlock()

	// Told once, when the batch becomes whole. While the inbox does not
	// accept events more than a batch waits, and it is tried again every
	// FlushEvery, not with every event.
	if whole {
		select {
		case b.batch <- struct{}{}:
		default:
		}
	}
}

// Count adds one to a counter that is of no workspace, on the day of a
// moment.
func (b *Buffer) Count(name Counter, at time.Time) {
	b.mu.Lock()
	b.counts[tally{at.Unix() / secondsInADay, "", name}]++
	b.mu.Unlock()
}

// Run writes what the buffer holds every FlushEvery, and as soon as
// BatchSize events wait, until its context is done.
func (b *Buffer) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.batch:
		}
		b.flush(ctx)
	}
}

// flush writes the counts and then the events that wait, in batches. What
// the inbox does not take stays where it was.
func (b *Buffer) flush(ctx context.Context) {
	// The counts and the events are taken at one instant, and the counts
	// are written first: an event is never in the inbox before the count
	// of the response that owed it, so what was accepted never exceeds
	// what was served.
	b.mu.Lock()
	counts := make([]Count, 0, len(b.counts))
	for key, value := range b.counts {
		counts = append(counts, Count{
			Day: time.Unix(key.day*secondsInADay, 0).UTC(), WorkspaceID: key.workspace, Name: key.name, Value: value,
		})
	}
	clear(b.counts)
	remaining := len(b.waiting)
	dropped := b.dropped
	b.dropped = 0
	b.mu.Unlock()

	if dropped > 0 {
		b.logger.WarnContext(ctx, "the buffer was full and events were dropped", "dropped", dropped)
	}
	err := b.write(ctx, counts, remaining)
	switch {
	case err != nil && ctx.Err() != nil:
		// The process is stopping: the write was cut off, not refused.
	case err != nil && !b.failing:
		b.logger.WarnContext(ctx, "the inbox did not take what the buffer holds, which waits", "error", err.Error())
	case err == nil && b.failing:
		b.logger.InfoContext(ctx, "the inbox takes events again")
	}
	b.failing = err != nil
}

// write writes counts and then the first events that wait, up to a
// number of them.
func (b *Buffer) write(ctx context.Context, counts []Count, remaining int) error {
	if len(counts) > 0 {
		settled, err := b.count(ctx, counts)
		if err != nil {
			// The counts not written go back to where they were taken
			// from, and the events wait for them.
			b.mu.Lock()
			for _, count := range counts[settled:] {
				b.counts[tally{count.Day.Unix() / secondsInADay, count.WorkspaceID, count.Name}] += count.Value
			}
			b.mu.Unlock()
			return err
		}
	}
	for remaining > 0 {
		b.mu.Lock()
		size := min(remaining, BatchSize)
		batch := b.waiting[:size:size]
		b.mu.Unlock()

		settled, err := b.accept(ctx, batch)
		b.mu.Lock()
		b.waiting = b.waiting[settled:]
		b.mu.Unlock()
		remaining -= settled
		if err != nil {
			return err
		}
		if settled < size {
			return errLeftUnsaid
		}
	}
	return nil
}

func (b *Buffer) count(ctx context.Context, counts []Count) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return b.inbox.Count(ctx, counts)
}

func (b *Buffer) accept(ctx context.Context, events []Event) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return b.inbox.Accept(ctx, events)
}
