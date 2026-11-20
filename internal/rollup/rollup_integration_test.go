//go:build integration

package rollup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// stage is a consumer over a real database, with two workspaces: Acme
// with two links, Globex with one.
type stage struct {
	pool     *pgxpool.Pool
	inbox    *ingest.Store
	consumer *Consumer
	// log is what the consumer logged.
	log *lockedLog
	// The workspaces and their links.
	acme, globex           string
	spring, summer, autumn string
}

func newStage(t *testing.T) *stage {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	s := &stage{pool: pool, log: &lockedLog{}, inbox: ingest.NewStore(pool, slog.New(slog.NewJSONHandler(io.Discard, nil)))}
	s.consumer = NewConsumer(pool, slog.New(slog.NewJSONHandler(s.log, nil)))
	links := link.NewStore(pool, "go.example.dev", rand.Reader)
	for _, owned := range []struct {
		workspace *string
		name      string
		links     []*string
	}{{&s.acme, "Acme", []*string{&s.spring, &s.summer}}, {&s.globex, "Globex", []*string{&s.autumn}}} {
		workspace, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: owned.name, MaxLinks: 1000, MaxKeys: 10})
		if err != nil {
			t.Fatal(err)
		}
		*owned.workspace = workspace.ID
		for _, id := range owned.links {
			created, err := links.Create(t.Context(), workspace.ID, link.Draft{Destination: "https://example.com/"})
			if err != nil {
				t.Fatal(err)
			}
			*id = created.ID
		}
	}
	return s
}

// west makes the process of a test run eight hours behind UTC: the hours
// and the days of the aggregates are the ones of UTC wherever it runs.
func west(t *testing.T) {
	t.Helper()
	local := time.Local
	time.Local = time.FixedZone("UTC-8", -8*60*60)
	t.Cleanup(func() { time.Local = local })
}

// The day these tests happen on, and the next.
var (
	day      = time.Date(2026, time.November, 19, 0, 0, 0, 0, time.UTC)
	nextDay  = day.Add(24 * time.Hour)
	today    = "2026-11-19"
	tomorrow = "2026-11-20"
)

// at returns a moment of the day of these tests.
func at(hour, minute, second int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute + time.Duration(second)*time.Second)
}

// click returns the event of an estimated human click on a link: a
// redirect to somebody on a desktop Chrome that named no referrer.
func click(linkID, workspaceID string, at time.Time) ingest.Event {
	return ingest.Event{
		ID: uuid.NewV7().String(), LinkID: linkID, WorkspaceID: workspaceID, OccurredAt: at,
		Outcome: ingest.OutcomeRedirected, Method: "GET",
		Classification: classify.Classification{
			Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
			Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
		},
	}
}

// with returns an event changed.
func with(event ingest.Event, change func(*ingest.Event)) ingest.Event {
	change(&event)
	return event
}

// accept writes events to the inbox, as the redirect does.
func (s *stage) accept(t *testing.T, events ...ingest.Event) {
	t.Helper()
	for len(events) > 0 {
		batch := events[:min(len(events), BatchSize)]
		if settled, err := s.inbox.Accept(t.Context(), batch); err != nil || settled != len(batch) {
			t.Fatalf("Accept settled %d events with the error %v", settled, err)
		}
		events = events[len(batch):]
	}
}

func (s *stage) number(t *testing.T, query string, arguments ...any) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(t.Context(), query, arguments...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (s *stage) pending(t *testing.T) int64 {
	t.Helper()
	return s.number(t, `SELECT count(*) FROM event_inbox WHERE processed_at IS NULL`)
}

// table reads the rows of a query as a map from the text of a key to a
// number.
func (s *stage) table(t *testing.T, query string) map[string]int64 {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	read := make(map[string]int64)
	var (
		key   string
		value int64
	)
	if _, err := pgx.ForEachRow(rows, []any{&key, &value}, func() error {
		read[key] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return read
}

const (
	// The time series, with the name of the workspace and the place of the
	// link among the ones of its workspace instead of their identifiers.
	seriesRows = `SELECT concat_ws(' ', w.name, l.identifier, to_char(h.hour AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI'), h.outcome, h.class), h.events
	                FROM link_hourly AS h JOIN links AS l ON l.id = h.link_id JOIN workspaces AS w ON w.id = h.workspace_id`
	breakdownRows = `SELECT concat_ws(' ', w.name, l.identifier, d.day, d.dimension, d.value), d.events
	                   FROM link_daily_dimension AS d JOIN links AS l ON l.id = d.link_id JOIN workspaces AS w ON w.id = d.workspace_id`
	processedRows = `SELECT concat_ws(' ', w.name, c.day), c.value
	                   FROM pipeline_counters AS c JOIN workspaces AS w ON w.id = c.workspace_id WHERE c.name = 'processed'`
)

// identifier returns the identifier of a link, which is how the tests
// name it in what they read.
func (s *stage) identifier(t *testing.T, linkID string) string {
	t.Helper()
	var identifier string
	if err := s.pool.QueryRow(t.Context(), `SELECT identifier FROM links WHERE id = $1::uuid`, linkID).Scan(&identifier); err != nil {
		t.Fatal(err)
	}
	return identifier
}

// compare fails when what was read is not what was expected, row by row.
func compare(t *testing.T, what string, got, want map[string]int64) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: %q is at %d, want %d", what, key, got[key], value)
		}
	}
	for key, value := range got {
		if _, expected := want[key]; !expected {
			t.Errorf("%s: %q is at %d and should not be there", what, key, value)
		}
	}
}

// drain runs consumers at once until nothing is pending, and fails when
// one of them does.
func (s *stage) drain(t *testing.T, consumers int) {
	t.Helper()
	var wait sync.WaitGroup
	for range consumers {
		wait.Go(func() {
			// More rounds than any of these tests has batches.
			for range 1000 {
				processed, err := s.consumer.Batch(t.Context())
				if err != nil {
					t.Errorf("a batch failed: %v", err)
					return
				}
				// None can mean that others hold what is pending.
				if processed == 0 && s.pending(t) == 0 {
					return
				}
			}
			t.Error("a consumer never saw the inbox empty")
		})
	}
	wait.Wait()
}

func TestABatchIsAddedToTheAggregatesAndMarkedProcessed(t *testing.T) {
	west(t)
	s := newStage(t)
	spring, autumn := s.identifier(t, s.spring), s.identifier(t, s.autumn)
	from := func(host string) func(*ingest.Event) {
		return func(e *ingest.Event) { e.ReferrerHost = host }
	}
	on := func(device classify.Device, browser classify.Browser, host string) func(*ingest.Event) {
		return func(e *ingest.Event) {
			e.Classification.Device, e.Classification.Browser, e.ReferrerHost = device, browser, host
		}
	}
	as := func(class classify.Class) func(*ingest.Event) {
		return func(e *ingest.Event) {
			e.Classification.Class, e.Classification.Rule = class, string(class)+":test"
		}
	}
	s.accept(t,
		with(click(s.spring, s.acme, at(10, 5, 0)), from("news.example.com")),
		// The last second of an hour, and the first of the next.
		with(click(s.spring, s.acme, at(10, 59, 59)), on(classify.DeviceMobile, classify.BrowserSafari, "")),
		with(click(s.spring, s.acme, at(11, 0, 0)), from("news.example.com")),
		// What is not an estimated human click is in the series and in no
		// breakdown.
		with(click(s.spring, s.acme, at(10, 30, 0)), as(classify.ClassCrawler)),
		with(click(s.spring, s.acme, at(10, 31, 0)), func(e *ingest.Event) {
			e.Outcome, e.ReferrerHost = ingest.OutcomeInactive, "news.example.com"
		}),
		// The last second of a day, and the first of the next.
		with(click(s.spring, s.acme, at(23, 59, 59)), on(classify.DeviceTablet, classify.BrowserFirefox, "blog.example.org")),
		with(click(s.spring, s.acme, at(24, 0, 0)), on(classify.DeviceTablet, classify.BrowserFirefox, "blog.example.org")),
		// Another workspace.
		with(click(s.autumn, s.globex, at(10, 5, 0)), as(classify.ClassAutomation)),
		with(click(s.autumn, s.globex, at(10, 6, 0)), on(classify.DeviceDesktop, classify.BrowserEdge, "")),
	)

	if processed, err := s.consumer.Batch(t.Context()); err != nil || processed != 9 {
		t.Fatalf("Batch processed %d events with the error %v, want 9", processed, err)
	}
	compare(t, "the time series", s.table(t, seriesRows), map[string]int64{
		"Acme " + spring + " 2026-11-19 10:00 redirected presumed_human":   2,
		"Acme " + spring + " 2026-11-19 10:00 redirected crawler":          1,
		"Acme " + spring + " 2026-11-19 10:00 inactive presumed_human":     1,
		"Acme " + spring + " 2026-11-19 11:00 redirected presumed_human":   1,
		"Acme " + spring + " 2026-11-19 23:00 redirected presumed_human":   1,
		"Acme " + spring + " 2026-11-20 00:00 redirected presumed_human":   1,
		"Globex " + autumn + " 2026-11-19 10:00 redirected automation":     1,
		"Globex " + autumn + " 2026-11-19 10:00 redirected presumed_human": 1,
	})
	compare(t, "the breakdowns", s.table(t, breakdownRows), map[string]int64{
		"Acme " + spring + " 2026-11-19 referrer_host news.example.com": 2,
		"Acme " + spring + " 2026-11-19 referrer_host (none)":           1,
		"Acme " + spring + " 2026-11-19 referrer_host blog.example.org": 1,
		"Acme " + spring + " 2026-11-19 device desktop":                 2,
		"Acme " + spring + " 2026-11-19 device mobile":                  1,
		"Acme " + spring + " 2026-11-19 device tablet":                  1,
		"Acme " + spring + " 2026-11-19 browser chrome":                 2,
		"Acme " + spring + " 2026-11-19 browser safari":                 1,
		"Acme " + spring + " 2026-11-19 browser firefox":                1,
		"Acme " + spring + " 2026-11-20 referrer_host blog.example.org": 1,
		"Acme " + spring + " 2026-11-20 device tablet":                  1,
		"Acme " + spring + " 2026-11-20 browser firefox":                1,
		"Globex " + autumn + " 2026-11-19 referrer_host (none)":         1,
		"Globex " + autumn + " 2026-11-19 device desktop":               1,
		"Globex " + autumn + " 2026-11-19 browser edge":                 1,
	})
	// Processed, and counted on the day of the access.
	compare(t, "the events processed", s.table(t, processedRows), map[string]int64{
		"Acme " + today: 6, "Acme " + tomorrow: 1, "Globex " + today: 2,
	})
	if pending := s.pending(t); pending != 0 {
		t.Errorf("%d events are still pending", pending)
	}
	if late := s.number(t, `SELECT count(*) FROM event_inbox WHERE processed_at < accepted_at OR processed_at > now()`); late != 0 {
		t.Errorf("%d events were processed at a moment that is not now", late)
	}

	// With nothing pending there is nothing to do, and nothing changes.
	if processed, err := s.consumer.Batch(t.Context()); err != nil || processed != 0 {
		t.Errorf("a second Batch processed %d events with the error %v, want none", processed, err)
	}
	if total := s.number(t, `SELECT sum(events) FROM link_hourly`); total != 9 {
		t.Errorf("the time series adds up to %d events, want 9", total)
	}
}

// The enumerations are in the code and, repeated, in the tables. The
// aggregates take every value the code can write.
func TestTheAggregatesTakeEveryValueTheCodeWrites(t *testing.T) {
	s := newStage(t)
	var events []ingest.Event
	for _, class := range classify.Classes() {
		for _, outcome := range []ingest.Outcome{ingest.OutcomeRedirected, ingest.OutcomeInactive} {
			events = append(events, with(click(s.spring, s.acme, at(10, 0, 0)), func(e *ingest.Event) {
				e.Classification.Class, e.Outcome = class, outcome
			}))
		}
	}
	for _, device := range classify.Devices() {
		events = append(events, with(click(s.summer, s.acme, at(10, 0, 0)), func(e *ingest.Event) { e.Classification.Device = device }))
	}
	for _, browser := range classify.Browsers() {
		events = append(events, with(click(s.autumn, s.globex, at(10, 0, 0)), func(e *ingest.Event) { e.Classification.Browser = browser }))
	}
	s.accept(t, events...)
	if processed, err := s.consumer.Batch(t.Context()); err != nil || processed != len(events) {
		t.Fatalf("Batch processed %d of %d events with the error %v", processed, len(events), err)
	}
	if series, want := s.number(t, `SELECT count(*) FROM link_hourly WHERE link_id = $1::uuid`, s.spring), int64(2*len(classify.Classes())); series != want {
		t.Errorf("the time series of the link has %d rows, want one per class and outcome, %d", series, want)
	}
	if devices := s.number(t, `SELECT count(*) FROM link_daily_dimension WHERE link_id = $1::uuid AND dimension = 'device'`, s.summer); devices != int64(len(classify.Devices())) {
		t.Errorf("the breakdown by device has %d rows, want %d", devices, len(classify.Devices()))
	}
	if browsers := s.number(t, `SELECT count(*) FROM link_daily_dimension WHERE link_id = $1::uuid AND dimension = 'browser'`, s.autumn); browsers != int64(len(classify.Browsers())) {
		t.Errorf("the breakdown by browser has %d rows, want %d", browsers, len(classify.Browsers()))
	}
}

// varied returns events that differ in everything an aggregate is keyed
// by, in an order that says nothing of their keys.
func (s *stage) varied(n int) []ingest.Event {
	links := []struct{ link, workspace string }{{s.spring, s.acme}, {s.summer, s.acme}, {s.autumn, s.globex}}
	classes, devices, browsers := classify.Classes(), classify.Devices(), classify.Browsers()
	hosts := []string{"", "news.example.com", "blog.example.org", "search.example.net", "", "mail.example.com", "forum.example.org"}
	events := make([]ingest.Event, n)
	for i := range events {
		// A multiplication by a prime scatters the keys over the events.
		scattered := i * 7919
		of := links[scattered%len(links)]
		events[i] = with(click(of.link, of.workspace, day.Add(time.Duration(scattered%(30*60))*time.Minute)), func(e *ingest.Event) {
			// Most are human clicks, so that the breakdowns have rows that
			// several consumers write.
			if i%3 == 0 {
				e.Classification.Class = classes[scattered%len(classes)]
			}
			if i%11 == 0 {
				e.Outcome = ingest.OutcomeInactive
			}
			e.Classification.Device = devices[(scattered/3)%len(devices)]
			e.Classification.Browser = browsers[(scattered/5)%len(browsers)]
			e.ReferrerHost = hosts[(scattered/7)%len(hosts)]
		})
	}
	return events
}

const (
	// The aggregates as the events say they should be, and the ones that
	// are there: the rows that are in one and not in the other.
	seriesOff = `
WITH expected AS (
    SELECT link_id, workspace_id, date_trunc('hour', occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour,
           outcome, class, count(*) AS events
      FROM event_inbox GROUP BY 1, 2, 3, 4, 5
), stored AS (
    SELECT link_id, workspace_id, hour, outcome, class, events FROM link_hourly
)
SELECT (SELECT count(*) FROM (TABLE expected EXCEPT TABLE stored) AS missing)
     + (SELECT count(*) FROM (TABLE stored EXCEPT TABLE expected) AS extra)`
	breakdownsOff = `
WITH clicks AS (
    SELECT link_id, workspace_id, (occurred_at AT TIME ZONE 'UTC')::date AS day,
           coalesce(referrer_host, '(none)') AS referrer_host, device, browser
      FROM event_inbox WHERE outcome = 'redirected' AND class = 'presumed_human'
), expected AS (
    SELECT link_id, workspace_id, day, 'referrer_host' AS dimension, referrer_host AS value, count(*) AS events FROM clicks GROUP BY 1, 2, 3, 5
    UNION ALL
    SELECT link_id, workspace_id, day, 'device', device, count(*) FROM clicks GROUP BY 1, 2, 3, 5
    UNION ALL
    SELECT link_id, workspace_id, day, 'browser', browser, count(*) FROM clicks GROUP BY 1, 2, 3, 5
), stored AS (
    SELECT link_id, workspace_id, day, dimension, value, events FROM link_daily_dimension
)
SELECT (SELECT count(*) FROM (TABLE expected EXCEPT TABLE stored) AS missing)
     + (SELECT count(*) FROM (TABLE stored EXCEPT TABLE expected) AS extra)`
	// The versions of the rules as the events that were processed say they
	// should be, and the ones that are recorded.
	versionsOff = `
WITH expected AS (
    SELECT rules_version AS version,
           min(date_trunc('hour', occurred_at, 'UTC')) AS first_hour,
           max(date_trunc('hour', occurred_at, 'UTC')) AS last_hour
      FROM event_inbox WHERE processed_at IS NOT NULL GROUP BY 1
)
SELECT (SELECT count(*) FROM (TABLE expected EXCEPT TABLE rule_versions) AS missing)
     + (SELECT count(*) FROM (TABLE rule_versions EXCEPT TABLE expected) AS extra)`
)

// agrees fails unless the aggregates are what the events of the inbox add
// up to, every event is processed and counted as such once, and the
// versions of the rules are recorded with the hours of their events.
func (s *stage) agrees(t *testing.T, events int) {
	t.Helper()
	if off := s.number(t, seriesOff); off != 0 {
		t.Errorf("%d rows of the time series are not what the events add up to", off)
	}
	if off := s.number(t, breakdownsOff); off != 0 {
		t.Errorf("%d rows of the breakdowns are not what the events add up to", off)
	}
	if off := s.number(t, versionsOff); off != 0 {
		t.Errorf("%d versions of the rules are not recorded with the hours of their events", off)
	}
	if total := s.number(t, `SELECT coalesce(sum(events), 0) FROM link_hourly`); total != int64(events) {
		t.Errorf("the time series adds up to %d events, want %d", total, events)
	}
	if processed := s.number(t, `SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'processed'`); processed != int64(events) {
		t.Errorf("%d events are counted as processed, want %d", processed, events)
	}
	if pending := s.pending(t); pending != 0 {
		t.Errorf("%d events are still pending", pending)
	}
}

// Statements 4 and 6: several consumers at once, and each accepted event
// counts exactly once, whatever order they took the events in.
func TestSeveralConsumersCountEachEventOnce(t *testing.T) {
	s := newStage(t)
	const events = 6000
	s.accept(t, s.varied(events)...)
	s.drain(t, 4)
	s.agrees(t, events)
}

// Statement 4, and the rule on schema that goes with it: consumers that
// write the same rows of an aggregate write them in the order of their
// key. The events of every batch here go to the same few rows, and come
// in an order that is another one for each batch.
func TestAggregateUpsertsInKeyOrderDoNotDeadlock(t *testing.T) {
	const batches = 16
	t.Run("the time series", func(t *testing.T) {
		s := newStage(t)
		few := s.varied(batches * BatchSize)
		for i := range few {
			// Two hours of one day: every batch has every row.
			few[i].OccurredAt = day.Add(time.Duration(10+i%2) * time.Hour)
		}
		s.accept(t, few...)
		s.drain(t, 8)
		s.agrees(t, batches*BatchSize)
	})
	// A consumer that waits for a row of the time series has not reached
	// the breakdowns. Here no row of the series is of two batches: each
	// batch is of an hour of its own, and what the consumers share are the
	// rows of the breakdowns of a day. No click names a referrer, so no
	// consumer waits for the turn of a link either.
	t.Run("the breakdowns", func(t *testing.T) {
		s := newStage(t)
		few := s.varied(batches * BatchSize)
		for i := range few {
			few[i].OccurredAt = day.Add(time.Duration(i/BatchSize) * time.Hour)
			few[i].Outcome, few[i].ReferrerHost = ingest.OutcomeRedirected, ""
			few[i].Classification.Class = classify.ClassPresumedHuman
		}
		s.accept(t, few...)
		s.drain(t, 8)
		s.agrees(t, batches*BatchSize)
	})
}

// Statement 4: a consumer that stops before the commit leaves its batch
// pending for another one. Meanwhile the others do not wait for it.
func TestAConsumerThatStopsBeforeTheCommitLeavesItsBatchPending(t *testing.T) {
	s := newStage(t)
	// The version of the rules already reaches the hour of these events.
	// The first batch of an hour writes the row of its version, which is
	// one for everybody, and the others of that hour wait for its commit.
	s.accept(t, click(s.summer, s.acme, at(10, 0, 0)))
	if processed := s.batch(t); processed != 1 {
		t.Fatalf("Batch processed %d events, want the first", processed)
	}
	// A whole batch of one workspace, and then some events of the other:
	// no row of an aggregate is of both.
	var events []ingest.Event
	for i := range BatchSize {
		events = append(events, click(s.spring, s.acme, at(10, i%60, 0)))
	}
	s.accept(t, events...)
	events = events[:0]
	for i := range 100 {
		events = append(events, click(s.autumn, s.globex, at(10, i%60, 0)))
	}
	s.accept(t, events...)

	// A consumer takes a batch, counts it, and does not commit.
	stopped, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stopped.Rollback(context.WithoutCancel(t.Context())) }()
	taken, err := ingest.Take(t.Context(), stopped, BatchSize)
	if err != nil || len(taken) != BatchSize {
		t.Fatalf("Take returned %d events with the error %v", len(taken), err)
	}
	if err := count(t.Context(), stopped, taken); err != nil {
		t.Fatal(err)
	}

	// Another one takes what is left, without waiting for the events the
	// first one holds.
	quick, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if processed, err := s.consumer.Batch(quick); err != nil || processed != 100 {
		t.Fatalf("with a batch held, Batch processed %d events with the error %v, want the other 100", processed, err)
	}
	if processed, err := s.consumer.Batch(quick); err != nil || processed != 0 {
		t.Fatalf("with a batch held and nothing else pending, Batch processed %d events with the error %v", processed, err)
	}
	if total := s.number(t, `SELECT coalesce(sum(events), 0) FROM link_hourly`); total != 101 {
		t.Fatalf("the time series adds up to %d events, want the first and the 100 of the consumer that committed", total)
	}

	// The first one stops: nothing of what it did is there, and its
	// events are pending.
	if err := stopped.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := s.pending(t); pending != BatchSize {
		t.Fatalf("%d events are pending after the consumer stopped, want its %d", pending, BatchSize)
	}
	if processed, err := s.consumer.Batch(t.Context()); err != nil || processed != BatchSize {
		t.Fatalf("Batch processed %d events with the error %v, want the %d left pending", processed, err, BatchSize)
	}
	s.agrees(t, BatchSize+101)
}

// Consumers whose batches are of the same many links and days take their
// turns in one order, and so never wait for each other in a circle. Each
// batch here is of an hour of its own and of five hundred links and days,
// which it shares with every other batch.
func TestConsumersTakeTheTurnsOfManyLinksAndDaysInOneOrder(t *testing.T) {
	s := newStage(t)
	const batches = 12
	events := make([]ingest.Event, batches*BatchSize)
	for i := range events {
		of := []string{s.spring, s.summer}[i%2]
		on := day.Add(time.Duration((i/2)%(BatchSize/2))*24*time.Hour + time.Duration(i/BatchSize)*time.Hour)
		events[i] = with(click(of, s.acme, on), func(e *ingest.Event) { e.ReferrerHost = "news.example.com" })
	}
	s.accept(t, events...)
	s.drain(t, 6)
	s.agrees(t, batches*BatchSize)
}

// A consumer that needs a row another one holds waits for it, in a line.
// It does not wait for ever: after batchTimeout its batch has failed, and
// its events are pending for the next attempt.
func TestAConsumerGivesUpARowThatAnotherHoldsForTooLong(t *testing.T) {
	s := newStage(t)
	s.accept(t, click(s.spring, s.acme, at(10, 0, 0)), click(s.spring, s.acme, at(10, 1, 0)))

	// One consumer adds an event to a row of the time series and hangs.
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

	// Another one has an event for the same row.
	bounded, cancel := context.WithTimeout(t.Context(), 2*batchTimeout)
	defer cancel()
	started := time.Now()
	processed, err := s.consumer.Batch(bounded)
	if elapsed := time.Since(started); err == nil || processed != 0 || elapsed < batchTimeout || elapsed > batchTimeout+3*time.Second {
		t.Fatalf("Batch processed %d events with the error %v after %s; want none and an error after %s", processed, err, elapsed, batchTimeout)
	}
	if pending := s.pending(t); pending != 2 {
		t.Fatalf("%d events are pending, want both", pending)
	}
	// A batch that ran out of time is not a failure of its events.
	if retried := s.number(t, `SELECT count(*) FROM event_inbox WHERE attempts <> 0`); retried != 0 {
		t.Errorf("%d events have a failed attempt after a batch that ran out of time", retried)
	}

	// The first one ends, and both events are counted, once.
	if err := hung.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.drain(t, 1)
	s.agrees(t, 2)
}

// The tables refuse what the code would not write.
func TestTheAggregatesRefuseWhatTheCodeWouldNotWrite(t *testing.T) {
	s := newStage(t)
	hourly := `INSERT INTO link_hourly (link_id, workspace_id, hour, outcome, class, events) VALUES ($1::uuid, $2::uuid, `
	daily := `INSERT INTO link_daily_dimension (link_id, workspace_id, day, dimension, value, events) VALUES ($1::uuid, $2::uuid, '2026-11-19', `
	for _, refused := range []struct {
		name, statement, constraint string
	}{
		{"an hour that is not on the hour", hourly + `'2026-11-19 10:30:00+00', 'redirected', 'crawler', 1)`, "link_hourly_hour_check"},
		{"an hour that is on the hour somewhere else", hourly + `'2026-11-19 10:00:00+05:30', 'redirected', 'crawler', 1)`, "link_hourly_hour_check"},
		{"an outcome outside the enumeration", hourly + `'2026-11-19 10:00:00+00', 'blocked', 'crawler', 1)`, "link_hourly_outcome_check"},
		{"a class outside the enumeration", hourly + `'2026-11-19 10:00:00+00', 'redirected', 'human', 1)`, "link_hourly_class_check"},
		{"a row of the series that counts nothing", hourly + `'2026-11-19 10:00:00+00', 'redirected', 'crawler', 0)`, "link_hourly_events_check"},
		{"a dimension that is not one", daily + `'country', 'es', 1)`, "link_daily_dimension_dimension_check"},
		{"a device outside the enumeration", daily + `'device', 'watch', 1)`, "link_daily_dimension_device"},
		{"a browser outside the enumeration", daily + `'browser', 'netscape', 1)`, "link_daily_dimension_browser"},
		{"an empty value", daily + `'referrer_host', '', 1)`, "link_daily_dimension_value_check"},
		{"a row of a breakdown that counts nothing", daily + `'device', 'mobile', 0)`, "link_daily_dimension_events_check"},
	} {
		_, err := s.pool.Exec(t.Context(), refused.statement, s.spring, s.acme)
		var answer *pgconn.PgError
		if !errors.As(err, &answer) || answer.Code != "23514" || answer.ConstraintName != refused.constraint {
			t.Errorf("%s: the insert ended with %v, want it refused by %s", refused.name, err, refused.constraint)
		}
	}
	// And they take what it writes.
	for _, statement := range []string{
		hourly + `'2026-11-19 10:00:00+00', 'redirected', 'crawler', 1)`,
		daily + `'device', 'mobile', 1)`, daily + `'referrer_host', '(other)', 1)`,
	} {
		if _, err := s.pool.Exec(t.Context(), statement, s.spring, s.acme); err != nil {
			t.Errorf("an insert the code could make was refused: %v", err)
		}
	}
}

// hosts returns the referrer hosts of a link on a day, with their clicks.
func (s *stage) hosts(t *testing.T, linkID, day string) map[string]int64 {
	t.Helper()
	return s.table(t, fmt.Sprintf(
		`SELECT value, events FROM link_daily_dimension WHERE link_id = '%s' AND day = '%s' AND dimension = 'referrer_host'`, linkID, day))
}

// PLAN.md section 5: at most 100 distinct referrer hosts per link and day,
// the rest under "(other)".
func TestALinkKeepsAHundredReferrerHostsADay(t *testing.T) {
	west(t)
	s := newStage(t)
	from := func(linkID, workspaceID string, at time.Time, host string) ingest.Event {
		return with(click(linkID, workspaceID, at), func(e *ingest.Event) { e.ReferrerHost = host })
	}
	host := func(i int) string { return fmt.Sprintf("site-%03d.example.com", i) }

	var events []ingest.Event
	for i := range 150 {
		events = append(events, from(s.spring, s.acme, at(10, 0, 0), host(i)))
	}
	for range 3 {
		events = append(events, click(s.spring, s.acme, at(10, 0, 0)))
	}
	s.accept(t, events...)
	s.drain(t, 1)
	// Of the hosts that come together, the first hundred by name are
	// kept: which ones does not depend on how the events came.
	want := map[string]int64{OtherReferrers: 50, NoReferrer: 3}
	for i := range MaxReferrerHosts {
		want[host(i)] = 1
	}
	compare(t, "the referrer hosts of the link", s.hosts(t, s.spring, today), want)

	// Later, a host that is kept goes on being counted as itself, and
	// one that is not, new or seen before, goes with the others.
	s.accept(t,
		from(s.spring, s.acme, at(11, 0, 0), host(5)), from(s.spring, s.acme, at(12, 0, 0), host(5)),
		from(s.spring, s.acme, at(11, 0, 0), host(120)), from(s.spring, s.acme, at(11, 0, 0), "new.example.org"),
		// Another day, another link and another workspace have each their
		// own hundred.
		from(s.spring, s.acme, nextDay.Add(time.Hour), "new.example.org"),
		from(s.summer, s.acme, at(11, 0, 0), "new.example.org"),
		from(s.autumn, s.globex, at(11, 0, 0), "new.example.org"),
	)
	s.drain(t, 1)
	want[host(5)], want[OtherReferrers] = 3, 52
	compare(t, "the referrer hosts of the link", s.hosts(t, s.spring, today), want)
	for _, own := range []struct{ link, day string }{{s.spring, tomorrow}, {s.summer, today}, {s.autumn, today}} {
		compare(t, "the referrer hosts of another link or day", s.hosts(t, own.link, own.day), map[string]int64{"new.example.org": 1})
	}
	// Every click is in the breakdown once, under one name or another.
	if clicks := s.number(t, `SELECT sum(events) FROM link_daily_dimension WHERE dimension = 'referrer_host'`); clicks != 160 {
		t.Errorf("the breakdown by referrer adds up to %d clicks, want 160", clicks)
	}

	// The clicks that named no referrer are not a host: a link with 99
	// hosts and some of those clicks keeps one host more.
	events = events[:0]
	for i := range MaxReferrerHosts - 1 {
		events = append(events, from(s.autumn, s.globex, nextDay.Add(time.Hour), host(i)))
	}
	s.accept(t, append(events, click(s.autumn, s.globex, nextDay.Add(time.Hour)))...)
	s.drain(t, 1)
	s.accept(t, from(s.autumn, s.globex, nextDay.Add(2*time.Hour), "hundredth.example.org"))
	s.drain(t, 1)
	s.accept(t, from(s.autumn, s.globex, nextDay.Add(3*time.Hour), "one-too-many.example.org"))
	s.drain(t, 1)
	last := s.hosts(t, s.autumn, tomorrow)
	if len(last) != MaxReferrerHosts+2 || last["hundredth.example.org"] != 1 || last[OtherReferrers] != 1 || last[NoReferrer] != 1 {
		t.Errorf("the link has %d rows of referrers, with %d clicks from its hundredth host, %d from the others and %d from none; want %d rows and one click in each",
			len(last), last["hundredth.example.org"], last[OtherReferrers], last[NoReferrer], MaxReferrerHosts+2)
	}
}

// The cap is exact with several consumers at once: each has counted the
// hosts a link keeps before it adds one, and nobody adds one in between.
func TestConsumersAtOnceKeepTheCapOfReferrerHostsExact(t *testing.T) {
	s := newStage(t)
	const clicks = 12 * BatchSize
	events := make([]ingest.Event, clicks)
	for i := range events {
		// Every click from a host of its own, on two links. Each batch is
		// of an hour of its own: the consumers share no row of the time
		// series, and the first thing one has to wait for is the turn of a
		// link and a day.
		of := []string{s.spring, s.summer}[i%2]
		events[i] = with(click(of, s.acme, day.Add(time.Duration(i/BatchSize)*time.Hour)), func(e *ingest.Event) {
			e.ReferrerHost = fmt.Sprintf("site-%04d.example.com", i)
		})
	}
	s.accept(t, events...)
	s.drain(t, 6)

	for _, linkID := range []string{s.spring, s.summer} {
		hosts := s.hosts(t, linkID, today)
		others := hosts[OtherReferrers]
		delete(hosts, OtherReferrers)
		if len(hosts) != MaxReferrerHosts || others != clicks/2-MaxReferrerHosts {
			t.Errorf("the link keeps %d referrer hosts and counts %d clicks with the others; want %d and %d",
				len(hosts), others, MaxReferrerHosts, clicks/2-MaxReferrerHosts)
		}
		for name, clicked := range maps.All(hosts) {
			if clicked != 1 {
				t.Errorf("the host %s has %d clicks, want 1", name, clicked)
			}
		}
	}
	if pending := s.pending(t); pending != 0 {
		t.Errorf("%d events are still pending", pending)
	}
}
