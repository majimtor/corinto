//go:build integration

package rollup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
)

// lockedLog is a log that a test reads while consumers write it.
type lockedLog struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// lines returns the entries of the log whose message is one.
func (l *lockedLog) lines(t *testing.T, message string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.Lines(l.String()) {
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
	messageRetried      = "an event could not be processed and will be tried again"
	messageDeadLettered = "an event failed on every attempt and is among the dead letters"
)

// A defect that makes the events of one class impossible to add: the time
// series refuses previews. Nothing in the real schema refuses an event the
// inbox took, so the tests of what happens when something does have to
// break it on purpose.
func (s *stage) refusePreviews(t *testing.T) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE link_hourly ADD CONSTRAINT refuse_previews CHECK (class <> 'preview')`); err != nil {
		t.Fatal(err)
	}
}

// acceptPreviews is the defect corrected.
func (s *stage) acceptPreviews(t *testing.T) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE link_hourly DROP CONSTRAINT refuse_previews`); err != nil {
		t.Fatal(err)
	}
}

// preview returns an event that the broken schema refuses, with something
// in every field that an event can have.
func (s *stage) preview(at time.Time) ingest.Event {
	return with(click(s.spring, s.acme, at), func(e *ingest.Event) {
		e.Method, e.ReferrerHost = "HEAD", "sentinel.example.org"
		e.Classification = classify.Classification{
			Class: classify.ClassPreview, Rule: "preview:WhatsApp", Version: 3,
			Device: classify.DeviceMobile, Browser: classify.BrowserSafari,
		}
	})
}

// state is what the inbox says of the attempts of an event.
type state struct {
	// pending is whether the event is in the inbox and not processed.
	pending  bool
	attempts int
	// wait is how long it is until the next attempt; zero when it is due.
	wait time.Duration
}

func (s *stage) state(t *testing.T, id string) state {
	t.Helper()
	var (
		got     state
		seconds float64
	)
	err := s.pool.QueryRow(t.Context(),
		`SELECT processed_at IS NULL, attempts, coalesce(extract(epoch FROM next_attempt_at - now()), 0)::float8
		   FROM event_inbox WHERE id = $1::uuid`, id).Scan(&got.pending, &got.attempts, &seconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return state{}
	}
	if err != nil {
		t.Fatal(err)
	}
	got.wait = time.Duration(seconds * float64(time.Second))
	return got
}

// due makes the wait of an event be over, as time would.
func (s *stage) due(t *testing.T, id string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
}

// batch asks for a batch, which must not fail, and returns how many
// events it processed.
func (s *stage) batch(t *testing.T) int {
	t.Helper()
	processed, err := s.consumer.Batch(t.Context())
	if err != nil {
		t.Fatalf("Batch failed: %v", err)
	}
	return processed
}

// counted returns a counter of the pipeline, over every day and workspace.
func (s *stage) counted(t *testing.T, name string) int64 {
	t.Helper()
	return s.number(t, `SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = $1`, name)
}

// Statement 5: when a batch fails its events are tried one by one, so one
// bad event does not hold the others back and only its own failures count
// as attempts.
func TestOneBadEventDoesNotHoldItsBatch(t *testing.T) {
	s := newStage(t)
	s.refusePreviews(t)
	events := make([]ingest.Event, 0, 10)
	for i := range 9 {
		events = append(events, with(click(s.spring, s.acme, at(10, i, 0)), func(e *ingest.Event) { e.ReferrerHost = "news.example.com" }))
	}
	bad := s.preview(at(10, 30, 0))
	// In the middle of the batch.
	events = append(events[:4:4], append([]ingest.Event{bad}, events[4:]...)...)
	s.accept(t, events...)

	if processed := s.batch(t); processed != 9 {
		t.Fatalf("Batch processed %d events, want the 9 that the database takes", processed)
	}
	if total, counted := s.number(t, `SELECT coalesce(sum(events), 0) FROM link_hourly`), s.counted(t, "processed"); total != 9 || counted != 9 {
		t.Errorf("the time series adds up to %d events and %d are counted as processed, want 9 of each", total, counted)
	}
	// The bad one is pending with one attempt less and a second to wait.
	if got := s.state(t, bad.ID); !got.pending || got.attempts != 1 || got.wait <= 0 || got.wait > time.Second {
		t.Errorf("the event that was refused is %+v, want pending with one failed attempt and up to a second to wait", got)
	}
	// The failure of the batch is of nobody: the others have no attempt.
	if others := s.number(t, `SELECT count(*) FROM event_inbox WHERE id <> $1::uuid AND (attempts <> 0 OR processed_at IS NULL)`, bad.ID); others != 0 {
		t.Errorf("%d of the other events have a failed attempt or are pending", others)
	}
	// The log names the event and the answer of the database, and nothing
	// of what the event says.
	retried := s.log.lines(t, messageRetried)
	if len(retried) != 1 || retried[0]["event"] != bad.ID || retried[0]["sqlstate"] != "23514" || retried[0]["constraint"] != "refuse_previews" || retried[0]["level"] != "WARN" {
		t.Errorf("the log says %v, want one failed attempt of %s with 23514 and refuse_previews", retried, bad.ID)
	}
	if said := s.log.String(); strings.Contains(said, "sentinel") || strings.Contains(said, s.spring) || strings.Contains(said, "WhatsApp") {
		t.Errorf("the log quotes the event: %s", said)
	}

	// It is not tried again before its time: not in a batch, and not on
	// its own by a consumer that had it in a batch that failed.
	if processed := s.batch(t); processed != 0 {
		t.Errorf("a batch right after processed %d events", processed)
	}
	if processed, err := s.consumer.alone(t.Context(), bad); err != nil || processed {
		t.Errorf("an event that waits was processed on its own (%v)", err)
	}
	if got := s.state(t, bad.ID); got.attempts != 1 || len(s.log.lines(t, messageRetried)) != 1 {
		t.Errorf("an event that waits was tried again: %+v", got)
	}
}

// Statement 5: only a refusal is a failure of an event. While the events
// of a refused batch are tried one by one, one of them finds a row that
// another consumer holds, and the database cancels its statement when it
// has waited for a second: it has no attempt less, and the batch says
// that it could not finish.
func TestAnEventThatRanOutOfTimeHasNoAttemptLess(t *testing.T) {
	s := newStage(t)
	s.refusePreviews(t)
	// A consumer whose statements the database gives a second.
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["statement_timeout"] = "1000"
	hurried, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hurried.Close)
	consumer := NewConsumer(hurried, slog.New(slog.NewJSONHandler(s.log, nil)))
	// The link of Acme was created first, and its rows are written first:
	// the batch is refused for its preview before it reaches the row of
	// Globex that is held.
	fine, bad, slow := click(s.spring, s.acme, at(10, 0, 0)), s.preview(at(10, 0, 0)), click(s.autumn, s.globex, at(10, 0, 0))
	held := click(s.autumn, s.globex, at(10, 5, 0))
	s.accept(t, held)
	hung, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hung.Rollback(context.WithoutCancel(t.Context())) }()
	taken, err := ingest.Take(t.Context(), hung, 1)
	if err != nil || len(taken) != 1 {
		t.Fatalf("Take returned %d events with the error %v", len(taken), err)
	}
	if err := add(t.Context(), hung, taken); err != nil {
		t.Fatal(err)
	}
	s.accept(t, fine, bad, slow)

	processed, err := consumer.Batch(t.Context())
	var answer *pgconn.PgError
	if !errors.As(err, &answer) || answer.Code != "57014" || processed != 1 {
		t.Fatalf("Batch processed %d events with the error %v; want the one that is fine, and the statement of another cancelled", processed, err)
	}
	if got := s.state(t, bad.ID); !got.pending || got.attempts != 1 {
		t.Errorf("the event that was refused is %+v, want pending with one failed attempt", got)
	}
	if got := s.state(t, slow.ID); !got.pending || got.attempts != 0 || got.wait != 0 {
		t.Errorf("the event that ran out of time is %+v, want pending, with no failed attempt and nothing to wait", got)
	}

	// The row is free again, and the event is processed like any other.
	if err := hung.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if processed := s.batch(t); processed != 2 {
		t.Errorf("Batch processed %d events, want the one that was held and the one that waited for its row", processed)
	}
	if got := s.state(t, slow.ID); got.pending {
		t.Errorf("the event that ran out of time is still pending: %+v", got)
	}
}

// Statement 5: an event has five attempts, waits 1 second, 5 seconds, 30
// seconds and 2 minutes between them, and after the fifth failure moves to
// the dead-letter table with the reason of the last one.
func TestAnEventThatExhaustsItsAttemptsIsDeadLettered(t *testing.T) {
	s := newStage(t)
	s.refusePreviews(t)
	bad := s.preview(at(23, 59, 59))
	s.accept(t, click(s.spring, s.acme, at(10, 0, 0)), bad, click(s.autumn, s.globex, at(10, 0, 0)))
	var accepted time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, bad.ID).Scan(&accepted); err != nil {
		t.Fatal(err)
	}

	waits := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	for attempt, wait := range waits {
		want := 0
		if attempt == 0 {
			// The first batch has the two events that are fine.
			want = 2
		}
		if processed := s.batch(t); processed != want {
			t.Fatalf("attempt %d: Batch processed %d events, want %d", attempt+1, processed, want)
		}
		// By the clock of the database: the wait, less what the test took
		// to ask.
		if got := s.state(t, bad.ID); !got.pending || got.attempts != attempt+1 || got.wait > wait || got.wait < wait-2*time.Second {
			t.Fatalf("after %d failed attempts the event is %+v, want pending and %s to wait", attempt+1, got, wait)
		}
		// Not before its time.
		if processed := s.batch(t); processed != 0 || s.state(t, bad.ID).attempts != attempt+1 {
			t.Fatalf("attempt %d: the event was tried again before its wait was over", attempt+1)
		}
		s.due(t, bad.ID)
	}

	// The fifth failure.
	if processed := s.batch(t); processed != 0 {
		t.Fatalf("Batch processed %d events", processed)
	}
	if got := s.state(t, bad.ID); got.pending || s.pending(t) != 0 {
		t.Fatalf("after its fifth failure the event is still in the inbox: %+v", got)
	}
	var (
		dead            ingest.Event
		device, browser string
		class, rule     string
		reason          string
		constraint      *string
		acceptedThen    time.Time
		since           float64
	)
	err := s.pool.QueryRow(t.Context(),
		`SELECT link_id::text, workspace_id::text, occurred_at, outcome, method, referrer_host, device, browser, class, rule,
		        rules_version, accepted_at, reason_sqlstate, reason_constraint, extract(epoch FROM now() - dead_lettered_at)::float8
		   FROM event_dead_letters WHERE id = $1::uuid`, bad.ID).
		Scan(&dead.LinkID, &dead.WorkspaceID, &dead.OccurredAt, &dead.Outcome, &dead.Method, &dead.ReferrerHost, &device, &browser,
			&class, &rule, &dead.Classification.Version, &acceptedThen, &reason, &constraint, &since)
	if err != nil {
		t.Fatalf("the event is not among the dead letters: %v", err)
	}
	// The event as it was accepted, whole.
	if dead.LinkID != bad.LinkID || dead.WorkspaceID != bad.WorkspaceID || !dead.OccurredAt.Equal(bad.OccurredAt) || dead.Outcome != bad.Outcome ||
		dead.Method != "HEAD" || dead.ReferrerHost != "sentinel.example.org" || device != "mobile" || browser != "safari" ||
		class != "preview" || rule != "preview:WhatsApp" || dead.Classification.Version != 3 || !acceptedThen.Equal(accepted) {
		t.Errorf("the dead letter is %+v, %s, %s, %s, %s, accepted at %s; want the event as it was accepted at %s", dead, device, browser, class, rule, acceptedThen, accepted)
	}
	// With the reason of the last attempt, and now.
	if reason != "23514" || constraint == nil || *constraint != "refuse_previews" || since < 0 || since > 5 {
		t.Errorf("the dead letter has the reason %s and %v, from %.1f seconds ago; want 23514 and refuse_previews, from now", reason, constraint, since)
	}
	// Counted on the day of its access and for its workspace, and the
	// second equality has its three terms: accepted = processed + pending
	// + dead-lettered.
	compare(t, "the dead-lettered events", s.table(t, strings.Replace(processedRows, "'processed'", "'dead_lettered'", 1)), map[string]int64{"Acme " + today: 1})
	if accepted, processed, dead := s.counted(t, "accepted"), s.counted(t, "processed"), s.counted(t, "dead_lettered"); accepted != 3 || processed != 2 || dead != 1 {
		t.Errorf("%d accepted, %d processed and %d dead-lettered; want 3, 2 and 1", accepted, processed, dead)
	}
	if total := s.number(t, `SELECT coalesce(sum(events), 0) FROM link_hourly`); total != 2 {
		t.Errorf("the time series adds up to %d events, want the 2 that were processed", total)
	}
	// Four attempts that will be tried again, and one that will not.
	retried, last := s.log.lines(t, messageRetried), s.log.lines(t, messageDeadLettered)
	if len(retried) != 4 || len(last) != 1 || last[0]["event"] != bad.ID || last[0]["sqlstate"] != "23514" || last[0]["level"] != "ERROR" {
		t.Errorf("the log has %d failed attempts and says %v of the last", len(retried), last)
	}
}

// deadLetter makes events fail on every attempt.
func (s *stage) deadLetter(t *testing.T, events ...ingest.Event) {
	t.Helper()
	for range ingest.MaxAttempts {
		s.batch(t)
		for _, event := range events {
			s.due(t, event.ID)
		}
	}
	if dead := s.number(t, `SELECT count(*) FROM event_dead_letters`); dead != int64(len(events)) {
		t.Fatalf("%d events are among the dead letters, want %d", dead, len(events))
	}
}

// Statement 5: the operator can send a dead-lettered event back to the
// inbox, which gives it five attempts again.
func TestADeadLetteredEventCanBeReprocessed(t *testing.T) {
	s := newStage(t)
	s.refusePreviews(t)
	one, another, third := s.preview(at(10, 0, 0)), s.preview(at(11, 0, 0)), s.preview(nextDay.Add(time.Hour))
	s.accept(t, one, another, third)
	var accepted time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, one.ID).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	s.deadLetter(t, one, another, third)

	// Sent back with the defect still there: it has its attempts again,
	// and fails the first of them.
	if err := s.inbox.RetryDeadLetter(t.Context(), one.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, one.ID); !got.pending || got.attempts != 0 || got.wait != 0 {
		t.Fatalf("the event sent back is %+v, want pending, with no failed attempt and nothing to wait", got)
	}
	var acceptedThen time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, one.ID).Scan(&acceptedThen); err != nil || !acceptedThen.Equal(accepted) {
		t.Errorf("the event sent back was accepted at %s (%v), want when it was, %s", acceptedThen, err, accepted)
	}
	if dead, counted, again := s.number(t, `SELECT count(*) FROM event_dead_letters`), s.counted(t, "dead_lettered"), s.counted(t, "accepted"); dead != 2 || counted != 2 || again != 3 {
		t.Errorf("%d dead letters, %d counted as such and %d as accepted; want 2, 2 and the 3 accepted once", dead, counted, again)
	}
	if processed := s.batch(t); processed != 0 {
		t.Fatalf("Batch processed %d events with the defect still there", processed)
	}
	if got := s.state(t, one.ID); !got.pending || got.attempts != 1 {
		t.Fatalf("after failing again the event is %+v, want pending with one failed attempt of five", got)
	}

	// What is not a dead letter cannot be sent back.
	for name, id := range map[string]string{
		"an event that is pending": one.ID, "an identifier of nothing": "0193a000-0000-7000-8000-0000000000ff",
		"something that is not an identifier": "not-an-identifier", "nothing": "",
	} {
		if err := s.inbox.RetryDeadLetter(t.Context(), id); !errors.Is(err, ingest.ErrNoDeadLetter) {
			t.Errorf("sending back %s ended with %v, want ErrNoDeadLetter", name, err)
		}
	}
	if dead := s.number(t, `SELECT count(*) FROM event_dead_letters`); dead != 2 {
		t.Fatalf("%d events are among the dead letters, want the 2 nobody sent back", dead)
	}

	// The defect is corrected, and the event is processed when it is due.
	s.acceptPreviews(t)
	s.due(t, one.ID)
	if processed := s.batch(t); processed != 1 {
		t.Fatalf("Batch processed %d events, want the one that was sent back", processed)
	}
	// The rest go back together, and are processed.
	sent, err := s.inbox.RetryDeadLetters(t.Context())
	if err != nil || sent != 2 {
		t.Fatalf("RetryDeadLetters sent %d events back with the error %v, want 2", sent, err)
	}
	if processed := s.batch(t); processed != 2 {
		t.Fatalf("Batch processed %d events, want the two that were sent back", processed)
	}
	if again, err := s.inbox.RetryDeadLetters(t.Context()); err != nil || again != 0 {
		t.Errorf("with no dead letters, RetryDeadLetters sent %d events back with the error %v", again, err)
	}
	s.agrees(t, 3)
	// On each day, nothing is counted as dead-lettered any longer.
	if dead, rows := s.counted(t, "dead_lettered"), s.number(t, `SELECT count(*) FROM pipeline_counters WHERE name = 'dead_lettered' AND value <> 0`); dead != 0 || rows != 0 {
		t.Errorf("%d events are counted as dead-lettered, in %d rows", dead, rows)
	}
}

// hold is a tracer that stops a connection before a statement, as a
// process that hangs between two statements of a transaction.
type hold struct {
	// at is what the statement to stop before says.
	at string
	// reached is closed when a connection is stopped, and release lets it
	// go on.
	reached, release chan struct{}
	once             sync.Once
}

func (h *hold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, h.at) {
		h.once.Do(func() {
			close(h.reached)
			<-h.release
		})
	}
	return ctx
}

func (*hold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A consumer that hangs between two statements holds the rows it wrote.
// The database ends its transaction when it has been idle for too long,
// the others go on, and its events are pending for the next attempt.
func TestTheDatabaseEndsTheTransactionOfAConsumerThatHangs(t *testing.T) {
	s := newStage(t)
	// In the process, thirty seconds.
	if s.consumer.idle != 30*time.Second {
		t.Errorf("a consumer may be idle in a transaction for %s", s.consumer.idle)
	}
	tx, err := s.consumer.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var idle string
	if err := tx.QueryRow(t.Context(), `SHOW idle_in_transaction_session_timeout`).Scan(&idle); err != nil || idle != "30s" {
		t.Errorf("the transaction of a consumer has an idle time of %q (%v), want 30s", idle, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A consumer that hangs before it marks its events, with two seconds
	// of idle time instead of thirty.
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	stopped := &hold{at: "UPDATE event_inbox SET processed_at", reached: make(chan struct{}), release: make(chan struct{})}
	config.ConnConfig.Tracer = stopped
	own, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)
	// Whatever happens, the connection is let go before its pool is
	// closed, which waits for it.
	release := sync.OnceFunc(func() { close(stopped.release) })
	t.Cleanup(release)
	hung := NewConsumer(own, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	hung.idle = 2 * time.Second

	s.accept(t, click(s.spring, s.acme, at(10, 0, 0)), click(s.spring, s.acme, at(10, 1, 0)))
	ended := make(chan error, 1)
	go func() {
		_, err := hung.Batch(context.WithoutCancel(t.Context()))
		ended <- err
	}()
	<-stopped.reached

	// Another consumer has an event for the row the first one wrote.
	s.accept(t, click(s.spring, s.acme, at(10, 2, 0)))
	started := time.Now()
	processed, err := s.consumer.Batch(t.Context())
	// It waited for the row, and for no longer than the idle time.
	if elapsed := time.Since(started); err != nil || processed != 1 || elapsed < time.Second || elapsed > batchTimeout/2 {
		t.Fatalf("Batch processed %d events with the error %v after %s; want its event, once the database ended the transaction that held the row", processed, err, elapsed)
	}

	// The consumer that hung finds its transaction gone, and has processed
	// nothing: its two events are pending, and are counted once.
	release()
	if err := <-ended; err == nil {
		t.Error("the batch of the consumer that hung ended without an error")
	}
	if pending := s.pending(t); pending != 2 {
		t.Fatalf("%d events are pending, want the two of the consumer that hung", pending)
	}
	if retried := s.number(t, `SELECT count(*) FROM event_inbox WHERE attempts <> 0`); retried != 0 {
		t.Errorf("%d events have a failed attempt: a transaction that was ended is not a failure of its events", retried)
	}
	s.drain(t, 1)
	s.agrees(t, 3)
}
