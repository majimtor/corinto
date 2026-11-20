//go:build integration

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// status is what the ingestion status answers.
type status struct {
	ProcessedThrough time.Time `json:"processed_through"`
	Retention        struct {
		LastFinishedAt *time.Time `json:"last_finished_at"`
	} `json:"retention"`
	Days []shownDay `json:"days"`
}

// ingestion asks for the status of the ingestion, which must answer.
func (s *stage) ingestion(t *testing.T, credential, query string) (status, []byte) {
	t.Helper()
	response, body := s.do(t, credential, http.MethodGet, "/v1/ingestion"+query, nil)
	if response.status != http.StatusOK {
		t.Fatalf("GET /v1/ingestion%s = %d %s", query, response.status, body)
	}
	return read[status](t, body), body
}

// serve hands events to a buffer as the redirect does, and waits until the
// buffer has written them with the count of the responses that owed them.
func (s *stage) serve(t *testing.T, events ...ingest.Event) {
	t.Helper()
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	buffer := ingest.NewBuffer(ingest.NewStore(s.pool, quiet), quiet)
	for _, event := range events {
		buffer.Record(event)
	}
	// A buffer that is told to stop writes what it holds first.
	stopped, stop := context.WithCancel(t.Context())
	stop()
	buffer.Run(stopped)
}

// today returns the day it is in UTC, by the clock of the database.
func (s *stage) today(t *testing.T) time.Time {
	t.Helper()
	return s.moment(t, `SELECT (now() AT TIME ZONE 'UTC')::date::timestamp AT TIME ZONE 'UTC'`)
}

func TestTheIngestionStatusIsOfTheWorkspaceAndOfItsLastDays(t *testing.T) {
	s := newStage(t)
	acme, globex := s.workspace(t, link.WorkspaceDraft{}), s.workspace(t, link.WorkspaceDraft{Name: "Globex"})
	key, theirs := s.key(t, acme, auth.ScopeMetricsRead), s.key(t, globex, auth.ScopeMetricsRead)
	target, other := s.newTarget(t, acme), s.newTarget(t, globex)
	today := s.today(t)
	noon := func(daysAgo int) time.Time { return today.AddDate(0, 0, -daysAgo).Add(12 * time.Hour) }

	// Acme: three accesses today, of which the worker has counted two, and
	// two yesterday. Eight days ago, one more. Globex: one today, counted.
	s.serve(t, visit(target, noon(0)), visit(target, noon(0), unserved), visit(target, noon(1)), visit(target, noon(1)),
		visit(target, noon(8)), visit(other, noon(0)))
	s.consume(t)
	s.serve(t, visit(target, noon(0)))

	day := func(daysAgo int, served, accepted, processed, pending int64) shownDay {
		return shownDay{
			Day: today.AddDate(0, 0, -daysAgo).Format(time.DateOnly), Served: served, Accepted: accepted,
			Processed: processed, Pending: pending,
		}
	}
	week := []shownDay{
		day(6, 0, 0, 0, 0), day(5, 0, 0, 0, 0), day(4, 0, 0, 0, 0), day(3, 0, 0, 0, 0), day(2, 0, 0, 0, 0),
		day(1, 2, 2, 2, 0), day(0, 3, 3, 2, 1),
	}
	got, body := s.ingestion(t, key, "")
	if !slices.Equal(got.Days, week) {
		t.Errorf("the status of Acme is %s; want the days %+v", body, week)
	}
	// How far the events are processed is of the whole service: the event
	// of Acme that waits holds the moment back for Globex too.
	oldest := s.moment(t, `SELECT accepted_at FROM event_inbox WHERE processed_at IS NULL`)
	if !got.ProcessedThrough.Equal(oldest) || got.ProcessedThrough.Location() != time.UTC {
		t.Errorf("the events are processed through %s, want %s, the acceptance of the one that waits", got.ProcessedThrough, oldest)
	}
	if got.Retention.LastFinishedAt != nil {
		t.Errorf("the retention last finished at %s, and it never ran", got.Retention.LastFinishedAt)
	}

	for _, c := range []struct {
		name  string
		key   string
		query string
		want  []shownDay
	}{
		{"today", key, "?days=1", week[6:]},
		{"two days", key, "?days=2", week[5:]},
		{"nine days", key, "?days=9", append([]shownDay{day(8, 1, 1, 1, 0), day(7, 0, 0, 0, 0)}, week...)},
		{"the other workspace", theirs, "?days=2", []shownDay{day(1, 0, 0, 0, 0), day(0, 1, 1, 1, 0)}},
	} {
		if got, body := s.ingestion(t, c.key, c.query); !slices.Equal(got.Days, c.want) {
			t.Errorf("%s: %s; want the days %+v", c.name, body, c.want)
		}
	}
	if theirStatus, _ := s.ingestion(t, theirs, ""); !theirStatus.ProcessedThrough.Equal(oldest) {
		t.Errorf("for the other workspace the events are processed through %s, want %s", theirStatus.ProcessedThrough, oldest)
	}
	if longest, _ := s.ingestion(t, key, "?days=31"); len(longest.Days) != 31 || longest.Days[30] != week[6] ||
		longest.Days[0].Day != today.AddDate(0, 0, -30).Format(time.DateOnly) {
		t.Errorf("31 days are %d, from %s", len(longest.Days), longest.Days[0].Day)
	}

	// The retention job ran yesterday and is running now: it last finished
	// yesterday.
	finished := s.moment(t, `WITH runs AS (
	    INSERT INTO retention_runs (started_at, finished_at)
	    VALUES (now() - interval '1 day', now() - interval '1 day' + interval '2 seconds'), (now(), NULL)
	    RETURNING finished_at)
	 SELECT max(finished_at) FROM runs`)
	if got, body := s.ingestion(t, key, "?days=1"); got.Retention.LastFinishedAt == nil || !got.Retention.LastFinishedAt.Equal(finished) {
		t.Errorf("the status says %s; want the retention to have last finished at %s", body, finished)
	}

	// Two responses of today whose events never reached the inbox, and an
	// event set aside: each is in the term that is its own.
	if _, err := s.pool.Exec(t.Context(),
		`INSERT INTO pipeline_counters (day, workspace_id, name, value)
		 VALUES ((now() AT TIME ZONE 'UTC')::date, $1::uuid, 'served', 2), ((now() AT TIME ZONE 'UTC')::date, $1::uuid, 'dead_lettered', 1)
		 ON CONFLICT ON CONSTRAINT pipeline_counters_key DO UPDATE SET value = pipeline_counters.value + EXCLUDED.value`, acme); err != nil {
		t.Fatal(err)
	}
	want := shownDay{Day: today.Format(time.DateOnly), Served: 5, Accepted: 3, Lost: 2, Processed: 2, Pending: 1, DeadLettered: 1}
	if got, body := s.ingestion(t, key, "?days=1"); len(got.Days) != 1 || got.Days[0] != want {
		t.Errorf("the status says %s; want the day %+v", body, want)
	}

	for _, days := range []string{"0", "32", "-1", "many", "1.5"} {
		response, body := s.do(t, key, http.MethodGet, "/v1/ingestion?days="+days, nil)
		if want := []rejection{{inQuery, "days", ReasonInvalid}}; response.status != http.StatusBadRequest || !slices.Equal(rejected(t, body), want) {
			t.Errorf("days=%s = %d %s, want 400 with %v", days, response.status, body, want)
		}
	}
}

// The reconciliation of docs/delivery-semantics.md, as the operation
// returns it: the terms are read at one moment, so what was accepted is
// what was processed, what waits and what was set aside, added up, in
// every answer, also while the worker is processing.
func TestTheIngestionStatusAddsUpWhileEventsAreProcessed(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	key := s.key(t, owner, auth.ScopeMetricsRead)
	target := s.newTarget(t, owner)
	const events = 20 * rollup.BatchSize
	now := s.today(t).Add(12 * time.Hour)
	for range events / rollup.BatchSize {
		batch := make([]ingest.Event, rollup.BatchSize)
		for i := range batch {
			batch[i] = visit(target, now)
		}
		s.accept(t, batch...)
	}

	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		s.consume(t)
	}()
	// A key makes 120 requests a minute: fewer than that, a few
	// milliseconds apart, while the worker goes through its batches.
	between := 0
	for asked := 0; asked < 100; asked++ {
		got, body := s.ingestion(t, key, "?days="+strconv.Itoa(2))
		var accepted, processed, pending, deadLettered int64
		for _, day := range got.Days {
			accepted, processed = accepted+day.Accepted, processed+day.Processed
			pending, deadLettered = pending+day.Pending, deadLettered+day.DeadLettered
			if day.Accepted != day.Processed+day.Pending+day.DeadLettered {
				t.Fatalf("on %s, %d accepted are not %d processed, %d pending and %d dead-lettered: %s",
					day.Day, day.Accepted, day.Processed, day.Pending, day.DeadLettered, body)
			}
		}
		if accepted != events || deadLettered != 0 {
			t.Fatalf("%d events are accepted and %d dead-lettered, want %d and none: %s", accepted, deadLettered, events, body)
		}
		if processed > 0 && pending > 0 {
			between++
		}
		if processed == events {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-consumed
	if between == 0 {
		t.Error("no answer was given while the events were being processed: the test saw only the beginning and the end")
	}
	if got, body := s.ingestion(t, key, "?days=2"); got.Days[0].Processed+got.Days[1].Processed != events || got.Days[0].Pending+got.Days[1].Pending != 0 {
		t.Errorf("once the worker is done the status is %s; want %d processed and nothing pending", body, events)
	}
}
