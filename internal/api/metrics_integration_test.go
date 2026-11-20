//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// The day the metrics of these tests are of.
var metricsDay = time.Date(2026, time.November, 19, 0, 0, 0, 0, time.UTC)

// on returns a moment of that day.
func on(hour, minute int) time.Time {
	return metricsDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// visit returns the event of an estimated human click on a link: a
// redirect to somebody on a desktop Chrome that named no referrer.
func visit(target link.Link, at time.Time, changes ...func(*ingest.Event)) ingest.Event {
	event := ingest.Event{
		ID: uuid.NewV7().String(), LinkID: target.ID, WorkspaceID: target.WorkspaceID, OccurredAt: at,
		Outcome: ingest.OutcomeRedirected, Method: "GET",
		Classification: classify.Classification{
			Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
			Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
		},
	}
	for _, change := range changes {
		change(&event)
	}
	return event
}

func referred(host string) func(*ingest.Event) {
	return func(e *ingest.Event) { e.ReferrerHost = host }
}

func classed(class classify.Class) func(*ingest.Event) {
	return func(e *ingest.Event) { e.Classification.Class = class }
}

func unserved(e *ingest.Event) { e.Outcome = ingest.OutcomeInactive }

// accept writes events to the inbox, as the redirect does.
func (s *stage) accept(t *testing.T, events ...ingest.Event) {
	t.Helper()
	inbox := ingest.NewStore(s.pool, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if settled, err := inbox.Accept(t.Context(), events); err != nil || settled != len(events) {
		t.Fatalf("Accept settled %d of %d events with the error %v", settled, len(events), err)
	}
}

// consume adds what waits in the inbox to the aggregates, as the worker
// does.
func (s *stage) consume(t *testing.T) {
	t.Helper()
	consumer := rollup.NewConsumer(s.pool, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	for {
		processed, err := consumer.Batch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if processed == 0 {
			return
		}
	}
}

// moment asks the database something that is a moment.
func (s *stage) moment(t *testing.T, query string, arguments ...any) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(t.Context(), query, arguments...).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// answered is what the three operations answer, with the parts each adds.
type answered struct {
	LinkID           string    `json:"link_id"`
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	ProcessedThrough time.Time `json:"processed_through"`
	Synthetic        bool      `json:"synthetic"`
	RuleVersions     []struct {
		Version   int       `json:"version"`
		FirstHour time.Time `json:"first_hour"`
		LastHour  time.Time `json:"last_hour"`
	} `json:"rule_versions"`
	shownFigures
	Granularity string `json:"granularity"`
	Points      []struct {
		Start time.Time `json:"start"`
		shownFigures
	} `json:"points"`
	Dimension string `json:"dimension"`
	Items     []struct {
		Value  string `json:"value"`
		Clicks int64  `json:"estimated_human_clicks"`
	} `json:"items"`
	Other int64 `json:"other"`
}

// metrics asks one of the three operations about a link, which must
// answer, and returns what it said.
func (s *stage) metrics(t *testing.T, credential, id, operation string, from, to time.Time, more ...string) (answered, []byte) {
	t.Helper()
	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	for i := 0; i+1 < len(more); i += 2 {
		query.Set(more[i], more[i+1])
	}
	response, body := s.do(t, credential, http.MethodGet, "/v1/links/"+id+"/metrics/"+operation+"?"+query.Encode(), nil)
	if response.status != http.StatusOK {
		t.Fatalf("GET %s of the metrics = %d %s", operation, response.status, body)
	}
	return read[answered](t, body), body
}

// newTarget creates a link of a workspace.
func (s *stage) newTarget(t *testing.T, workspaceID string) link.Link {
	t.Helper()
	created, err := s.store.Create(t.Context(), workspaceID, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// Statement 7 of docs/delivery-semantics.md: metrics are eventually
// consistent, and every response says how far they reach.
func TestMetricsSayThroughWhenTheyAreProcessed(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	key := s.key(t, owner, auth.ScopeMetricsRead)
	target := s.newTarget(t, owner)
	from, to := metricsDay, metricsDay.Add(24*time.Hour)
	summary := func() answered {
		t.Helper()
		got, _ := s.metrics(t, key, target.ID, "summary", from, to)
		return got
	}
	// current fails unless an answer reaches the moment it is given at, by
	// the clock of the database, which is the one that accepts the events.
	current := func(what string, requests int64) {
		t.Helper()
		before := s.moment(t, `SELECT clock_timestamp()`)
		got := summary()
		after := s.moment(t, `SELECT clock_timestamp()`)
		if got.ProcessedThrough.Before(before) || got.ProcessedThrough.After(after) {
			t.Errorf("%s: the figures reach %s, want the moment of the answer, between %s and %s", what, got.ProcessedThrough, before, after)
		}
		if got.Requests != requests {
			t.Errorf("%s: %d requests, want %d", what, got.Requests, requests)
		}
	}

	current("with nothing accepted", 0)

	// One access is counted. Two more are accepted and wait: the worker is
	// behind.
	s.accept(t, visit(target, on(10, 0)))
	s.consume(t)
	second, third := visit(target, on(10, 1)), visit(target, on(10, 2))
	s.accept(t, third, second)
	for event, ago := range map[string]string{second.ID: "5 minutes", third.ID: "2 minutes"} {
		if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET accepted_at = now() - $2::interval WHERE id = $1::uuid`, event, ago); err != nil {
			t.Fatal(err)
		}
	}
	oldest := s.moment(t, `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, second.ID)

	// The figures reach the acceptance of the oldest event that waits, and
	// hold what was counted. The moment is of the acceptance, and has
	// nothing to do with the period that is asked for.
	if got := summary(); !got.ProcessedThrough.Equal(oldest) || got.Requests != 1 {
		t.Fatalf("with two events waiting, the figures reach %s with %d requests; want %s, the acceptance of the oldest, and 1", got.ProcessedThrough, got.Requests, oldest)
	}
	if got := summary(); got.ProcessedThrough.Location() != time.UTC {
		t.Errorf("the moment is in %s, want it in UTC", got.ProcessedThrough.Location())
	}
	// Every answer about metrics says it, not only the summary.
	series, _ := s.metrics(t, key, target.ID, "series", from, to, "granularity", "day")
	breakdown, _ := s.metrics(t, key, target.ID, "breakdown", from, to, "dimension", "device")
	if !series.ProcessedThrough.Equal(oldest) || !breakdown.ProcessedThrough.Equal(oldest) {
		t.Errorf("the series reaches %s and the breakdown %s, want %s", series.ProcessedThrough, breakdown.ProcessedThrough, oldest)
	}
	if got, _ := s.metrics(t, key, target.ID, "summary", from.Add(-240*time.Hour), from); !got.ProcessedThrough.Equal(oldest) {
		t.Errorf("asked about other days, the figures reach %s, want %s", got.ProcessedThrough, oldest)
	}

	// The worker catches up.
	s.consume(t)
	current("with everything processed", 3)
}

// hold is a tracer that stops a connection before a statement.
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

// Statement 7: the moment an answer says its figures reach is read before
// the figures. An access that is accepted and counted while the answer is
// being made is then in the figures, or after the moment: never before the
// moment and missing.
func TestTheFiguresOfAnAnswerHoldWhatItSaysTheyReach(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	key := s.key(t, owner, auth.ScopeMetricsRead)
	target := s.newTarget(t, owner)

	// The API on connections that stop before they ask how far the events
	// are processed.
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	stopped := &hold{at: "FROM event_inbox WHERE processed_at IS NULL", reached: make(chan struct{}), release: make(chan struct{})}
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
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	held := *s
	held.handler, err = New(t.Context(), Settings{
		Logger: quiet, Keys: s.keys, Links: link.NewStore(own, redirectDomain, rand.Reader),
		Inbox: ingest.NewStore(own, quiet), Aggregates: rollup.NewReader(own),
	})
	if err != nil {
		t.Fatal(err)
	}

	said := make(chan answered, 1)
	go func() {
		got, _ := held.metrics(t, key, target.ID, "summary", metricsDay, metricsDay.Add(24*time.Hour))
		said <- got
	}()
	select {
	case <-stopped.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer never asked how far the events are processed")
	}

	// Meanwhile an access is accepted and counted.
	event := visit(target, on(10, 0))
	s.accept(t, event)
	s.consume(t)
	accepted := s.moment(t, `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, event.ID)

	release()
	got := <-said
	if !got.ProcessedThrough.Before(accepted) && got.Requests != 1 {
		t.Errorf("the answer says its figures reach %s, after an event accepted at %s, and has %d requests: the event is missing",
			got.ProcessedThrough, accepted, got.Requests)
	}
	// The moment was asked for after the access was counted: it is past it.
	if got.ProcessedThrough.Before(accepted) {
		t.Errorf("the figures reach %s, before the event accepted at %s that the test counted first", got.ProcessedThrough, accepted)
	}
}

func TestTheMetricsOfALinkAreItsFiguresOverThePeriod(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	key := s.key(t, owner, auth.ScopeMetricsRead)
	target, other := s.newTarget(t, owner), s.newTarget(t, owner)
	s.accept(t,
		visit(target, on(10, 0), referred("news.example.com")),
		visit(target, on(10, 59), referred("news.example.com")),
		visit(target, on(12, 30), referred("blog.example.org"), func(e *ingest.Event) {
			e.Classification.Device, e.Classification.Browser = classify.DeviceMobile, classify.BrowserSafari
		}),
		visit(target, on(12, 31)),
		visit(target, on(12, 32), classed(classify.ClassCrawler), referred("search.example.net")),
		visit(target, on(13, 0), unserved),
		// The next day, and another link.
		visit(target, on(24, 0), referred("later.example.com")),
		visit(other, on(10, 0), referred("other.example.com")),
	)
	s.consume(t)
	from, to := on(10, 0), on(14, 0)

	summary, body := s.metrics(t, key, target.ID, "summary", from, to)
	if want := (shownFigures{Requests: 5, InactiveRequests: 1, EstimatedHumanClicks: 4}); summary.shownFigures != want {
		t.Errorf("the summary is %+v, want %+v: %s", summary.shownFigures, want, body)
	}
	// What every answer carries: the link, the period in UTC, that the
	// traffic is not generated, and the one version of the rules, with the
	// hours of its first and of its last event in any link.
	if summary.LinkID != target.ID || !summary.From.Equal(from) || !summary.To.Equal(to) || summary.Synthetic {
		t.Errorf("the summary is about %s from %s to %s, synthetic %v", summary.LinkID, summary.From, summary.To, summary.Synthetic)
	}
	if !strings.Contains(string(body), `"from":"2026-11-19T10:00:00Z","to":"2026-11-19T14:00:00Z"`) {
		t.Errorf("the period is not written in UTC: %s", body)
	}
	if len(summary.RuleVersions) != 1 || summary.RuleVersions[0].Version != 1 ||
		!summary.RuleVersions[0].FirstHour.Equal(on(10, 0)) || !summary.RuleVersions[0].LastHour.Equal(on(24, 0)) {
		t.Errorf("the versions of the rules are %+v, want version 1 from %s to %s", summary.RuleVersions, on(10, 0), on(24, 0))
	}
	// A period before any event has no version of the rules, and says so
	// with an empty list.
	if _, early := s.metrics(t, key, target.ID, "summary", on(-48, 0), on(-24, 0)); !strings.Contains(string(early), `"rule_versions":[]`) {
		t.Errorf("a period before any event: %s", early)
	}

	// The same period said from another time zone, and the link named in
	// capitals: the answer is the same, byte for byte but for its moment.
	madrid := time.FixedZone("", 3600)
	again, _ := s.metrics(t, key, strings.ToUpper(target.ID), "summary", from.In(madrid), to.In(madrid))
	again.ProcessedThrough = summary.ProcessedThrough
	if string(mustJSON(t, again)) != string(mustJSON(t, summary)) {
		t.Errorf("asked from another zone the summary is %+v, want %+v", again, summary)
	}

	series, body := s.metrics(t, key, target.ID, "series", from, to, "granularity", "hour")
	wantPoints := []shownFigures{
		{Requests: 2, EstimatedHumanClicks: 2}, {}, {Requests: 3, EstimatedHumanClicks: 2}, {InactiveRequests: 1},
	}
	if series.Granularity != "hour" || len(series.Points) != len(wantPoints) {
		t.Fatalf("the series by the hour has %d points, want %d: %s", len(series.Points), len(wantPoints), body)
	}
	for i, point := range series.Points {
		if !point.Start.Equal(on(10+i, 0)) || point.shownFigures != wantPoints[i] {
			t.Errorf("point %d of the series is %+v at %s, want %+v at %s", i, point.shownFigures, point.Start, wantPoints[i], on(10+i, 0))
		}
	}
	byDay, body := s.metrics(t, key, target.ID, "series", on(-24, 0), on(48, 0), "granularity", "day")
	if byDay.Granularity != "day" || len(byDay.Points) != 3 || byDay.Points[0].shownFigures != (shownFigures{}) ||
		byDay.Points[1].shownFigures != (shownFigures{Requests: 5, InactiveRequests: 1, EstimatedHumanClicks: 4}) ||
		byDay.Points[2].shownFigures != (shownFigures{Requests: 1, EstimatedHumanClicks: 1}) || !byDay.Points[2].Start.Equal(on(24, 0)) {
		t.Errorf("the series by the day: %s", body)
	}

	type item = struct {
		Value  string `json:"value"`
		Clicks int64  `json:"estimated_human_clicks"`
	}
	day, next := on(0, 0), on(24, 0)
	for _, c := range []struct {
		name      string
		dimension string
		limit     string
		want      []item
		other     int64
	}{
		{"by referrer host", "referrer_host", "", []item{{"news.example.com", 2}, {"(none)", 1}, {"blog.example.org", 1}}, 0},
		{"the first host", "referrer_host", "1", []item{{"news.example.com", 2}}, 2},
		{"by device", "device", "", []item{{"desktop", 3}, {"mobile", 1}}, 0},
		{"by browser", "browser", "100", []item{{"chrome", 3}, {"safari", 1}}, 0},
	} {
		more := []string{"dimension", c.dimension}
		if c.limit != "" {
			more = append(more, "limit", c.limit)
		}
		breakdown, body := s.metrics(t, key, target.ID, "breakdown", day, next, more...)
		if breakdown.Dimension != c.dimension || !slices.Equal(breakdown.Items, c.want) || breakdown.Other != c.other {
			t.Errorf("%s: %s; want %v and %d others", c.name, body, c.want, c.other)
		}
	}
	// Twenty-five hosts on a day of their own, one click each: twenty are
	// named unless more are asked for, and the other five are added up.
	var many []ingest.Event
	for i := range 25 {
		many = append(many, visit(target, on(120, i), referred(string(rune('a'+i))+".example.com")))
	}
	s.accept(t, many...)
	s.consume(t)
	fifth, sixth := on(120, 0), on(144, 0)
	if unasked, body := s.metrics(t, key, target.ID, "breakdown", fifth, sixth, "dimension", "referrer_host"); len(unasked.Items) != 20 ||
		unasked.Items[0].Value != "a.example.com" || unasked.Items[19].Value != "t.example.com" || unasked.Other != 5 {
		t.Errorf("a breakdown of 25 hosts, with no limit: %s; want the first twenty by their names and 5 others", body)
	}
	if all, body := s.metrics(t, key, target.ID, "breakdown", fifth, sixth, "dimension", "referrer_host", "limit", "25"); len(all.Items) != 25 || all.Other != 0 {
		t.Errorf("a breakdown of 25 hosts, with a limit of 25: %s; want them all", body)
	}

	// With nothing to break down the list is empty, not missing.
	if _, empty := s.metrics(t, key, target.ID, "breakdown", on(48, 0), on(72, 0), "dimension", "device"); !strings.Contains(string(empty), `"items":[],"other":0`) {
		t.Errorf("a breakdown of a day with nothing: %s", empty)
	}

	// A blocked link is read, and its metrics with it. A deleted one is not.
	if _, err := s.store.Block(t.Context(), target.Identifier); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := s.metrics(t, key, target.ID, "summary", from, to); blocked.Requests != 5 {
		t.Errorf("the summary of a blocked link has %d requests, want 5", blocked.Requests)
	}
	if err := s.store.Delete(t.Context(), owner, target.ID); err != nil {
		t.Fatal(err)
	}
	gone, body := s.do(t, key, http.MethodGet, "/v1/links/"+target.ID+"/metrics/summary?from=2026-11-19T10:00:00Z&to=2026-11-19T14:00:00Z", nil)
	if gone.status != http.StatusNotFound || code(t, body) != CodeNotFound {
		t.Errorf("the metrics of a deleted link = %d %s, want 404 %s", gone.status, body, CodeNotFound)
	}
}

// The traffic of a demo workspace is generated, and every answer about it
// says so.
func TestTheMetricsOfADemoWorkspaceSayTheyAreSynthetic(t *testing.T) {
	s := newStage(t)
	demo := s.workspace(t, link.WorkspaceDraft{Name: "Demo", Demo: true})
	key := s.key(t, demo, auth.ScopeMetricsRead)
	target := s.newTarget(t, demo)
	from, to := metricsDay, metricsDay.Add(24*time.Hour)
	for operation, more := range map[string][]string{
		"summary": nil, "series": {"granularity", "hour"}, "breakdown": {"dimension", "browser"},
	} {
		if got, body := s.metrics(t, key, target.ID, operation, from, to, more...); !got.Synthetic {
			t.Errorf("the %s of a link of a demo workspace does not say that it is synthetic: %s", operation, body)
		}
	}
}

func TestAPeriodThatIsNotOneIsRejectedWithItsReason(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	key := s.key(t, owner, auth.ScopeMetricsRead)
	target := s.newTarget(t, owner)
	const (
		hour10 = "2026-11-19T10:00:00Z"
		hour11 = "2026-11-19T11:00:00Z"
		day19  = "2026-11-19T00:00:00Z"
		day20  = "2026-11-20T00:00:00Z"
	)
	query := func(pairs ...string) string {
		values := url.Values{}
		for i := 0; i+1 < len(pairs); i += 2 {
			values.Set(pairs[i], pairs[i+1])
		}
		return values.Encode()
	}
	for _, c := range []struct {
		name      string
		operation string
		query     string
		want      []rejection
	}{
		{"no period", "summary", "", []rejection{{inQuery, "from", ReasonRequired}, {inQuery, "to", ReasonRequired}}},
		{"no end", "summary", query("from", hour10), []rejection{{inQuery, "to", ReasonRequired}}},
		{"a beginning that is not a moment", "summary", query("from", "yesterday", "to", hour11), []rejection{{inQuery, "from", ReasonInvalid}}},
		{"a day without its time", "summary", query("from", "2026-11-19", "to", hour11), []rejection{{inQuery, "from", ReasonInvalid}}},
		{"a moment that the calendar does not have", "summary", query("from", "2026-02-30T00:00:00Z", "to", hour11), []rejection{{inQuery, "from", ReasonInvalid}}},
		{"neither is a moment", "summary", query("from", "then", "to", "now"), []rejection{{inQuery, "from", ReasonInvalid}, {inQuery, "to", ReasonInvalid}}},
		{"a sixtieth second", "summary", query("from", "2026-11-18T23:59:60Z", "to", hour11), []rejection{{inQuery, "from", ReasonInvalid}}},
		{"sixtieth seconds", "summary", query("from", "2026-11-18T23:59:60Z", "to", "2026-11-19T23:59:60Z"), []rejection{{inQuery, "from", ReasonInvalid}, {inQuery, "to", ReasonInvalid}}},
		{"a beginning past the hour", "summary", query("from", "2026-11-19T10:00:01Z", "to", hour11), []rejection{{inQuery, "from", ReasonNotAligned}}},
		{"an end past the hour", "summary", query("from", hour10, "to", "2026-11-19T11:30:00Z"), []rejection{{inQuery, "to", ReasonNotAligned}}},
		{"an hour of somewhere else", "summary", query("from", "2026-11-19T10:00:00+05:30", "to", hour11), []rejection{{inQuery, "from", ReasonNotAligned}}},
		{"an end that is the beginning", "summary", query("from", hour10, "to", hour10), []rejection{{inQuery, "to", ReasonNotAfterFrom}}},
		{"an end before the beginning", "summary", query("from", hour11, "to", hour10), []rejection{{inQuery, "to", ReasonNotAfterFrom}}},
		{"398 days", "summary", query("from", "2025-10-17T00:00:00Z", "to", "2026-11-19T00:00:00Z"), []rejection{{inQuery, "to", ReasonPeriodTooLong}}},

		{"a series of nothing in particular", "series", query("from", hour10, "to", hour11), []rejection{{inQuery, "granularity", ReasonRequired}}},
		{"a series by the week", "series", query("from", hour10, "to", hour11, "granularity", "week"), []rejection{{inQuery, "granularity", ReasonInvalid}}},
		{"days that begin at ten", "series", query("from", hour10, "to", day20, "granularity", "day"), []rejection{{inQuery, "from", ReasonNotAligned}}},
		{"days that end at ten", "series", query("from", day19, "to", "2026-11-20T10:00:00Z", "granularity", "day"), []rejection{{inQuery, "to", ReasonNotAligned}}},
		{"745 hours", "series", query("from", "2026-10-19T00:00:00Z", "to", "2026-11-19T01:00:00Z", "granularity", "hour"), []rejection{{inQuery, "to", ReasonPeriodTooLong}}},
		{"398 days by the day", "series", query("from", "2025-10-17T00:00:00Z", "to", "2026-11-19T00:00:00Z", "granularity", "day"), []rejection{{inQuery, "to", ReasonPeriodTooLong}}},

		{"a breakdown by nothing", "breakdown", query("from", day19, "to", day20), []rejection{{inQuery, "dimension", ReasonRequired}}},
		{"a breakdown by country", "breakdown", query("from", day19, "to", day20, "dimension", "country"), []rejection{{inQuery, "dimension", ReasonInvalid}}},
		{"a breakdown of hours", "breakdown", query("from", hour10, "to", hour11, "dimension", "device"), []rejection{{inQuery, "from", ReasonNotAligned}}},
		{"no value at all", "breakdown", query("from", day19, "to", day20, "dimension", "device", "limit", "0"), []rejection{{inQuery, "limit", ReasonInvalid}}},
		{"101 values", "breakdown", query("from", day19, "to", day20, "dimension", "device", "limit", "101"), []rejection{{inQuery, "limit", ReasonInvalid}}},
		{"a limit that is not a number", "breakdown", query("from", day19, "to", day20, "dimension", "device", "limit", "many"), []rejection{{inQuery, "limit", ReasonInvalid}}},
	} {
		response, body := s.do(t, key, http.MethodGet, "/v1/links/"+target.ID+"/metrics/"+c.operation+"?"+c.query, nil)
		if response.status != http.StatusBadRequest || code(t, body) != CodeInvalidRequest || !slices.Equal(rejected(t, body), c.want) {
			t.Errorf("%s = %d %s, want 400 with %v", c.name, response.status, body, c.want)
		}
		// Nothing of what was sent comes back.
		if strings.Contains(string(body), "yesterday") || strings.Contains(string(body), "country") || strings.Contains(string(body), "many") {
			t.Errorf("%s: the error repeats what was sent: %s", c.name, body)
		}
	}

	// At the bounds, a period is one: 397 days, 744 hours, 100 values.
	for name, path := range map[string]string{
		"397 days":            "summary?" + query("from", "2025-10-18T00:00:00Z", "to", "2026-11-19T00:00:00Z"),
		"397 days by the day": "series?" + query("from", "2025-10-18T00:00:00Z", "to", "2026-11-19T00:00:00Z", "granularity", "day"),
		"744 hours":           "series?" + query("from", "2026-10-19T00:00:00Z", "to", "2026-11-19T00:00:00Z", "granularity", "hour"),
		"100 values":          "breakdown?" + query("from", day19, "to", day20, "dimension", "referrer_host", "limit", "100"),
	} {
		if response, body := s.do(t, key, http.MethodGet, "/v1/links/"+target.ID+"/metrics/"+path, nil); response.status != http.StatusOK {
			t.Errorf("%s = %d %s, want 200", name, response.status, body)
		}
	}
}
