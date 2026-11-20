//go:build integration

package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/postgres"
)

// elsewhere returns a store whose sessions in the database are in a time
// zone: the days of a reconciliation are the ones of UTC wherever the
// database believes it is.
func (s *stage) elsewhere(t *testing.T, zone string) *Store {
	t.Helper()
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["timezone"] = zone
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool, slog.New(slog.DiscardHandler))
}

func TestTheReconciliationOfAWorkspaceIsItsLastDaysOneByOne(t *testing.T) {
	s := newStage(t)
	var today time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&today); err != nil {
		t.Fatal(err)
	}
	// ago returns a moment of the day that was so many days ago.
	ago := func(days int, hour, minute, second int) time.Time {
		return today.AddDate(0, 0, -days).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute + time.Duration(second)*time.Second)
	}
	accept := func(events ...Event) []Event {
		t.Helper()
		if settled, err := s.store.Accept(t.Context(), events); err != nil || settled != len(events) {
			t.Fatalf("Accept settled %d events with the error %v", settled, err)
		}
		return events
	}
	served := func(workspace string, at time.Time, n int64) {
		t.Helper()
		if settled, err := s.store.Count(t.Context(), []Count{{Day: at, WorkspaceID: workspace, Name: counterServed, Value: n}}); err != nil || settled != 1 {
			t.Fatalf("Count settled %d counts with the error %v", settled, err)
		}
	}
	process := func(events ...Event) {
		t.Helper()
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if err := MarkProcessed(t.Context(), tx, events); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	deadLetter := func(event Event) {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET attempts = 4 WHERE id = $1::uuid`, event.ID); err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if dead, err := Fail(t.Context(), tx, event, postgres.Refusal{Code: "23514", Constraint: "a_test"}); err != nil || !dead {
			t.Fatalf("Fail = %v, %v; want the event among the dead letters", dead, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	// Today: five responses, of which one was lost, two are in the figures,
	// one waits and one was set aside. The first second of the day is of
	// today.
	served(s.acme, ago(0, 0, 0, 0), 5)
	now := accept(s.event(ago(0, 0, 0, 0)), s.event(ago(0, 0, 0, 1)), s.event(ago(0, 0, 0, 2)), s.event(ago(0, 0, 0, 3)))
	process(now[0], now[1])
	deadLetter(now[3])
	// Yesterday: three, all in the figures. The last second of the day is
	// of yesterday.
	served(s.acme, ago(1, 23, 59, 59), 3)
	process(accept(s.event(ago(1, 23, 59, 59)), s.event(ago(1, 12, 0, 0)), s.event(ago(1, 0, 0, 0)))...)
	// Three days ago: two events whose count of served responses the
	// database refused. Nothing was lost, and nothing is shown below zero.
	accept(s.event(ago(3, 12, 0, 0)), s.event(ago(3, 12, 0, 1)))
	// Six days ago, the first day of a week that ends today: two responses
	// and one event, which waits.
	served(s.acme, ago(6, 12, 0, 0), 2)
	accept(s.event(ago(6, 12, 0, 0)))
	// Seven days ago: outside that week.
	served(s.acme, ago(7, 23, 59, 59), 9)
	accept(s.events(ago(7, 23, 59, 59), 9)...)
	// Another workspace, today.
	served(s.globex, ago(0, 12, 0, 0), 40)
	theirs := s.events(ago(0, 12, 0, 0), 40)
	for i := range theirs {
		theirs[i].LinkID, theirs[i].WorkspaceID = s.globexLink, s.globex
	}
	accept(theirs...)

	day := func(days int, served, accepted, lost, processed, pending, deadLettered int64) Day {
		return Day{Day: ago(days, 0, 0, 0), Served: served, Accepted: accepted, Lost: lost, Processed: processed, Pending: pending, DeadLettered: deadLettered}
	}
	week := []Day{
		day(6, 2, 1, 1, 0, 1, 0), day(5, 0, 0, 0, 0, 0, 0), day(4, 0, 0, 0, 0, 0, 0), day(3, 0, 2, 0, 0, 2, 0),
		day(2, 0, 0, 0, 0, 0, 0), day(1, 3, 3, 0, 3, 0, 0), day(0, 5, 4, 1, 2, 1, 1),
	}
	same := func(got, want []Day) bool {
		return slices.EqualFunc(got, want, func(a, b Day) bool {
			return a.Day.Equal(b.Day) && a.Day.Location() == time.UTC && a.Served == b.Served && a.Accepted == b.Accepted &&
				a.Lost == b.Lost && a.Processed == b.Processed && a.Pending == b.Pending && a.DeadLettered == b.DeadLettered
		})
	}
	// Twelve hours behind UTC and fourteen ahead: at every hour of the day
	// one of the two is on another date than UTC.
	for name, store := range map[string]*Store{
		"in UTC": s.store, "behind UTC": s.elsewhere(t, "Etc/GMT+12"), "ahead of UTC": s.elsewhere(t, "Pacific/Kiritimati"),
	} {
		for _, c := range []struct {
			name string
			of   string
			days int
			want []Day
		}{
			{"a week", s.acme, DefaultDays, week},
			{"today", s.acme, 1, week[6:]},
			{"two days", s.acme, 2, week[5:]},
			{"eight days", s.acme, 8, append([]Day{day(7, 9, 9, 0, 0, 9, 0)}, week...)},
			{"the other workspace", s.globex, 2, []Day{day(1, 0, 0, 0, 0, 0, 0), day(0, 40, 40, 0, 0, 40, 0)}},
		} {
			got, err := store.Reconciliation(t.Context(), c.of, c.days)
			if err != nil || !same(got, c.want) {
				t.Errorf("%s, %s: Reconciliation = %+v, %v; want %+v", name, c.name, got, err, c.want)
			}
			// The second equality, in every day of every answer.
			for _, terms := range got {
				if terms.Accepted != terms.Processed+terms.Pending+terms.DeadLettered {
					t.Errorf("%s, %s: on %s, %d accepted are not %d processed, %d pending and %d dead-lettered",
						name, c.name, terms.Day.Format(time.DateOnly), terms.Accepted, terms.Processed, terms.Pending, terms.DeadLettered)
				}
			}
		}
		// As many days as there can be, the oldest first and today the last.
		longest, err := store.Reconciliation(t.Context(), s.acme, MaxDays)
		if err != nil || len(longest) != MaxDays || !longest[0].Day.Equal(ago(MaxDays-1, 0, 0, 0)) || !same(longest[MaxDays-7:], week) {
			t.Errorf("%s: the longest reconciliation has %d days (%v), want %d that end with the week", name, len(longest), err, MaxDays)
		}
	}
	// The events that wait are counted, not worked out from the counts: a
	// defect that loses one shows as a day that does not add up.
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM event_inbox WHERE id = $1::uuid`, now[2].ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.store.Reconciliation(t.Context(), s.acme, 1); err != nil || !same(got, []Day{day(0, 5, 4, 1, 2, 0, 1)}) {
		t.Errorf("with an event that waited gone, Reconciliation = %+v, %v; want 4 accepted, 2 processed, none pending and 1 dead-lettered", got, err)
	}

	for _, days := range []int{-1, 0, MaxDays + 1} {
		if got, err := s.store.Reconciliation(t.Context(), s.acme, days); !errors.Is(err, ErrDays) || got != nil {
			t.Errorf("a reconciliation of %d days = %v, %v; want %v", days, got, err, ErrDays)
		}
	}
	if DefaultDays != 7 || MaxDays != 31 {
		t.Errorf("a reconciliation covers %d days, and %d at the most; PLAN.md section 7 says 7 and 31", DefaultDays, MaxDays)
	}
}
