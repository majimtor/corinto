//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// What the redirect serves reaches the figures of its link: three
// processes that share nothing but the database.
func TestWhatTheRedirectServesReachesTheFiguresOfItsLink(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		 VALUES ($1, $2, 'k7m2xq9d', 'https://example.com/', 'https://example.com/'),
		        ($1, $2, 'bcdfghjk', 'https://example.com/', 'https://example.com/')`,
		workspaceID, redirectDomain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET state = 'disabled', version = version + 1 WHERE identifier = 'bcdfghjk'`); err != nil {
		t.Fatal(err)
	}

	workerLog := process(t, env, "worker")
	probes := "http://" + announced(t, workerLog, "health endpoints listening")
	if got := status(t, probes+"/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz of the worker = %d, want 200", got)
	}
	redirectLog := process(t, env, "redirect")
	short := "http://" + announced(t, redirectLog, "redirect listening")

	started := time.Now()
	for range 5 {
		if got, _ := follow(t, short+"/k7m2xq9d"); got != http.StatusFound {
			t.Fatalf("GET = %d, want 302", got)
		}
	}
	if got, _ := follow(t, short+"/bcdfghjk"); got != http.StatusNotFound {
		t.Fatalf("GET of a disabled link = %d, want 404", got)
	}

	// Until the figures have the six accesses: the buffer of the redirect
	// is written every second, and the worker asks every second.
	figures := func() (redirected, inactive, clicks, processed, pending int64) {
		t.Helper()
		err := pool.QueryRow(t.Context(),
			`SELECT (SELECT coalesce(sum(events), 0) FROM link_hourly WHERE outcome = 'redirected' AND class = 'presumed_human'),
			        (SELECT coalesce(sum(events), 0) FROM link_hourly WHERE outcome = 'inactive'),
			        (SELECT coalesce(sum(events), 0) FROM link_daily_dimension WHERE dimension = 'referrer_host' AND value = '(none)'),
			        (SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'processed'),
			        (SELECT count(*) FROM event_inbox WHERE processed_at IS NULL)`).
			Scan(&redirected, &inactive, &clicks, &processed, &pending)
		if err != nil {
			t.Fatal(err)
		}
		return redirected, inactive, clicks, processed, pending
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		redirected, inactive, clicks, processed, pending := figures()
		if redirected == 5 && inactive == 1 && clicks == 5 && processed == 6 && pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 15 seconds the figures are %d redirected, %d inactive, %d clicks, %d processed and %d pending; want 5, 1, 5, 6 and 0; log of the worker: %s",
				redirected, inactive, clicks, processed, pending, workerLog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("six accesses were in the figures of their links %s after the first", time.Since(started).Round(100*time.Millisecond))

	// Counted once: nothing changes afterwards.
	time.Sleep(2 * time.Second)
	if redirected, inactive, _, processed, _ := figures(); redirected != 5 || inactive != 1 || processed != 6 {
		t.Errorf("two seconds later the figures are %d redirected, %d inactive and %d processed", redirected, inactive, processed)
	}
	if said := workerLog.String(); strings.Contains(said, "k7m2xq9d") || strings.Contains(said, "sentinel") || strings.Contains(said, "could not be processed") {
		t.Errorf("the log of the worker: %s", said)
	}
}

func TestTheWorkerRefusesADatabaseThatWasNotMigrated(t *testing.T) {
	env := environmentFor(postgrestest.NewDatabase(t))
	// A worker that started would run until it is stopped.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	if code := run(ctx, []string{"worker"}, env, io.Discard, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want %d; log: %s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "corinto migrate") || strings.Contains(stderr.String(), "listening") || strings.Contains(stderr.String(), "consumed") {
		t.Errorf("the worker did not name the command to run, or started: %s", stderr.String())
	}
}

// Without its database the worker processes nothing, and says so where an
// orchestrator looks: it is not ready. The outage is in its log once.
func TestTheWorkerIsNotReadyWithoutItsDatabase(t *testing.T) {
	databaseURL := postgrestest.NewDatabase(t)
	env := environmentFor(databaseURL)
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("migrate exit code = %d", code)
	}
	log := process(t, env, "worker")
	probes := "http://" + announced(t, log, "health endpoints listening")
	if got := status(t, probes+"/readyz"); got != http.StatusOK {
		t.Fatalf("with the database there /readyz = %d, want 200", got)
	}

	// The database of this process goes away, with its connections.
	target, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(target.Path, "/")
	target.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(t.Context())) }()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for status(t, probes+"/readyz") != http.StatusServiceUnavailable || !strings.Contains(log.String(), "could not be processed") {
		if time.Now().After(deadline) {
			t.Fatalf("the worker never said that its database was gone; /readyz = %d; log: %s", status(t, probes+"/readyz"), log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := status(t, probes+"/livez"); got != http.StatusOK {
		t.Errorf("without the database /livez = %d, want 200: the process is alive", got)
	}
	// Several polls later the outage is still said once.
	time.Sleep(3 * time.Second)
	if said := strings.Count(log.String(), "could not be processed"); said != 1 {
		t.Errorf("the outage is in the log %d times, want once: %s", said, log.String())
	}
}
