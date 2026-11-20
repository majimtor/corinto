//go:build integration

package rollup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
)

// The versions of the rules that are recorded, each with its two hours.
const versionRows = `SELECT concat_ws(' ', version, to_char(first_hour AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI'),
                                    to_char(last_hour AT TIME ZONE 'UTC', 'HH24:MI DD')), 1
                       FROM rule_versions`

// under returns the change that makes an event one that a version of the
// rules classified.
func under(version int) func(*ingest.Event) {
	return func(e *ingest.Event) { e.Classification.Version = version }
}

func TestTheVersionsOfTheRulesAreRecordedWithTheHoursTheyClassifiedBetween(t *testing.T) {
	west(t)
	s := newStage(t)
	s.accept(t,
		// The last second of an hour and the first of another, neither of
		// them the first event of the batch nor the last.
		with(click(s.spring, s.acme, at(11, 15, 0)), under(1)),
		with(click(s.spring, s.acme, at(12, 0, 0)), under(1)),
		with(click(s.spring, s.acme, at(10, 59, 59)), under(1)),
		with(click(s.spring, s.acme, at(11, 20, 0)), under(1)),
		// Another version began while the first was in use, in another
		// workspace, and reached the next day. What it classified there was
		// no click: every event that is counted says its version.
		with(click(s.autumn, s.globex, at(11, 30, 0)), under(2)),
		with(click(s.autumn, s.globex, at(24, 0, 0)), func(e *ingest.Event) {
			e.Classification.Version, e.Classification.Class, e.Outcome = 2, classify.ClassCrawler, ingest.OutcomeInactive
		}),
		with(click(s.autumn, s.globex, at(11, 40, 0)), under(2)),
	)
	if processed := s.batch(t); processed != 7 {
		t.Fatalf("Batch processed %d events, want 7", processed)
	}
	const second = "2 2026-11-19 11:00 00:00 20"
	compare(t, "the versions of the rules", s.table(t, versionRows), map[string]int64{
		"1 2026-11-19 10:00 12:00 19": 1, second: 1,
	})

	// An event before the first hour of its version moves it, and one
	// after the last moves that one. The other versions stay as they were.
	for _, step := range []struct {
		name  string
		event ingest.Event
		want  map[string]int64
	}{
		{"an event within the hours", with(click(s.spring, s.acme, at(11, 0, 0)), under(1)),
			map[string]int64{"1 2026-11-19 10:00 12:00 19": 1, second: 1}},
		{"an event of the hour before", with(click(s.summer, s.acme, at(9, 59, 59)), under(1)),
			map[string]int64{"1 2026-11-19 09:00 12:00 19": 1, second: 1}},
		{"an event of the hour after", with(click(s.summer, s.acme, at(13, 0, 0)), under(1)),
			map[string]int64{"1 2026-11-19 09:00 13:00 19": 1, second: 1}},
		{"an event of the day before", with(click(s.spring, s.acme, at(-1, 30, 0)), under(1)),
			map[string]int64{"1 2026-11-18 23:00 13:00 19": 1, second: 1}},
		// A version is new whatever the hours of the others.
		{"an event of a new version within the hours of the others", with(click(s.spring, s.acme, at(11, 0, 0)), under(3)),
			map[string]int64{"1 2026-11-18 23:00 13:00 19": 1, second: 1, "3 2026-11-19 11:00 11:00 19": 1}},
	} {
		s.accept(t, step.event)
		if processed := s.batch(t); processed != 1 {
			t.Fatalf("%s: Batch processed %d events, want it", step.name, processed)
		}
		compare(t, "the versions of the rules after "+step.name, s.table(t, versionRows), step.want)
	}
	s.agrees(t, 12)
}

// The row of a version is one for every consumer. It is written only when
// a batch reaches an hour that it does not cover: a consumer whose events
// are within its hours does not need the row, and does not wait for
// whoever holds it.
func TestAConsumerWithinTheHoursOfAVersionDoesNotWaitForItsRow(t *testing.T) {
	s := newStage(t)
	s.accept(t, click(s.spring, s.acme, at(10, 0, 0)), click(s.spring, s.acme, at(12, 0, 0)))
	if processed := s.batch(t); processed != 2 {
		t.Fatalf("Batch processed %d events, want 2", processed)
	}

	holder, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := holder.Exec(t.Context(), `SELECT FROM rule_versions WHERE version = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	// The first hour, the last, and one between them.
	s.accept(t, click(s.spring, s.acme, at(10, 59, 59)), click(s.spring, s.acme, at(11, 30, 0)), click(s.spring, s.acme, at(12, 0, 0)))
	quick, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if processed, err := s.consumer.Batch(quick); err != nil || processed != 3 {
		t.Fatalf("with the row of the version held, Batch processed %d events with the error %v; want the 3 that are within its hours", processed, err)
	}

	// An event of another hour needs the row, and waits for it. A wait
	// that was cut off is not a failure of the event.
	late := click(s.spring, s.acme, at(13, 0, 0))
	s.accept(t, late)
	short, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if processed, err := s.consumer.Batch(short); err == nil || processed != 0 {
		t.Fatalf("with the row of the version held, a batch that reaches another hour processed %d events with the error %v; want it to wait", processed, err)
	}
	if got := s.state(t, late.ID); !got.pending || got.attempts != 0 {
		t.Errorf("the event that waited is %+v, want pending with no failed attempt", got)
	}
	compare(t, "the versions of the rules", s.table(t, versionRows), map[string]int64{"1 2026-11-19 10:00 12:00 19": 1})

	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if processed := s.batch(t); processed != 1 {
		t.Fatalf("with the row free, Batch processed %d events, want the one that waited", processed)
	}
	compare(t, "the versions of the rules", s.table(t, versionRows), map[string]int64{"1 2026-11-19 10:00 13:00 19": 1})
	s.agrees(t, 6)
}

// Consumers that write the rows of the same versions write them in the
// order of the version, and so never wait for each other in a circle.
// Each consumer here has events of every version at an hour that no other
// has, later than any before: all the rows are the ones to write, for each
// of them, and they begin at the same moment.
func TestConsumersRecordTheVersionsOfTheRulesInOneOrder(t *testing.T) {
	s := newStage(t)
	const rounds, consumers, versions = 6, 8, 8
	// Every consumer holds a transaction while it waits for the others to
	// be ready: a connection for each, on a machine of any size.
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = consumers
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for round := range rounds {
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		for consumer := range consumers {
			events := make([]ingest.Event, versions)
			for i := range events {
				events[i] = with(click(s.spring, s.acme, day.Add(time.Duration(round*consumers+consumer)*time.Hour)), under(1+i))
			}
			ready.Add(1)
			done.Go(func() {
				tx, err := pool.Begin(t.Context())
				ready.Done()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
				<-start
				if err := recordVersions(t.Context(), tx, events); err != nil {
					t.Errorf("a consumer could not record the versions: %v", err)
					return
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Error(err)
				}
			})
		}
		ready.Wait()
		close(start)
		done.Wait()
	}
	// From the hour of the first consumer to the one of the last, 47 hours
	// later.
	want := make(map[string]int64)
	for version := range versions {
		want[fmt.Sprintf("%d 2026-11-19 00:00 23:00 20", 1+version)] = 1
	}
	compare(t, "the versions of the rules", s.table(t, versionRows), want)
}

// A version is recorded with the events that were counted, also when they
// were counted one by one. An event that the database refuses leaves
// nothing of its version until it is counted.
func TestTheVersionOfAnEventThatWasNotCountedIsNotRecorded(t *testing.T) {
	s := newStage(t)
	s.refusePreviews(t)
	// The version of the event that is refused is 3.
	bad := s.preview(at(14, 0, 0))
	s.accept(t, click(s.spring, s.acme, at(10, 0, 0)), bad)
	if processed := s.batch(t); processed != 1 {
		t.Fatalf("Batch processed %d events, want the one that the database takes", processed)
	}
	compare(t, "the versions of the rules", s.table(t, versionRows), map[string]int64{"1 2026-11-19 10:00 10:00 19": 1})

	s.acceptPreviews(t)
	s.due(t, bad.ID)
	if processed := s.batch(t); processed != 1 {
		t.Fatalf("with the defect corrected, Batch processed %d events, want the one that waited", processed)
	}
	compare(t, "the versions of the rules", s.table(t, versionRows), map[string]int64{
		"1 2026-11-19 10:00 10:00 19": 1, "3 2026-11-19 14:00 14:00 19": 1,
	})
	s.agrees(t, 2)
}

func TestTheVersionsOfTheRulesRefuseWhatTheCodeWouldNotWrite(t *testing.T) {
	s := newStage(t)
	// Asked where the clocks are five and a half hours ahead: an hour is
	// on the hour of UTC, wherever the question comes from.
	exec := func(statement string) error {
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if _, err := tx.Exec(t.Context(), `SET LOCAL TIME ZONE 'Asia/Kolkata'`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	insert := `INSERT INTO rule_versions (version, first_hour, last_hour) VALUES `
	for _, refused := range []struct {
		name, statement, constraint string
	}{
		{"a version that is not one", insert + `(0, '2026-11-19 10:00:00+00', '2026-11-19 10:00:00+00')`, "rule_versions_version_check"},
		{"a first hour that is not on the hour", insert + `(1, '2026-11-19 10:00:01+00', '2026-11-19 11:00:00+00')`, "rule_versions_first_hour_check"},
		{"a first hour that is on the hour somewhere else", insert + `(1, '2026-11-19 10:00:00+05:30', '2026-11-19 11:00:00+00')`, "rule_versions_first_hour_check"},
		{"a last hour that is not on the hour", insert + `(1, '2026-11-19 10:00:00+00', '2026-11-19 11:59:59+00')`, "rule_versions_last_hour_check"},
		{"a last hour that is on the hour somewhere else", insert + `(1, '2026-11-19 10:00:00+00', '2026-11-19 17:00:00+05:30')`, "rule_versions_last_hour_check"},
		{"a last hour before the first", insert + `(1, '2026-11-19 10:00:00+00', '2026-11-19 09:00:00+00')`, "rule_versions_hours"},
	} {
		err := exec(refused.statement)
		var answer *pgconn.PgError
		if !errors.As(err, &answer) || answer.Code != "23514" || answer.ConstraintName != refused.constraint {
			t.Errorf("%s: the insert ended with %v, want it refused by %s", refused.name, err, refused.constraint)
		}
	}
	// And it takes what the code writes: a version seen in one hour.
	if err := exec(insert + `(1, '2026-11-19 10:00:00+00', '2026-11-19 10:00:00+00')`); err != nil {
		t.Errorf("an insert the code could make was refused: %v", err)
	}
	// One row per version.
	err := exec(insert + `(1, '2026-11-19 12:00:00+00', '2026-11-19 12:00:00+00')`)
	var answer *pgconn.PgError
	if !errors.As(err, &answer) || answer.Code != "23505" {
		t.Errorf("a second row of a version ended with %v, want it refused", err)
	}
}
