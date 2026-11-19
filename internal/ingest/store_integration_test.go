//go:build integration

package ingest

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// stage is a store over a real database, with two workspaces and a link in
// each.
type stage struct {
	pool  *pgxpool.Pool
	store *Store
	log   *lockedLog
	// The workspaces, and the link of each.
	acme, globex         string
	acmeLink, globexLink string
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
	s := &stage{pool: pool, log: &lockedLog{}}
	s.store = NewStore(pool, slog.New(slog.NewJSONHandler(s.log, nil)))
	links := link.NewStore(pool, "go.example.dev", rand.Reader)
	for _, owner := range []struct {
		name            string
		workspace, link *string
	}{{"Acme", &s.acme, &s.acmeLink}, {"Globex", &s.globex, &s.globexLink}} {
		workspace, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: owner.name, MaxLinks: 1000, MaxKeys: 10})
		if err != nil {
			t.Fatal(err)
		}
		created, err := links.Create(t.Context(), workspace.ID, link.Draft{Destination: "https://example.com/"})
		if err != nil {
			t.Fatal(err)
		}
		*owner.workspace, *owner.link = workspace.ID, created.ID
	}
	return s
}

// human is what the rules say of an ordinary browser.
var human = classify.Classification{
	Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
	Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
}

// event returns an event of the link of Acme as the buffer would hand it
// over: with its identifier and its time to the second.
func (s *stage) event(at time.Time) Event {
	at = at.UTC().Truncate(time.Second)
	return Event{
		ID: newID(at), LinkID: s.acmeLink, WorkspaceID: s.acme, OccurredAt: at,
		Outcome: OutcomeRedirected, Method: "GET", Classification: human,
	}
}

func (s *stage) events(at time.Time, n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = s.event(at)
	}
	return events
}

// number reads one number from the database.
func (s *stage) number(t *testing.T, query string, arguments ...any) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(t.Context(), query, arguments...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (s *stage) rows(t *testing.T) int64 {
	t.Helper()
	return s.number(t, `SELECT count(*) FROM event_inbox`)
}

// counter reads a counter; one that was never written is at zero. An
// empty workspace is none.
func (s *stage) counter(t *testing.T, day, workspace string, name Counter) int64 {
	t.Helper()
	return s.number(t,
		`SELECT coalesce(sum(value), 0) FROM pipeline_counters
		  WHERE day = $1::date AND workspace_id IS NOT DISTINCT FROM nullif($2, '')::uuid AND name = $3`,
		day, workspace, string(name))
}

// Statement 2: the insert ignores conflicts on the event identifier, so a
// batch written twice leaves one row per event. The second time happens
// when the answer to the first was lost on the way.
func TestABatchInsertedTwiceLeavesOneRowPerEvent(t *testing.T) {
	s := newStage(t)
	batch := s.events(noon, BatchSize)
	for range 2 {
		settled, err := s.store.Accept(t.Context(), batch)
		if err != nil || settled != BatchSize {
			t.Fatalf("Accept settled %d events with the error %v, want %d and none", settled, err, BatchSize)
		}
	}
	if rows := s.rows(t); rows != BatchSize {
		t.Errorf("the inbox has %d rows, want %d", rows, BatchSize)
	}
	// What was accepted is counted by the insert: once.
	if accepted := s.counter(t, "2026-11-19", s.acme, "accepted"); accepted != BatchSize {
		t.Errorf("%d events were counted as accepted, want %d", accepted, BatchSize)
	}
	// A batch that is half new adds its new half.
	half := append(slices.Clone(batch[:10]), s.events(noon, 10)...)
	if settled, err := s.store.Accept(t.Context(), half); err != nil || settled != 20 {
		t.Fatalf("Accept settled %d events with the error %v, want 20 and none", settled, err)
	}
	if rows, accepted := s.rows(t), s.counter(t, "2026-11-19", s.acme, "accepted"); rows != BatchSize+10 || accepted != BatchSize+10 {
		t.Errorf("%d rows and %d counted as accepted, want %d of each", rows, accepted, BatchSize+10)
	}
	// An event that is already there is not one the inbox refuses.
	if refusals := s.log.lines(t, messageRefusedEvent); len(refusals) != 0 {
		t.Errorf("the log has refusals: %v", refusals)
	}
}

func TestAnEventIsStoredAsItWasServed(t *testing.T) {
	s := newStage(t)
	before := time.Now().Add(-time.Second)
	served := s.event(noon)
	served.Outcome, served.Method, served.ReferrerHost = OutcomeInactive, "HEAD", "news.example.com"
	served.Classification = classify.Classification{
		Class: classify.ClassAutomation, Rule: "automation:head", Version: 3,
		Device: classify.DeviceMobile, Browser: classify.BrowserSafari,
	}
	plain := s.event(noon)
	if settled, err := s.store.Accept(t.Context(), []Event{served, plain}); err != nil || settled != 2 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}

	var (
		got                Event
		referrer           *string
		accepted           time.Time
		processed          *time.Time
		device, browser    string
		class, rule        string
		version, precision int
	)
	err := s.pool.QueryRow(t.Context(),
		`SELECT link_id::text, workspace_id::text, occurred_at, outcome, method, referrer_host, device, browser,
		        class, rule, rules_version, accepted_at, processed_at,
		        (SELECT datetime_precision FROM information_schema.columns
		          WHERE table_name = 'event_inbox' AND column_name = 'occurred_at')
		   FROM event_inbox WHERE id = $1`, served.ID).
		Scan(&got.LinkID, &got.WorkspaceID, &got.OccurredAt, &got.Outcome, &got.Method, &referrer, &device, &browser,
			&class, &rule, &version, &accepted, &processed, &precision)
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkID != s.acmeLink || got.WorkspaceID != s.acme || !got.OccurredAt.Equal(noon) || got.Outcome != OutcomeInactive || got.Method != "HEAD" {
		t.Errorf("the event was stored as %+v", got)
	}
	if referrer == nil || *referrer != "news.example.com" {
		t.Errorf("the referrer host was stored as %v", referrer)
	}
	if device != "mobile" || browser != "safari" || class != "automation" || rule != "automation:head" || version != 3 {
		t.Errorf("the classification was stored as %s, %s, %s, %s, %d", device, browser, class, rule, version)
	}
	// Accepted now, by the clock of the database, and not processed.
	if accepted.Before(before) || accepted.After(time.Now().Add(time.Second)) || processed != nil {
		t.Errorf("accepted at %s and processed at %v, want now and never", accepted, processed)
	}
	// The column cannot hold a time finer than the second.
	if precision != 0 {
		t.Errorf("the time of an access is stored with %d decimals of a second, want none", precision)
	}
	// An event without a referrer has none, not an empty one.
	if empty := s.number(t, `SELECT count(*) FROM event_inbox WHERE id = $1 AND referrer_host IS NULL`, plain.ID); empty != 1 {
		t.Error("an event without a referrer was not stored with none")
	}
}

// The enumerations are in the code and, repeated, in the table. The table
// takes every value the code can write.
func TestTheInboxTakesEveryValueTheCodeWrites(t *testing.T) {
	s := newStage(t)
	var batch []Event
	for _, class := range classify.Classes() {
		event := s.event(noon)
		event.Classification.Class = class
		batch = append(batch, event)
	}
	for _, device := range classify.Devices() {
		event := s.event(noon)
		event.Classification.Device = device
		batch = append(batch, event)
	}
	for _, browser := range classify.Browsers() {
		event := s.event(noon)
		event.Classification.Browser = browser
		batch = append(batch, event)
	}
	for _, outcome := range []Outcome{OutcomeRedirected, OutcomeInactive} {
		for _, method := range []string{"GET", "HEAD"} {
			event := s.event(noon)
			event.Outcome, event.Method = outcome, method
			batch = append(batch, event)
		}
	}
	settled, err := s.store.Accept(t.Context(), batch)
	if err != nil || settled != len(batch) {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	if rows := s.rows(t); rows != int64(len(batch)) {
		t.Errorf("the inbox has %d rows of %d events: it refused a value the code writes", rows, len(batch))
	}
	if refusals := s.log.lines(t, messageRefusedEvent); len(refusals) != 0 {
		t.Errorf("the log has refusals: %v", refusals)
	}
}

const (
	messageRefusedEvent = "the inbox refused an event, which is dropped"
	messageRefusedCount = "the database refused a count, which is dropped"
)

// Statement 2: a batch that the database refuses for what it holds is
// inserted event by event, and an event refused on its own is dropped.
func TestOneRefusedEventDoesNotHoldItsBatch(t *testing.T) {
	cases := []struct {
		name       string
		spoil      func(*Event)
		sqlstate   string
		constraint string
	}{
		{"a link that does not exist", func(e *Event) { e.LinkID = "0193a000-0000-7000-8000-0000000000ff" }, "23503", "event_inbox_link_id_fkey"},
		{"a workspace that does not exist", func(e *Event) { e.WorkspaceID = "0193a000-0000-7000-8000-0000000000ff" }, "23503", "event_inbox_workspace_id_fkey"},
		{"an identifier that is not one", func(e *Event) { e.ID = "not-an-identifier" }, "22P02", ""},
		{"an outcome outside the enumeration", func(e *Event) { e.Outcome = "blocked" }, "23514", "event_inbox_outcome_check"},
		{"a method outside the enumeration", func(e *Event) { e.Method = "POST" }, "23514", "event_inbox_method_check"},
		{"a device outside the enumeration", func(e *Event) { e.Classification.Device = "watch" }, "23514", "event_inbox_device_check"},
		{"a browser outside the enumeration", func(e *Event) { e.Classification.Browser = "netscape" }, "23514", "event_inbox_browser_check"},
		{"a class outside the enumeration", func(e *Event) { e.Classification.Class = "human" }, "23514", "event_inbox_class_check"},
		{"no rule", func(e *Event) { e.Classification.Rule = "" }, "23514", "event_inbox_rule_check"},
		{"no version of the rules", func(e *Event) { e.Classification.Version = 0 }, "23514", "event_inbox_rules_version_check"},
		{"a version of the rules that is not a number of one", func(e *Event) { e.Classification.Version = 1 << 40 }, "22003", ""},
		{"a referrer host longer than a name", func(e *Event) { e.ReferrerHost = strings.Repeat("a", 254) }, "23514", "event_inbox_referrer_host_check"},
		{"a text the database cannot store", func(e *Event) { e.Classification.Rule = "crawler:\x00" }, "22021", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStage(t)
			batch := s.events(noon, 5)
			batch[2].ReferrerHost = "refused.example.org"
			c.spoil(&batch[2])

			settled, err := s.store.Accept(t.Context(), batch)
			if err != nil || settled != 5 {
				t.Fatalf("Accept settled %d events with the error %v, want the five and none", settled, err)
			}
			if rows, accepted := s.rows(t), s.counter(t, "2026-11-19", s.acme, "accepted"); rows != 4 || accepted != 4 {
				t.Errorf("%d rows and %d counted as accepted, want the other four", rows, accepted)
			}
			for i, event := range batch {
				if stored := s.number(t, `SELECT count(*) FROM event_inbox WHERE id::text = $1`, event.ID); (stored == 1) == (i == 2) {
					t.Errorf("the event %d is in the inbox %d times", i, stored)
				}
			}
			// The log names the refusal by its code and its constraint, and
			// says nothing of the row.
			refusals := s.log.lines(t, messageRefusedEvent)
			if len(refusals) != 1 || refusals[0]["sqlstate"] != c.sqlstate || refusals[0]["constraint"] != c.constraint || refusals[0]["level"] != "ERROR" {
				t.Errorf("the log says %v, want one refusal with %s and %q", refusals, c.sqlstate, c.constraint)
			}
			s.log.mu.Lock()
			defer s.log.mu.Unlock()
			for _, of := range []string{"refused.example.org", batch[2].LinkID, s.acme, "not-an-identifier"} {
				if strings.Contains(s.log.text.String(), of) {
					t.Errorf("the log quotes the row: it has %q", of)
				}
			}
		})
	}
}

// The day of an accepted event is the one of UTC, whatever time zone the
// database works in.
func TestAnAcceptedEventIsCountedOnItsDayOfUTC(t *testing.T) {
	s := newStage(t)
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	// Nine hours ahead: eight in the evening is already the next day.
	config.ConnConfig.RuntimeParams["timezone"] = "Asia/Tokyo"
	ahead, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ahead.Close)

	evening := time.Date(2026, time.November, 19, 20, 0, 0, 0, time.UTC)
	store := NewStore(ahead, slog.New(slog.NewJSONHandler(s.log, nil)))
	if settled, err := store.Accept(t.Context(), s.events(evening, 3)); err != nil || settled != 3 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	if today, tomorrow := s.counter(t, "2026-11-19", s.acme, "accepted"), s.counter(t, "2026-11-20", s.acme, "accepted"); today != 3 || tomorrow != 0 {
		t.Errorf("%d events were counted on the 19th and %d on the 20th, want the 3 on the 19th", today, tomorrow)
	}
}

// With the database out of reach nothing is settled: what was given is
// tried again.
func TestWithTheDatabaseOutOfReachNothingIsSettled(t *testing.T) {
	s := newStage(t)
	s.pool.Close()
	if settled, err := s.store.Accept(context.Background(), s.events(noon, 5)); err == nil || settled != 0 {
		t.Errorf("Accept settled %d events with the error %v, want none and an error", settled, err)
	}
	counts := []Count{{Day: noon.Truncate(24 * time.Hour), WorkspaceID: s.acme, Name: counterServed, Value: 5}}
	if settled, err := s.store.Count(context.Background(), counts); err == nil || settled != 0 {
		t.Errorf("Count settled %d counts with the error %v, want none and an error", settled, err)
	}
	if refusals := len(s.log.lines(t, messageRefusedEvent)) + len(s.log.lines(t, messageRefusedCount)); refusals != 0 {
		t.Errorf("the log has %d refusals: a database that is away refuses nothing", refusals)
	}
}

func TestCountsAddToTheCounterOfTheirDayAndWorkspace(t *testing.T) {
	s := newStage(t)
	today, tomorrow := noon.Truncate(24*time.Hour), noon.Truncate(24*time.Hour).Add(24*time.Hour)
	for range 2 {
		settled, err := s.store.Count(t.Context(), []Count{
			{Day: today, WorkspaceID: s.acme, Name: counterServed, Value: 7},
			{Day: today, WorkspaceID: s.globex, Name: counterServed, Value: 2},
			{Day: tomorrow, WorkspaceID: s.acme, Name: counterServed, Value: 1},
			{Day: today, Name: CounterNotFound, Value: 3},
			{Day: today, Name: CounterRateLimited, Value: 4},
			{Day: tomorrow, Name: CounterNotFound, Value: 5},
		})
		if err != nil || settled != 6 {
			t.Fatalf("Count settled %d counts with the error %v", settled, err)
		}
	}
	for _, want := range []struct {
		day, workspace string
		name           Counter
		value          int64
	}{
		{"2026-11-19", s.acme, counterServed, 14},
		{"2026-11-19", s.globex, counterServed, 4},
		{"2026-11-20", s.acme, counterServed, 2},
		{"2026-11-19", "", CounterNotFound, 6},
		{"2026-11-19", "", CounterRateLimited, 8},
		{"2026-11-20", "", CounterNotFound, 10},
	} {
		if got := s.counter(t, want.day, want.workspace, want.name); got != want.value {
			t.Errorf("the counter %s of %s and %q is at %d, want %d", want.name, want.day, want.workspace, got, want.value)
		}
	}
	// One row per counter, also for the ones of no workspace.
	if rows := s.number(t, `SELECT count(*) FROM pipeline_counters`); rows != 6 {
		t.Errorf("the counters are %d rows, want 6", rows)
	}
}

// A count that the database refuses is dropped, and the ones written with
// it are written.
func TestARefusedCountDoesNotHoldTheOthers(t *testing.T) {
	s := newStage(t)
	today := noon.Truncate(24 * time.Hour)
	settled, err := s.store.Count(t.Context(), []Count{
		{Day: today, WorkspaceID: s.acme, Name: counterServed, Value: 7},
		{Day: today, WorkspaceID: "0193a000-0000-7000-8000-0000000000ff", Name: counterServed, Value: 2},
		{Day: today, Name: CounterNotFound, Value: 3},
		// What is served is of a workspace, and what names no link is of none.
		{Day: today, Name: counterServed, Value: 1},
		{Day: today, WorkspaceID: s.acme, Name: CounterNotFound, Value: 1},
		{Day: today, WorkspaceID: s.acme, Name: "invented", Value: 1},
	})
	if err != nil || settled != 6 {
		t.Fatalf("Count settled %d counts with the error %v, want the six and none", settled, err)
	}
	if served, notFound, rows := s.counter(t, "2026-11-19", s.acme, counterServed), s.counter(t, "2026-11-19", "", CounterNotFound), s.number(t, `SELECT count(*) FROM pipeline_counters`); served != 7 || notFound != 3 || rows != 2 {
		t.Errorf("served is at %d and not found at %d, in %d rows; want 7 and 3 in two", served, notFound, rows)
	}
	var codes []string
	for _, refusal := range s.log.lines(t, messageRefusedCount) {
		codes = append(codes, fmt.Sprint(refusal["sqlstate"], " ", refusal["constraint"]))
	}
	want := []string{
		"23503 pipeline_counters_workspace_id_fkey", "23514 pipeline_counters_owner",
		"23514 pipeline_counters_owner", "23514 pipeline_counters_name_check",
	}
	if !slices.Equal(codes, want) {
		t.Errorf("the log has the refusals %v, want %v", codes, want)
	}
}

// Several processes add to the same counters at once. Each writes its rows
// in the order of their key, so none waits for a row that another holds
// while holding one the other waits for.
func TestProcessesThatCountAtOnceDoNotBlockEachOther(t *testing.T) {
	s := newStage(t)
	const processes, rounds = 8, 40
	var days []time.Time
	for day := range 10 {
		days = append(days, noon.Truncate(24*time.Hour).Add(time.Duration(day)*24*time.Hour))
	}
	var wait sync.WaitGroup
	var failed atomic.Int64
	for process := range processes {
		wait.Go(func() {
			for range rounds {
				var counts []Count
				for _, day := range days {
					counts = append(counts,
						Count{Day: day, WorkspaceID: s.acme, Name: counterServed, Value: 1},
						Count{Day: day, WorkspaceID: s.globex, Name: counterServed, Value: 1},
						Count{Day: day, Name: CounterNotFound, Value: 1})
				}
				// Whatever order they are given in: each process has its
				// own, and half of them the opposite of the others.
				counts = append(counts[process*3:], counts[:process*3]...)
				if process%2 == 1 {
					slices.Reverse(counts)
				}
				if settled, err := s.store.Count(t.Context(), counts); err != nil || settled != len(counts) {
					failed.Add(1)
					t.Errorf("Count settled %d counts with the error %v", settled, err)
					return
				}
			}
		})
	}
	wait.Wait()
	if failed.Load() != 0 {
		return
	}
	if total := s.number(t, `SELECT sum(value) FROM pipeline_counters`); total != processes*rounds*30 {
		t.Errorf("the counters add up to %d, want %d", total, processes*rounds*30)
	}
	if lowest := s.number(t, `SELECT min(value) FROM pipeline_counters`); lowest != processes*rounds {
		t.Errorf("a counter is at %d, want every one at %d", lowest, processes*rounds)
	}
}

// The same for the events several processes insert at once: the count of
// what each accepted goes to rows that the others write too.
func TestProcessesThatAcceptAtOnceDoNotBlockEachOther(t *testing.T) {
	s := newStage(t)
	const processes, rounds, days = 8, 30, 10
	var wait sync.WaitGroup
	for process := range processes {
		wait.Go(func() {
			for range rounds {
				// Each process serves on some of the days, its own, and in
				// its own order.
				var batch []Event
				for day := process; day < process+3+process%4; day++ {
					at := noon.Add(time.Duration(day%days) * 24 * time.Hour)
					event := s.event(at)
					other := s.event(at)
					other.LinkID, other.WorkspaceID = s.globexLink, s.globex
					batch = append(batch, event, other)
				}
				if process%2 == 1 {
					slices.Reverse(batch)
				}
				if settled, err := s.store.Accept(t.Context(), batch); err != nil || settled != len(batch) {
					t.Errorf("Accept settled %d events with the error %v", settled, err)
					return
				}
			}
		})
	}
	wait.Wait()
	if t.Failed() {
		return
	}
	if rows, accepted := s.rows(t), s.number(t, `SELECT sum(value) FROM pipeline_counters WHERE name = 'accepted'`); rows != accepted {
		t.Errorf("the inbox has %d rows and %d are counted as accepted", rows, accepted)
	}
}

// failing is a database that stops answering at one of the statements it
// is sent, counted from the first.
type failing struct {
	executor
	at, sent int
}

func (f *failing) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	f.sent++
	if f.sent == f.at {
		return pgconn.CommandTag{}, errAway
	}
	return f.executor.Exec(ctx, sql, arguments...)
}

// The database can go away while what it refused together is written one
// by one. What was written before is settled, and the rest is not: tried
// again, nothing is written twice and nothing is missing.
func TestTheDatabaseGoesAwayWhileABatchIsWrittenOneByOne(t *testing.T) {
	s := newStage(t)
	today := noon.Truncate(24 * time.Hour)
	missing := "0193a000-0000-7000-8000-0000000000ff"

	// The first statement is the whole batch, which is refused; the second
	// and the third are its first two events; the fourth does not answer.
	batch := s.events(noon, 5)
	batch[1].WorkspaceID = missing
	s.store.db = &failing{executor: s.pool, at: 4}
	settled, err := s.store.Accept(t.Context(), batch)
	if err == nil || settled != 2 {
		t.Fatalf("Accept settled %d events with the error %v, want 2 and an error", settled, err)
	}
	s.store.db = s.pool
	if settled, err := s.store.Accept(t.Context(), batch[settled:]); err != nil || settled != 3 {
		t.Fatalf("Accept settled %d events with the error %v, want the other 3", settled, err)
	}
	if rows, accepted := s.rows(t), s.counter(t, "2026-11-19", s.acme, "accepted"); rows != 4 || accepted != 4 {
		t.Errorf("%d rows and %d counted as accepted, want 4 of each", rows, accepted)
	}

	counts := []Count{
		{Day: today, WorkspaceID: s.acme, Name: counterServed, Value: 7},
		{Day: today, WorkspaceID: missing, Name: counterServed, Value: 2},
		{Day: today, WorkspaceID: s.globex, Name: counterServed, Value: 3},
		{Day: today, Name: CounterNotFound, Value: 5},
	}
	s.store.db = &failing{executor: s.pool, at: 4}
	settled, err = s.store.Count(t.Context(), counts)
	if err == nil || settled != 2 {
		t.Fatalf("Count settled %d counts with the error %v, want 2 and an error", settled, err)
	}
	s.store.db = s.pool
	if settled, err := s.store.Count(t.Context(), counts[settled:]); err != nil || settled != 2 {
		t.Fatalf("Count settled %d counts with the error %v, want the other 2", settled, err)
	}
	if acme, globex, notFound := s.counter(t, "2026-11-19", s.acme, counterServed), s.counter(t, "2026-11-19", s.globex, counterServed), s.counter(t, "2026-11-19", "", CounterNotFound); acme != 7 || globex != 3 || notFound != 5 {
		t.Errorf("the counters are at %d, %d and %d; want 7, 3 and 5, each written once", acme, globex, notFound)
	}
}

// gate is an inbox that a test closes, as a database that goes away.
type gate struct {
	Inbox
	closed atomic.Bool
}

func (g *gate) Accept(ctx context.Context, events []Event) (int, error) {
	if g.closed.Load() {
		return 0, errAway
	}
	return g.Inbox.Accept(ctx, events)
}

func (g *gate) Count(ctx context.Context, counts []Count) (int, error) {
	if g.closed.Load() {
		return 0, errAway
	}
	return g.Inbox.Count(ctx, counts)
}

// The first equality of the reconciliation, for every UTC day and every
// workspace: served = accepted + lost. Events are dropped on purpose, by
// serving more than the buffer holds while the database is away.
func TestTheReconciliationAddsUp(t *testing.T) {
	s := newStage(t)
	into := &gate{Inbox: s.store}
	buffer, _ := newBuffer(into)
	into.closed.Store(true)

	tomorrow := noon.Add(24 * time.Hour)
	serve := func(n int, workspace, link string, at time.Time) {
		for range n {
			buffer.Record(Event{
				LinkID: link, WorkspaceID: workspace, OccurredAt: at,
				Outcome: OutcomeRedirected, Method: "GET", Classification: human,
			})
		}
	}
	// The buffer holds what came first; the last responses find it full.
	serve(30_000, s.acme, s.acmeLink, noon)
	serve(Capacity-40_000, s.acme, s.acmeLink, tomorrow)
	serve(10_000, s.globex, s.globexLink, tomorrow)
	serve(25, s.acme, s.acmeLink, tomorrow)
	serve(12, s.globex, s.globexLink, tomorrow)
	buffer.flush(t.Context())
	if rows := s.rows(t); rows != 0 {
		t.Fatalf("with the database away the inbox has %d rows", rows)
	}

	into.closed.Store(false)
	buffer.flush(t.Context())
	for _, want := range []struct {
		day, workspace string
		served, lost   int64
	}{
		{"2026-11-19", s.acme, 30_000, 0},
		{"2026-11-20", s.acme, Capacity - 40_000 + 25, 25},
		{"2026-11-20", s.globex, 10_012, 12},
		{"2026-11-19", s.globex, 0, 0},
	} {
		served := s.counter(t, want.day, want.workspace, counterServed)
		accepted := s.counter(t, want.day, want.workspace, "accepted")
		if served != want.served || served-accepted != want.lost {
			t.Errorf("on %s the workspace %s has %d served and %d accepted; want %d served and %d lost",
				want.day, want.workspace, served, accepted, want.served, want.lost)
		}
		// What is counted as accepted is what is in the inbox.
		rows := s.number(t,
			`SELECT count(*) FROM event_inbox WHERE workspace_id = $1::uuid AND (occurred_at AT TIME ZONE 'UTC')::date = $2::date`,
			want.workspace, want.day)
		if rows != accepted {
			t.Errorf("on %s the workspace %s has %d events in the inbox and %d counted as accepted", want.day, want.workspace, rows, accepted)
		}
	}
	if rows := s.rows(t); rows != Capacity {
		t.Errorf("the inbox has %d rows, want the %d that fitted in the buffer", rows, Capacity)
	}
	// Every event has an identifier of its own second.
	if finer := s.number(t, `SELECT count(*) FROM event_inbox WHERE uuid_extract_timestamp(id) <> occurred_at`); finer != 0 {
		t.Errorf("%d events have an identifier that does not say the second of the event", finer)
	}
}
