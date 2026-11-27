//go:build integration

package panel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/rollup"
)

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

// consume adds every event that waits to the aggregates, as the worker
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

// moment returns the one moment a statement gives.
func (s *stage) moment(t *testing.T, statement string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(t.Context(), statement).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

// day returns the day it is in UTC by the clock of the database, which is
// the one that dates the terms of a reconciliation. A test that asked just
// before midnight would find its days moved under it: it waits for the
// day to change first.
func (s *stage) day(t *testing.T) time.Time {
	t.Helper()
	var left float64
	if err := s.pool.QueryRow(t.Context(),
		`SELECT extract(epoch FROM date_trunc('day', now() AT TIME ZONE 'UTC') + interval '1 day' - (now() AT TIME ZONE 'UTC'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left < 30 {
		time.Sleep(time.Duration(left+1) * time.Second)
	}
	return s.moment(t, `SELECT (now() AT TIME ZONE 'UTC')::date::timestamp AT TIME ZONE 'UTC'`)
}

// The status page: what became of the events of the workspace, day by day,
// with what the days add up to, and of no other workspace.
func TestTheStatusPageShowsWhatBecameOfTheEventsOfTheWorkspace(t *testing.T) {
	s := newStage(t)
	acme, inside := s.shop(t, link.WorkspaceDraft{})
	globex := s.workspace(t, false)
	target, other := s.link(t, acme, link.Draft{}), s.link(t, globex, link.Draft{})
	today := s.day(t)
	noon := func(daysAgo int) time.Time { return today.AddDate(0, 0, -daysAgo).Add(12 * time.Hour) }

	// Acme: two accesses yesterday and two today, all of them counted by
	// the worker, and one more today that still waits. Eight days ago, one
	// more. Two days ago an event was set aside. Two responses of today
	// left no event. Globex: one access today, counted.
	s.serve(t, visit(target, noon(0)), visit(target, noon(0), unserved), visit(target, noon(1)), visit(target, noon(1)),
		visit(target, noon(8)), visit(other, noon(0)))
	s.consume(t)
	s.serve(t, visit(target, noon(0)))
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.SetAsideDemoEvent(t.Context(), tx, visit(target, noon(2)), postgres.Refusal{Code: "23514", Constraint: "a_test"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if settled, err := ingest.NewStore(s.pool, quiet).Count(t.Context(), []ingest.Count{{Day: today, WorkspaceID: acme, Name: "served", Value: 2}}); err != nil || settled != 1 {
		t.Fatalf("Count settled %d counts with the error %v", settled, err)
	}
	waiting := s.moment(t, `SELECT accepted_at FROM event_inbox WHERE processed_at IS NULL`)

	// terms is what a day or a period came to: served, accepted, lost,
	// processed, pending and dead-lettered.
	type terms [6]int64
	cells := func(lang language, of terms) string {
		written := ""
		for _, n := range of {
			written += `<td class="count">` + s.texts.number(lang, n) + "</td>"
		}
		return written
	}
	for _, c := range []struct {
		days  int
		query string
		// What each of the days with something came to, by how long ago
		// it was, and what the period adds up to.
		of    map[int]terms
		total terms
	}{
		{days: 7, query: "", of: map[int]terms{0: {5, 3, 2, 2, 1, 0}, 1: {2, 2, 0, 2, 0, 0}, 2: {1, 1, 0, 0, 0, 1}}, total: terms{8, 6, 2, 4, 1, 1}},
		{days: 31, query: "?days=31", of: map[int]terms{0: {5, 3, 2, 2, 1, 0}, 1: {2, 2, 0, 2, 0, 0}, 2: {1, 1, 0, 0, 0, 1}, 8: {1, 1, 0, 1, 0, 0}}, total: terms{9, 7, 2, 5, 1, 1}},
	} {
		for _, lang := range languages {
			otherLang := spanish
			if lang == spanish {
				otherLang = english
			}
			write := func(text string, err error) string {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				return escaped(text)
			}
			here := "/" + string(lang) + "/status"
			answer := s.ask(t, http.MethodGet, here+c.query, with(inside))
			page := answer.Body.String()
			s.shows(t, "the status of "+strconv.Itoa(c.days)+" days", answer, http.StatusOK, lang, "status.title")

			var rows []string
			for ago := c.days - 1; ago >= 0; ago-- {
				day := today.AddDate(0, 0, -ago)
				rows = append(rows, `<tr><th scope="row" class="day"><time datetime="`+day.Format(time.DateOnly)+`">`+write(s.texts.date(lang, day))+
					"</time></th>"+cells(lang, c.of[ago])+"</tr>")
			}
			seven, thirtyOne := "", ` aria-current="page"`
			if c.days == 7 {
				seven, thirtyOne = thirtyOne, seven
			}
			for what, want := range map[string]string{
				"its title":             "<title>" + s.text(t, lang, "status.page_title", "days", c.days) + " · Corinto</title>",
				"that it is synthetic":  `<p class="notice">` + s.text(t, lang, "status.synthetic") + "</p>",
				"what the page is":      "<p>" + s.text(t, lang, "status.intro") + "</p>",
				"the period":            `<h2 id="period">` + s.text(t, lang, "link.period", "days", c.days, "first", write(s.texts.date(lang, today.AddDate(0, 0, 1-c.days))), "last", write(s.texts.date(lang, today))) + "</h2>",
				"its two periods":       `<li><a href="` + here + `"` + seven + ">" + write(s.texts.plural(lang, "period.days", 7)) + "</a></li>\n" + `<li><a href="` + here + `?days=31"` + thirtyOne + ">" + write(s.texts.plural(lang, "period.days", 31)) + "</a></li>",
				"what the days came to": s.text(t, lang, "status.summary", "days", c.days, "served", write(s.texts.plural(lang, "count.served", c.total[0])), "accepted", write(s.texts.plural(lang, "count.accepted", c.total[1])), "lost", write(s.texts.plural(lang, "count.lost", c.total[2]))),
				"that events were lost": "<p>" + s.text(t, lang, "status.something_lost") + "</p>",
				"a row for each day":    "<tbody>\n" + strings.Join(rows, "\n") + "\n</tbody>",
				"what the days add to":  `<tfoot>` + "\n" + `<tr><th scope="row">` + s.text(t, lang, "status.total") + "</th>" + cells(lang, c.total) + "</tr>\n</tfoot>",
				"what lost is":          `<p class="definition">` + s.text(t, lang, "status.definition.lost") + "</p>",
				"what accepted is":      `<p class="definition">` + s.text(t, lang, "status.definition.accepted", "attempts", ingest.MaxAttempts) + "</p>",
				"how far it has got":    "<p>" + s.text(t, lang, "status.through", "moment", write(s.texts.moment(lang, waiting))) + "</p>",
				"the retention":         "<p>" + s.text(t, lang, "status.retention_never") + "</p>",
				"the other language":    `<a href="/` + string(otherLang) + "/status" + c.query + `" hreflang="` + string(otherLang) + `"`,
			} {
				if !strings.Contains(page, want) {
					t.Errorf("the status of %d days in %q does not show %s: no %q in it", c.days, lang, what, want)
				}
			}
			if strings.Count(page, "aria-current=") != 1 || strings.Contains(page, s.text(t, lang, "status.nothing_lost")) || strings.Contains(page, s.text(t, lang, "status.empty", "days", c.days)) {
				t.Errorf("the status of %d days in %q marks more than one period, or says that nothing was lost or asked for", c.days, lang)
			}
		}
	}

	// The retention job ran yesterday and is running now: it last finished
	// yesterday. And the events that wait are counted: none waits.
	finished := s.moment(t, `WITH runs AS (
	    INSERT INTO retention_runs (started_at, finished_at)
	    VALUES (now() - interval '1 day', now() - interval '1 day' + interval '2 seconds'), (now(), NULL)
	    RETURNING finished_at)
	 SELECT max(finished_at) FROM runs`)
	s.consume(t)
	page := s.ask(t, http.MethodGet, "/en/status", with(inside)).Body.String()
	moment, err := s.texts.moment(english, finished)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "<p>"+s.text(t, english, "status.retention", "moment", moment)+"</p>") || strings.Contains(page, s.text(t, english, "status.retention_never")) {
		t.Errorf("the status does not say that the retention last finished at %s", moment)
	}
	if want := `<tfoot>` + "\n" + `<tr><th scope="row">` + s.text(t, english, "status.total") + "</th>" + cells(english, terms{8, 6, 2, 5, 0, 1}) + "</tr>"; !strings.Contains(page, want) {
		t.Errorf("the status, once every event is counted, does not add up to %q", want)
	}

	// Globex sees its own events: one response, one event, nothing lost.
	// It is not a demo workspace, and its page does not say it is.
	theirs := s.get(t, globex, "/en/status").Body.String()
	served, _ := s.texts.plural(english, "count.served", 1)
	accepted, _ := s.texts.plural(english, "count.accepted", 1)
	lost, _ := s.texts.plural(english, "count.lost", 0)
	for what, want := range map[string]string{
		"what its days came to": s.text(t, english, "status.summary", "days", 7, "served", served, "accepted", accepted, "lost", lost),
		"that nothing was lost": "<p>" + s.text(t, english, "status.nothing_lost") + "</p>",
		"what its days add to":  `<tr><th scope="row">` + s.text(t, english, "status.total") + "</th>" + cells(english, terms{1, 1, 0, 1, 0, 0}) + "</tr>",
	} {
		if !strings.Contains(theirs, want) {
			t.Errorf("the status of the other workspace does not show %s: no %q in it", what, want)
		}
	}
	if strings.Contains(theirs, s.text(t, english, "status.synthetic")) || strings.Contains(theirs, s.text(t, english, "status.something_lost")) {
		t.Error("the status of a workspace that is not a demo says that it is one, or that events were lost")
	}

	// A workspace nobody asked a link of says so, in a table of zeros.
	empty, inEmpty := s.shop(t, link.WorkspaceDraft{Name: "Initech"})
	s.link(t, empty, link.Draft{})
	for _, lang := range languages {
		quiet := s.ask(t, http.MethodGet, "/"+string(lang)+"/status?days=31", with(inEmpty)).Body.String()
		if !strings.Contains(quiet, "<p>"+s.text(t, lang, "status.empty", "days", 31)+"</p>") || strings.Contains(quiet, s.text(t, lang, "status.nothing_lost")) ||
			strings.Count(quiet, "<time datetime=") != 31 || !strings.Contains(quiet, cells(lang, terms{})+"</tr>\n</tfoot>") {
			t.Errorf("the status in %q of a workspace nobody asked a link of does not say so over 31 days of zeros", lang)
		}
	}
}

// The status page is behind a session, and has the two periods a
// reconciliation is asked for: a week, which it shows unasked, and 31 days.
func TestTheStatusPageIsBehindASessionAndHasTwoPeriods(t *testing.T) {
	s := newStage(t)
	_, inside := s.shop(t, link.WorkspaceDraft{})
	s.day(t)

	sentTo(t, "the status without a session", s.ask(t, http.MethodGet, "/en/status"), "/en/")
	plain, named := s.ask(t, http.MethodGet, "/en/status", with(inside)), s.ask(t, http.MethodGet, "/en/status?days=7", with(inside))
	if named.Code != http.StatusOK || settled(named.Body.String()) != settled(plain.Body.String()) || strings.Count(plain.Body.String(), "<time datetime=") != 7 {
		t.Errorf("the status asked for seven days = %d, and it is not the page asked for no period, with its seven days", named.Code)
	}
	for _, query := range []string{"?days=1", "?days=30", "?days=32", "?days=90", "?days=07", "?days=", "?days", "?days=7&days=31", "?days=week"} {
		s.shows(t, "the status with "+query, s.ask(t, http.MethodGet, "/en/status"+query, with(inside)), http.StatusNotFound, english, "notfound.title")
	}
	// Nothing is sent to it, and it has no page under it.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if answer := s.ask(t, method, "/en/status", with(inside)); answer.Code != http.StatusNotFound {
			t.Errorf("%s the status = %d, want %d", method, answer.Code, http.StatusNotFound)
		}
	}
	s.shows(t, "a page under the status", s.ask(t, http.MethodGet, "/en/status/dead-letters", with(inside)), http.StatusNotFound, english, "notfound.title")
	if statusPeriods[0] != ingest.DefaultDays || statusPeriods[1] != ingest.MaxDays || len(statusPeriods) != 2 || ingest.DefaultDays != 7 || ingest.MaxDays != 31 {
		t.Errorf("the periods of the status are %v, want the %d days a reconciliation covers unasked and the %d it covers at the most", statusPeriods, ingest.DefaultDays, ingest.MaxDays)
	}
}
