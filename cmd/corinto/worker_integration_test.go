//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/contract/contracttest"
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
	// Only the redirect has something to say of itself.
	if got := status(t, probes+"/statusz"); got != http.StatusNotFound {
		t.Errorf("/statusz of the worker = %d, want 404", got)
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
	// The version of the rules that classified the accesses is recorded,
	// with the hours of the first and of the last.
	var versions, theirs int64
	err := pool.QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM rule_versions),
		        (SELECT count(*) FROM rule_versions AS v
		          WHERE v.version = (SELECT DISTINCT rules_version FROM event_inbox)
		            AND v.first_hour = (SELECT min(date_trunc('hour', occurred_at, 'UTC')) FROM event_inbox)
		            AND v.last_hour = (SELECT max(date_trunc('hour', occurred_at, 'UTC')) FROM event_inbox))`).
		Scan(&versions, &theirs)
	if err != nil || versions != 1 || theirs != 1 {
		t.Errorf("%d versions of the rules are recorded, %d of them with the hours of the accesses (%v); want one", versions, theirs, err)
	}

	// And the figures are what the API answers about the link to a key of
	// its workspace: the fourth process, and the end of the path.
	credential, _ := printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Reports", "--scope", "metrics:read")["key"].(string)
	var linkID string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM links WHERE identifier = 'k7m2xq9d'`).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	api := "http://" + announced(t, process(t, env, "api"), "api listening")
	// Three days around this one, in case the test runs across midnight.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	period := url.Values{"from": {today.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {today.Add(48 * time.Hour).Format(time.RFC3339)}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, api+"/v1/links/"+linkID+"/metrics/summary?"+period.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	// The client checks the answer against the contract.
	response, err := contracttest.Client(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Requests             int64     `json:"requests"`
		InactiveRequests     int64     `json:"inactive_requests"`
		EstimatedHumanClicks int64     `json:"estimated_human_clicks"`
		ProcessedThrough     time.Time `json:"processed_through"`
		Synthetic            bool      `json:"synthetic"`
		RuleVersions         []struct {
			Version int `json:"version"`
		} `json:"rule_versions"`
	}
	if err := json.Unmarshal(body, &summary); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("the summary of the link = %d %s (%v)", response.StatusCode, body, err)
	}
	if summary.Requests != 5 || summary.InactiveRequests != 0 || summary.EstimatedHumanClicks != 5 || summary.Synthetic || len(summary.RuleVersions) != 1 {
		t.Errorf("the summary of the link is %s; want 5 requests, all of them estimated human clicks, under one version of the rules", body)
	}
	// Nothing waits: the figures reach the moment of the answer.
	if behind := time.Since(summary.ProcessedThrough); behind < -time.Minute || behind > time.Minute {
		t.Errorf("the figures reach %s, %s from now", summary.ProcessedThrough, behind)
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

// The worker deletes what has been kept for its time: once a day, the
// first worker that asks, and no other after it.
func TestTheWorkerDeletesWhatIsPastItsTime(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	// Two events that were processed: one accepted 31 days ago, and one
	// accepted 29 days ago.
	if _, err := pool.Exec(t.Context(),
		`WITH link AS (
		     INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		     VALUES ($1, $2, 'k7m2xq9d', 'https://example.com/', 'https://example.com/') RETURNING id, workspace_id
		 )
		 INSERT INTO event_inbox (id, link_id, workspace_id, occurred_at, outcome, method, device, browser, class, rule,
		                          rules_version, accepted_at, processed_at)
		 SELECT uuidv7(), link.id, link.workspace_id, now() - age, 'redirected', 'GET', 'desktop', 'chrome',
		        'presumed_human', 'presumed_human', 1, now() - age, now() - age + interval '1 second'
		   FROM link, (VALUES (interval '31 days'), (interval '29 days')) AS aged (age)`,
		workspaceID, redirectDomain); err != nil {
		t.Fatal(err)
	}

	log := process(t, env, "worker")
	state := func() (runs, finished, old, recent int) {
		t.Helper()
		err := pool.QueryRow(t.Context(),
			`SELECT (SELECT count(*) FROM retention_runs),
			        (SELECT count(*) FROM retention_runs WHERE finished_at IS NOT NULL AND inbox_events = 1),
			        (SELECT count(*) FROM event_inbox WHERE accepted_at < now() - interval '30 days'),
			        (SELECT count(*) FROM event_inbox WHERE accepted_at >= now() - interval '30 days')`).
			Scan(&runs, &finished, &old, &recent)
		if err != nil {
			t.Fatal(err)
		}
		return runs, finished, old, recent
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs, finished, old, recent := state()
		if runs == 1 && finished == 1 && old == 0 && recent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 15 seconds there are %d runs, %d of them finished with one event deleted, and %d old and %d recent events; want 1, 1, 0 and 1; log: %s",
				runs, finished, old, recent, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(log.String(), `"msg":"the retention run finished"`) || !strings.Contains(log.String(), `"inbox_events":1`) {
		t.Errorf("the log of the worker does not say what the run deleted: %s", log.String())
	}

	// Another worker starts on the same day, and finds the run done.
	second := process(t, env, "worker")
	announced(t, second, "the inbox is being consumed")
	time.Sleep(2 * time.Second)
	if runs, _, _, recent := state(); runs != 1 || recent != 1 {
		t.Errorf("after a second worker started there are %d runs and %d recent events, want 1 and 1", runs, recent)
	}
	if strings.Contains(second.String(), "the retention run") {
		t.Errorf("the second worker did a run, or failed one: %s", second.String())
	}
}
