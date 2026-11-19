package rollup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// docs/delivery-semantics.md is the only source of the parameters. Whoever
// changes one changes it there, and here.
func TestTheParametersOfAConsumerAreTheOnesTheDocumentStates(t *testing.T) {
	if BatchSize != 500 || PollEvery != time.Second || idleTimeout != 30*time.Second {
		t.Errorf("a consumer takes %d events at once, asks every %s and may be idle in a transaction for %s", BatchSize, PollEvery, idleTimeout)
	}
	document, err := os.ReadFile("../../docs/delivery-semantics.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{
		"| Consumer batch | 500 events |", "| Consumer poll | Every second, and at once after a whole batch |",
		"| Idle transaction of a consumer | Ended after 30 seconds |",
	} {
		if !strings.Contains(string(document), row) {
			t.Errorf("docs/delivery-semantics.md does not have the row %q", row)
		}
	}
}

// inbox is what a consumer finds each time it asks, as a test says, and
// when it asked.
type inbox struct {
	mu sync.Mutex
	// finds is what the next batches find, in order; after them, nothing.
	finds []find
	asked []time.Duration
	since time.Time
}

type find struct {
	processed int
	err       error
}

func (i *inbox) batch(context.Context) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asked = append(i.asked, time.Since(i.since))
	if len(i.finds) == 0 {
		return 0, nil
	}
	next := i.finds[0]
	i.finds = i.finds[1:]
	return next.processed, next.err
}

// moments returns when the consumer asked, counted from its start.
func (i *inbox) moments() []time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.asked)
}

// consuming runs a consumer over what a test says it finds, and returns
// its log and what stops it. It is for tests inside a bubble.
func consuming(t *testing.T, finds ...find) (*inbox, *bytes.Buffer, func()) {
	t.Helper()
	found := &inbox{finds: finds, since: time.Now()}
	log := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, found.batch, slog.New(slog.NewJSONHandler(log, nil)))
	}()
	return found, log, func() {
		cancel()
		<-done
	}
}

const (
	messageFailing = "a batch of the inbox could not be processed, and its events stay pending"
	messageBack    = "the inbox is being processed again"
)

// A consumer asks at its start and then every second; after a whole batch
// it asks again at once, because more may wait.
func TestAConsumerAsksEverySecondAndAtOnceAfterAWholeBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		found, _, stop := consuming(t,
			find{processed: 0}, find{processed: 3},
			// Two whole batches and what is left.
			find{processed: BatchSize}, find{processed: BatchSize}, find{processed: 20},
		)
		time.Sleep(3*PollEvery + PollEvery/2)
		synctest.Wait()
		stop()
		want := []time.Duration{0, PollEvery, 2 * PollEvery, 2 * PollEvery, 2 * PollEvery, 3 * PollEvery}
		if got := found.moments(); !slices.Equal(got, want) {
			t.Errorf("the consumer asked at %v, want %v", got, want)
		}
	})
}

// A batch that fails is tried again with the next interval and not at
// once, and the log says that it fails once, and once that it is over.
func TestAConsumerThatFailsAsksAgainWithTheNextInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		away := errors.New("the database is away")
		found, log, stop := consuming(t, find{err: away}, find{err: away}, find{err: away}, find{processed: BatchSize}, find{processed: 1})
		time.Sleep(4*PollEvery + PollEvery/2)
		synctest.Wait()
		stop()
		want := []time.Duration{0, PollEvery, 2 * PollEvery, 3 * PollEvery, 3 * PollEvery, 4 * PollEvery}
		if got := found.moments(); !slices.Equal(got, want) {
			t.Errorf("the consumer asked at %v, want %v", got, want)
		}
		if failing, back := strings.Count(log.String(), messageFailing), strings.Count(log.String(), messageBack); failing != 1 || back != 1 {
			t.Errorf("the log says %d times that a batch fails and %d that it is over, want once each: %s", failing, back, log.String())
		}
	})
}

// A consumer stops with its context, and a batch cut off by the stop is
// not a failure of the inbox.
func TestAConsumerStopsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &bytes.Buffer{}
		ctx, cancel := context.WithCancel(t.Context())
		asked := 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			run(ctx, func(ctx context.Context) (int, error) {
				asked++
				// A batch that lasts until the process is told to stop.
				<-ctx.Done()
				return 0, ctx.Err()
			}, slog.New(slog.NewJSONHandler(log, nil)))
		}()
		time.Sleep(PollEvery / 2)
		cancel()
		<-done
		if asked != 1 || strings.Contains(log.String(), messageFailing) {
			t.Errorf("the consumer asked %d times and logged %q; want once, and nothing", asked, log.String())
		}
	})
}
