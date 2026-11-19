package ingest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"
)

var errAway = errors.New("the database is away")

// fakeInbox is an inbox in memory that fails as a test tells it to, and
// remembers what it was asked and in which order.
type fakeInbox struct {
	mu sync.Mutex
	// countErr and acceptErr, when set, are what every Count and every
	// Accept answer, having written nothing.
	countErr, acceptErr error
	// countsLeft, when set, is how many counts the next Count writes before
	// it fails; it is used once.
	countsLeft *int
	// held and heldCounts, when set, are what Accept and Count wait for
	// before they answer.
	held, heldCounts chan struct{}
	// short makes Accept settle nothing and return no error.
	short bool
	// duringCount, when set, runs once inside the next Count.
	duringCount func()

	calls   []string
	batches [][]Event
	counts  map[string]int64
	served  int64
	// ahead is set when more events were accepted than responses counted
	// as served.
	ahead bool
}

func (f *fakeInbox) set(change func(*fakeInbox)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// key is how the tests name a counter: its day, its workspace and its name.
func key(day, workspace string, name Counter) string {
	return day + "/" + workspace + "/" + string(name)
}

func (f *fakeInbox) Count(ctx context.Context, counts []Count) (int, error) {
	f.mu.Lock()
	held, during := f.heldCounts, f.duringCount
	f.duringCount = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			f.set(func(f *fakeInbox) { f.calls = append(f.calls, "count") })
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "count")
	if f.countErr != nil {
		return 0, f.countErr
	}
	settled := len(counts)
	if f.countsLeft != nil {
		settled, f.countsLeft = *f.countsLeft, nil
	}
	if f.counts == nil {
		f.counts = make(map[string]int64)
	}
	for _, count := range counts[:settled] {
		f.counts[key(count.Day.Format(time.DateOnly), count.WorkspaceID, count.Name)] += count.Value
		if count.Name == counterServed {
			f.served += count.Value
		}
	}
	if settled < len(counts) {
		return settled, errAway
	}
	return settled, nil
}

func (f *fakeInbox) Accept(ctx context.Context, events []Event) (int, error) {
	f.mu.Lock()
	held := f.held
	f.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			f.set(func(f *fakeInbox) { f.calls = append(f.calls, "accept") })
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "accept")
	if f.acceptErr != nil {
		return 0, f.acceptErr
	}
	if f.short {
		// A writer that asks again and again is stopped here, not by the
		// time limit of the tests.
		if len(f.calls) > 10 {
			panic("the inbox is asked for the same batch without end")
		}
		return 0, nil
	}
	f.batches = append(f.batches, slices.Clone(events))
	accepted := 0
	for _, batch := range f.batches {
		accepted += len(batch)
	}
	f.ahead = f.ahead || int64(accepted) > f.served
	return len(events), nil
}

// said returns what the inbox was asked, in order, and the links of the
// events it accepted, in order.
func (f *fakeInbox) said() (calls []string, sizes []int, links []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, batch := range f.batches {
		sizes = append(sizes, len(batch))
		for _, event := range batch {
			links = append(links, event.LinkID)
		}
	}
	return slices.Clone(f.calls), sizes, links
}

func (f *fakeInbox) counted(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

// lockedLog is a log that a test reads while a buffer writes it.
type lockedLog struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

// lines returns the entries of the log whose message is one.
func (l *lockedLog) lines(t *testing.T, message string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	for line := range strings.Lines(l.text.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a line of the log is not JSON: %s", line)
		}
		if entry["msg"] == message {
			found = append(found, entry)
		}
	}
	return found
}

const (
	workspaceA = "0193a000-0000-7000-8000-00000000000a"
	workspaceB = "0193a000-0000-7000-8000-00000000000b"
)

// noon is a moment in the middle of a UTC day.
var noon = time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)

// record hands a buffer events numbered from a first one, each with its
// number as its link.
func record(buffer *Buffer, first, events int) {
	for i := first; i < first+events; i++ {
		buffer.Record(Event{LinkID: strconv.Itoa(i), WorkspaceID: workspaceA, OccurredAt: noon})
	}
}

// numbered returns the links of events numbered from zero.
func numbered(events int) []string {
	links := make([]string, events)
	for i := range links {
		links[i] = strconv.Itoa(i)
	}
	return links
}

func waiting(buffer *Buffer) int {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return len(buffer.waiting)
}

// newBuffer returns a buffer over an inbox, and its log.
func newBuffer(into Inbox) (*Buffer, *lockedLog) {
	log := &lockedLog{}
	return NewBuffer(into, slog.New(slog.NewJSONHandler(log, nil))), log
}

// running returns a buffer that is being emptied, its log, and what stops
// it. It is for tests inside a bubble, where time is the test's.
func running(t *testing.T, into Inbox) (*Buffer, *lockedLog, func()) {
	t.Helper()
	buffer, log := newBuffer(into)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer.Run(ctx)
	}()
	return buffer, log, func() {
		cancel()
		<-done
	}
}

const (
	messageDropped = "the buffer was full and events were dropped"
	messageFailing = "the inbox did not take what the buffer holds, which waits"
	messageBack    = "the inbox takes events again"
)

// docs/delivery-semantics.md is the only source of the parameters. Whoever
// changes one changes it there, and here.
func TestTheParametersAreTheOnesTheDocumentStates(t *testing.T) {
	if Capacity != 50_000 || FlushEvery != time.Second || BatchSize != 500 {
		t.Errorf("a buffer of %d, a flush every %s or every %d events", Capacity, FlushEvery, BatchSize)
	}
	document, err := os.ReadFile("../../docs/delivery-semantics.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| Buffer | 50,000 events |", "| Flush | Every second, or every 500 events |"} {
		if !strings.Contains(string(document), row) {
			t.Errorf("docs/delivery-semantics.md does not have the row %q", row)
		}
	}
}

// Statement 2: every second or every 500 events, whichever comes first.
func TestTheBufferFlushesByCountAndByInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &fakeInbox{}
		buffer, _, stop := running(t, into)
		defer stop()

		// A few events wait for the interval, and for the whole of it.
		record(buffer, 0, 3)
		time.Sleep(FlushEvery - time.Millisecond)
		synctest.Wait()
		if _, sizes, _ := into.said(); len(sizes) != 0 {
			t.Fatalf("before the interval the inbox got batches of %v", sizes)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if _, sizes, _ := into.said(); !slices.Equal(sizes, []int{3}) {
			t.Fatalf("at the interval the inbox got batches of %v, want one of 3", sizes)
		}

		// One event short of a batch waits; the one that completes it does
		// not, and no time has passed.
		record(buffer, 3, BatchSize-1)
		synctest.Wait()
		if _, sizes, _ := into.said(); !slices.Equal(sizes, []int{3}) {
			t.Fatalf("one event short of a batch the inbox got batches of %v", sizes)
		}
		record(buffer, 3+BatchSize-1, 1)
		synctest.Wait()
		if _, sizes, _ := into.said(); !slices.Equal(sizes, []int{3, BatchSize}) {
			t.Fatalf("with a whole batch the inbox got batches of %v, want 3 and %d", sizes, BatchSize)
		}

		// More than a batch goes in several, none larger than one.
		record(buffer, 3+BatchSize, 2*BatchSize+200)
		time.Sleep(FlushEvery)
		synctest.Wait()
		_, sizes, links := into.said()
		if slices.Max(sizes) > BatchSize {
			t.Errorf("the inbox got batches of %v, one of them larger than %d", sizes, BatchSize)
		}
		if !slices.Equal(links, numbered(3+3*BatchSize+200)) {
			t.Errorf("the inbox got %d events, not each of the %d once and in order", len(links), 3+3*BatchSize+200)
		}
		if waiting(buffer) != 0 {
			t.Errorf("%d events still wait", waiting(buffer))
		}
	})
}

// Statement 2: a batch that the database does not accept stays in the
// buffer and is tried again at the next flush.
func TestABatchTheInboxDoesNotTakeStaysAndIsTriedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &fakeInbox{acceptErr: errAway}
		buffer, log, stop := running(t, into)
		defer stop()

		record(buffer, 0, BatchSize+200)
		synctest.Wait()
		if calls, _, _ := into.said(); !slices.Equal(calls, []string{"count", "accept"}) {
			t.Fatalf("with a whole batch waiting the inbox was asked %v, want its counts and one batch", calls)
		}
		// More events arrive while the inbox fails. It is tried again with
		// the interval, not with every event.
		record(buffer, BatchSize+200, 100)
		synctest.Wait()
		if calls, _, _ := into.said(); len(calls) != 2 {
			t.Fatalf("events that arrive while the inbox fails ask it again: %v", calls)
		}
		time.Sleep(3 * FlushEvery)
		synctest.Wait()
		// Each flush writes the counts taken since the last one, and then
		// tries the batch that waits.
		calls, sizes, _ := into.said()
		if want := []string{"count", "accept", "count", "accept", "accept", "accept"}; !slices.Equal(calls, want) {
			t.Errorf("in three intervals the inbox was asked %v, want %v", calls, want)
		}
		if len(sizes) != 0 || waiting(buffer) != BatchSize+300 {
			t.Fatalf("the inbox took batches of %v and %d events wait, want none and %d", sizes, waiting(buffer), BatchSize+300)
		}

		into.set(func(f *fakeInbox) { f.acceptErr = nil })
		time.Sleep(FlushEvery)
		synctest.Wait()
		if _, _, links := into.said(); !slices.Equal(links, numbered(BatchSize+300)) {
			t.Errorf("once the inbox answers it got %d events, not each of the %d once and in order", len(links), BatchSize+300)
		}
		// What was served was counted once, however many times the events
		// were tried.
		if served := into.counted(key("2026-11-19", workspaceA, counterServed)); served != BatchSize+300 {
			t.Errorf("%d responses were counted as served, want %d", served, BatchSize+300)
		}
		// The outage is in the log once, and so is its end.
		if failing, back := log.lines(t, messageFailing), log.lines(t, messageBack); len(failing) != 1 || len(back) != 1 {
			t.Errorf("the log says %d times that the inbox fails and %d that it is back, want once each", len(failing), len(back))
		}
	})
}

// Statement 3: the event that does not fit is dropped, the ones already
// waiting are kept, and the loss is the difference between two counts.
func TestAFullBufferDropsTheNewEventAndCountsIt(t *testing.T) {
	into := &fakeInbox{countErr: errAway, acceptErr: errAway}
	buffer, log := newBuffer(into)

	record(buffer, 0, Capacity+10)
	if waiting(buffer) != Capacity {
		t.Fatalf("%d events wait, want the %d that fit", waiting(buffer), Capacity)
	}
	buffer.flush(t.Context())
	if waiting(buffer) != Capacity {
		t.Fatalf("after a flush that failed %d events wait, want %d", waiting(buffer), Capacity)
	}

	into.set(func(f *fakeInbox) { f.countErr, f.acceptErr = nil, nil })
	buffer.flush(t.Context())
	_, _, links := into.said()
	if !slices.Equal(links, numbered(Capacity)) {
		t.Errorf("the inbox got %d events, not the first %d in order: the ones that waited are the ones kept", len(links), Capacity)
	}
	served := into.counted(key("2026-11-19", workspaceA, counterServed))
	if served != Capacity+10 || served-int64(len(links)) != 10 {
		t.Errorf("%d served and %d accepted: the difference is not the 10 that were dropped", served, len(links))
	}
	// The log says it once, for whoever watches the process.
	dropped := log.lines(t, messageDropped)
	if len(dropped) != 1 || dropped[0]["dropped"] != float64(10) {
		t.Errorf("the log says %v, want once that 10 events were dropped", dropped)
	}
	// A buffer with room again takes events again.
	record(buffer, Capacity+10, 1)
	if waiting(buffer) != 1 {
		t.Errorf("after the buffer was emptied %d events wait, want the new one", waiting(buffer))
	}
}

// Statement 3, under the race detector: many responses at once while the
// buffer is being emptied.
func TestConcurrentWritersLoseNoEventThatFits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &fakeInbox{}
		buffer, _, stop := running(t, into)
		defer stop()

		const writers, each = 8, 2500
		var wait sync.WaitGroup
		for writer := range writers {
			wait.Go(func() {
				for i := range each {
					buffer.Record(Event{LinkID: fmt.Sprintf("%d-%d", writer, i), WorkspaceID: workspaceA, OccurredAt: noon})
				}
			})
		}
		wait.Wait()
		time.Sleep(FlushEvery)
		synctest.Wait()

		_, sizes, links := into.said()
		slices.Sort(links)
		if len(links) != writers*each || len(slices.Compact(links)) != writers*each {
			t.Errorf("the inbox got %d events, %d of them different, want each of the %d once", len(links), len(slices.Compact(links)), writers*each)
		}
		if slices.Max(sizes) > BatchSize {
			t.Errorf("a batch of %d events, larger than %d", slices.Max(sizes), BatchSize)
		}
		if served := into.counted(key("2026-11-19", workspaceA, counterServed)); served != writers*each {
			t.Errorf("%d responses were counted as served, want %d", served, writers*each)
		}
		into.mu.Lock()
		defer into.mu.Unlock()
		if into.ahead {
			t.Error("at some moment more events were accepted than responses counted as served")
		}
	})
}

// Statement 1, on this side of it: taking an event does not wait for
// whoever writes.
func TestTakingAnEventDoesNotWaitForTheInbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &fakeInbox{held: make(chan struct{})}
		buffer, _, stop := running(t, into)
		defer stop()

		// A whole batch starts a write, which does not come back.
		record(buffer, 0, BatchSize)
		synctest.Wait()
		record(buffer, BatchSize, 10)
		// The batch being written stays until the inbox has settled it.
		if waiting(buffer) != BatchSize+10 {
			t.Fatalf("%d events wait while a batch is being written, want %d", waiting(buffer), BatchSize+10)
		}
		close(into.held)
		synctest.Wait()
		if _, sizes, _ := into.said(); !slices.Equal(sizes, []int{BatchSize}) || waiting(buffer) != 10 {
			t.Errorf("once the write came back: batches of %v and %d events waiting, want one of %d and 10", sizes, waiting(buffer), BatchSize)
		}
	})
}

// A database that does not answer holds the writer for a bounded time, and
// what it held is tried again.
func TestAnInboxThatDoesNotAnswerIsGivenUpAndTriedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &fakeInbox{held: make(chan struct{})}
		buffer, log, stop := running(t, into)
		defer stop()

		record(buffer, 0, 3)
		time.Sleep(FlushEvery)
		synctest.Wait()
		if calls, _, _ := into.said(); !slices.Equal(calls, []string{"count"}) {
			t.Fatalf("the inbox was asked %v, want its counts and a batch that has not come back", calls)
		}
		time.Sleep(writeTimeout)
		synctest.Wait()
		if calls, _, _ := into.said(); !slices.Equal(calls, []string{"count", "accept"}) || waiting(buffer) != 3 {
			t.Fatalf("after %s the inbox was asked %v and %d events wait, want the batch given up and 3 waiting", writeTimeout, calls, waiting(buffer))
		}
		if failing := log.lines(t, messageFailing); len(failing) != 1 {
			t.Errorf("the log says %d times that the inbox fails, want once", len(failing))
		}

		close(into.held)
		time.Sleep(FlushEvery)
		synctest.Wait()
		if _, _, links := into.said(); !slices.Equal(links, numbered(3)) {
			t.Errorf("once the inbox answers it got %v, want the three events", links)
		}

		// The same with the counts: they are given up, and written once.
		into.set(func(f *fakeInbox) { f.heldCounts = make(chan struct{}) })
		record(buffer, 3, 2)
		time.Sleep(FlushEvery + writeTimeout)
		synctest.Wait()
		if calls, _, _ := into.said(); !slices.Equal(calls[len(calls)-2:], []string{"accept", "count"}) || waiting(buffer) != 2 {
			t.Fatalf("after %s the inbox was asked %v and %d events wait, want the counts given up and 2 waiting", writeTimeout, calls, waiting(buffer))
		}
		into.set(func(f *fakeInbox) { close(f.heldCounts); f.heldCounts = nil })
		time.Sleep(FlushEvery)
		synctest.Wait()
		if served := into.counted(key("2026-11-19", workspaceA, counterServed)); served != 5 || waiting(buffer) != 0 {
			t.Errorf("%d responses were counted as served and %d events wait, want 5 and none", served, waiting(buffer))
		}
	})
}

// A response served while a flush is on its way has its count in the next
// one, and its event waits for it.
func TestAnEventServedDuringAFlushWaitsForItsCount(t *testing.T) {
	into := &fakeInbox{}
	buffer, _ := newBuffer(into)
	record(buffer, 0, 3)
	into.duringCount = func() { record(buffer, 3, 1) }

	buffer.flush(t.Context())
	if _, _, links := into.said(); !slices.Equal(links, numbered(3)) || waiting(buffer) != 1 {
		t.Fatalf("the inbox got the events %v and %d wait, want the three served before the flush and one waiting", links, waiting(buffer))
	}
	buffer.flush(t.Context())
	if _, _, links := into.said(); !slices.Equal(links, numbered(4)) {
		t.Errorf("after the next flush the inbox got the events %v, want the four", links)
	}
	into.mu.Lock()
	defer into.mu.Unlock()
	if into.ahead {
		t.Error("at some moment more events were accepted than responses counted as served")
	}
}

// The buffer gives an event its identifier and keeps its time to the
// second, whatever it was handed.
func TestAnEventIsGivenItsIdentifierAndKeptToTheSecond(t *testing.T) {
	into := &fakeInbox{}
	buffer, _ := newBuffer(into)
	at := time.Date(2026, time.November, 19, 12, 30, 45, 987_654_321, time.FixedZone("UTC+1", 60*60))
	buffer.Record(Event{ID: "given by whoever served", LinkID: "0", WorkspaceID: workspaceA, OccurredAt: at})
	buffer.Record(Event{LinkID: "1", WorkspaceID: workspaceA, OccurredAt: at})
	buffer.flush(t.Context())

	into.mu.Lock()
	defer into.mu.Unlock()
	if len(into.batches) != 1 || len(into.batches[0]) != 2 {
		t.Fatalf("the inbox got %v", into.batches)
	}
	first, second := into.batches[0][0], into.batches[0][1]
	want := time.Date(2026, time.November, 19, 11, 30, 45, 0, time.UTC)
	for _, event := range []Event{first, second} {
		if !event.OccurredAt.Equal(want) || event.OccurredAt.Nanosecond() != 0 {
			t.Errorf("the event occurred at %s, want %s", event.OccurredAt.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
		parsed, err := uuid.Parse(event.ID)
		if err != nil {
			t.Fatalf("the identifier %q is not a UUID: %v", event.ID, err)
		}
		if milliseconds := binary.BigEndian.Uint64(append([]byte{0, 0}, parsed[:6]...)); milliseconds != uint64(want.UnixMilli()) {
			t.Errorf("the identifier %s carries the time %d, want %d", event.ID, milliseconds, want.UnixMilli())
		}
	}
	if first.ID == second.ID {
		t.Errorf("two events have the identifier %s", first.ID)
	}
}

// The counts are written before the events, and the events wait for them:
// what was accepted never exceeds what was served. A count is written
// once, whatever happens to the ones written with it.
func TestTheCountsGoBeforeTheEventsTheyCount(t *testing.T) {
	one := 1
	into := &fakeInbox{countsLeft: &one}
	buffer, _ := newBuffer(into)

	buffer.Record(Event{LinkID: "0", WorkspaceID: workspaceA, OccurredAt: noon})
	buffer.Record(Event{LinkID: "1", WorkspaceID: workspaceB, OccurredAt: noon})
	buffer.Record(Event{LinkID: "2", WorkspaceID: workspaceB, OccurredAt: noon})
	buffer.Count(CounterNotFound, noon)

	// The first count is written and then the inbox fails.
	buffer.flush(t.Context())
	if calls, sizes, _ := into.said(); !slices.Equal(calls, []string{"count"}) || len(sizes) != 0 || waiting(buffer) != 3 {
		t.Fatalf("with its counts half written the inbox was asked %v and took batches of %v", calls, sizes)
	}
	buffer.flush(t.Context())
	if calls, sizes, _ := into.said(); !slices.Equal(calls, []string{"count", "count", "accept"}) || !slices.Equal(sizes, []int{3}) {
		t.Fatalf("the inbox was asked %v and took batches of %v, want the counts, then the batch of 3", calls, sizes)
	}
	for name, want := range map[string]int64{
		key("2026-11-19", "", CounterNotFound):       1,
		key("2026-11-19", workspaceA, counterServed): 1,
		key("2026-11-19", workspaceB, counterServed): 2,
	} {
		if got := into.counted(name); got != want {
			t.Errorf("the counter %s is at %d, want %d", name, got, want)
		}
	}
	// With nothing new there is nothing to write.
	buffer.flush(t.Context())
	if calls, _, _ := into.said(); len(calls) != 3 {
		t.Errorf("a flush with nothing to write asked the inbox: %v", calls)
	}
}

// A count is of the UTC day of its moment, and of the workspace of its
// event or of none.
func TestCountsAreOfTheirDayAndTheirWorkspace(t *testing.T) {
	// The day is the one of UTC wherever the process runs: here, eight
	// hours behind it.
	local := time.Local
	time.Local = time.FixedZone("UTC-8", -8*60*60)
	t.Cleanup(func() { time.Local = local })

	into := &fakeInbox{}
	buffer, _ := newBuffer(into)

	madrid := time.FixedZone("UTC+1", 60*60)
	lastSecond := time.Date(2026, time.November, 19, 23, 59, 59, 999_999_999, time.UTC)
	buffer.Record(Event{LinkID: "0", WorkspaceID: workspaceA, OccurredAt: lastSecond})
	buffer.Record(Event{LinkID: "1", WorkspaceID: workspaceA, OccurredAt: lastSecond.Add(time.Nanosecond)})
	// Half past midnight in a zone an hour ahead is still the day before.
	buffer.Record(Event{LinkID: "2", WorkspaceID: workspaceB, OccurredAt: time.Date(2026, time.November, 20, 0, 30, 0, 0, madrid)})
	buffer.Count(CounterRateLimited, lastSecond)
	buffer.Count(CounterRateLimited, lastSecond)
	buffer.Count(CounterNotFound, lastSecond.Add(time.Nanosecond))
	buffer.flush(t.Context())

	for name, want := range map[string]int64{
		key("2026-11-19", workspaceA, counterServed):   1,
		key("2026-11-20", workspaceA, counterServed):   1,
		key("2026-11-19", workspaceB, counterServed):   1,
		key("2026-11-19", "", CounterRateLimited):      2,
		key("2026-11-20", "", CounterNotFound):         1,
		key("2026-11-19", "", CounterNotFound):         0,
		key("2026-11-20", workspaceB, counterServed):   0,
		key("2026-11-19", workspaceA, CounterNotFound): 0,
	} {
		if got := into.counted(name); got != want {
			t.Errorf("the counter %s is at %d, want %d", name, got, want)
		}
	}
}

// An inbox that settles less than it was given and gives no reason must
// not turn the writer into a loop.
func TestAnInboxThatLeavesEventsWithoutAnErrorIsAFailure(t *testing.T) {
	into := &fakeInbox{short: true}
	buffer, log := newBuffer(into)
	record(buffer, 0, 3)
	buffer.flush(t.Context())
	failing := log.lines(t, messageFailing)
	if calls, _, _ := into.said(); !slices.Equal(calls, []string{"count", "accept"}) || waiting(buffer) != 3 || len(failing) != 1 {
		t.Fatalf("the inbox was asked %v, %d events wait and the log says %v", calls, waiting(buffer), failing)
	}
	if failing[0]["error"] != errLeftUnsaid.Error() {
		t.Errorf("the log says %v, want %q", failing[0]["error"], errLeftUnsaid)
	}
}

// A process that stops does not log that its last write was cut off: it
// was not a failure of the inbox.
func TestAWriteCutOffByTheStopIsNotAnOutage(t *testing.T) {
	into := &fakeInbox{acceptErr: context.Canceled}
	buffer, log := newBuffer(into)
	record(buffer, 0, 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	buffer.flush(ctx)
	if failing := log.lines(t, messageFailing); len(failing) != 0 || waiting(buffer) != 3 {
		t.Errorf("the log says %v and %d events wait, want nothing said and 3 waiting", failing, waiting(buffer))
	}
}
