//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// deactivation is the objective of PLAN.md section 15: how long a link
// keeps redirecting after it is disabled, at the most.
const deactivation = 5 * time.Second

// process starts a command of the binary and returns its log. It is
// stopped when the test ends, and must stop cleanly.
func process(t *testing.T, env func(string) string, command string) *lockedBuffer {
	t.Helper()
	log, _ := start(t, env, command)
	return log
}

// start starts a command of the binary and returns its log and what stops
// it and waits for it to have stopped, for a test that goes on after it.
func start(t *testing.T, env func(string) string, command string) (*lockedBuffer, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{command}, env, io.Discard, stderr) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case code := <-exited:
			if code != exitOK {
				t.Errorf("%s: exit code = %d after a clean stop; log: %s", command, code, stderr.String())
			}
		case <-time.After(20 * time.Second):
			t.Errorf("%s did not stop", command)
		}
	})
	t.Cleanup(stop)
	return stderr, stop
}

// follow asks the host of the short links for a path, without going where
// the answer leads, and returns the status and the Location.
func follow(t *testing.T, address string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("User-Agent", "sentinel-agent")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode, response.Header.Get("Location")
}

// until asks for a short link until it answers with a status, and returns
// how long that took. It fails when the objective passes first.
func until(t *testing.T, address string, status int) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) <= deactivation {
		if got, _ := follow(t, address); got == status {
			return time.Since(start)
		}
		// Ten times a second: half of what the host lets one address ask.
		time.Sleep(100 * time.Millisecond)
	}
	got, _ := follow(t, address)
	t.Fatalf("after %s the short link still answers %d, want %d", deactivation, got, status)
	return 0
}

// A link created through the API redirects, and stops redirecting within
// the published time after it is disabled or blocked: two processes that
// share nothing but the database.
func TestAChangeToALinkReachesTheRedirectWithinFiveSeconds(t *testing.T) {
	env, _ := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	credential, _ := printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline",
		"--scope", "links:read", "--scope", "links:write")["key"].(string)

	redirectLog := process(t, env, "redirect")
	api := "http://" + announced(t, process(t, env, "api"), "api listening")
	short := "http://" + announced(t, redirectLog, "redirect listening")

	client := contracttest.Client(t)
	call := func(method, path, body string) (int, http.Header, string) {
		t.Helper()
		var sent io.Reader
		if body != "" {
			sent = strings.NewReader(body)
		}
		request, err := http.NewRequestWithContext(t.Context(), method, api+path, sent)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+credential)
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		read, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, string(read)
	}

	// Created after the redirect loaded its copy: it arrives with a poll.
	status, header, body := call(http.MethodPost, "/v1/links", `{"destination":"https://example.com/landing","campaign":{"source":"newsletter"}}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/links = %d", status)
	}
	location := header.Get("Location")
	_, after, _ := strings.Cut(body, `"identifier":"`)
	identifier, _, _ := strings.Cut(after, `"`)
	link := short + "/" + identifier

	t.Logf("a new link redirects after %s", until(t, link, http.StatusFound).Round(time.Millisecond))
	if _, destination := follow(t, link); destination != "https://example.com/landing?utm_source=newsletter" {
		t.Errorf("the short link leads to %q", destination)
	}

	if status, _, _ := call(http.MethodPost, location+"/disable", ""); status != http.StatusOK {
		t.Fatalf("disable = %d", status)
	}
	t.Logf("a disabled link stops redirecting after %s", until(t, link, http.StatusNotFound).Round(time.Millisecond))

	if status, _, _ := call(http.MethodPost, location+"/enable", ""); status != http.StatusOK {
		t.Fatalf("enable = %d", status)
	}
	until(t, link, http.StatusFound)

	printed(t, env, "block-link", "--identifier", identifier)
	t.Logf("a blocked link stops redirecting after %s", until(t, link, http.StatusNotFound).Round(time.Millisecond))

	// What the redirect logged says nothing of who asked or for what.
	if log := redirectLog.String(); strings.Contains(log, identifier) || strings.Contains(log, "sentinel") {
		t.Errorf("the log of the redirect carries something of a request: %s", log)
	}
}

// Once it has its links the redirect does not need the database to answer.
func TestTheRedirectKeepsAnsweringWhenItsDatabaseIsGone(t *testing.T) {
	databaseURL := postgrestest.NewDatabase(t)
	env := environmentFor(databaseURL)
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("migrate exit code = %d", code)
	}
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	conn, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var identifier string
	if err := conn.QueryRow(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		 VALUES ($1, $2, 'k7m2xq9d', 'https://example.com/', 'https://example.com/') RETURNING identifier`,
		workspaceID, redirectDomain).Scan(&identifier); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	log := process(t, env, "redirect")
	short := "http://" + announced(t, log, "redirect listening")
	probes := "http://" + announced(t, log, "health endpoints listening")
	if status, _ := follow(t, short+"/"+identifier); status != http.StatusFound {
		t.Fatalf("with the database there the short link = %d, want 302", status)
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

	// Until a poll has failed, which the process says once.
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(log.String(), "could not be refreshed") {
		if time.Now().After(deadline) {
			t.Fatalf("the redirect never noticed that its database was gone; log: %s", log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, destination := follow(t, short+"/"+identifier); status != http.StatusFound || destination != "https://example.com/" {
		t.Errorf("without the database the short link = %d to %q, want 302 to its destination", status, destination)
	}
	if status, _ := follow(t, short+"/bcdfghjk"); status != http.StatusNotFound {
		t.Errorf("without the database an unknown link = %d, want 404", status)
	}
	if got := status(t, probes+"/readyz"); got != http.StatusOK {
		t.Errorf("without the database /readyz = %d, want 200: the process still answers", got)
	}
	if strings.Count(log.String(), "could not be refreshed") != 1 {
		t.Errorf("the outage is logged more than once: %s", log.String())
	}
}

func TestTheRedirectRefusesADatabaseThatWasNotMigrated(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), refusal)
	defer cancel()
	var stderr lockedBuffer
	code := run(ctx, []string{"redirect"}, environmentFor(postgrestest.NewDatabase(t)), io.Discard, &stderr)
	if code != exitFailure || !strings.Contains(stderr.String(), "corinto migrate") || strings.Contains(stderr.String(), "listening") {
		t.Errorf("exit code = %d; log: %s", code, stderr.String())
	}
}

// What the redirect serves reaches the inbox, and what it does not serve
// is counted. The process is stopped right after its last answer: a clean
// stop writes what the buffer holds before the process ends.
func TestWhatTheRedirectServesReachesTheInbox(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	var linkID string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		 VALUES ($1, $2, 'k7m2xq9d', 'https://example.com/', 'https://example.com/') RETURNING id::text`,
		workspaceID, redirectDomain).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, state, destination, resolved_destination)
		 VALUES ($1, $2, 'bcdfghjk', 'disabled', 'https://example.com/', 'https://example.com/')`,
		workspaceID, redirectDomain); err != nil {
		t.Fatal(err)
	}

	log, stop := start(t, env, "redirect")
	short := "http://" + announced(t, log, "redirect listening")
	before := time.Now().UTC().Truncate(time.Second)
	for _, asked := range []struct {
		path string
		want int
	}{
		{"/k7m2xq9d", http.StatusFound}, {"/k7m2xq9d?token=sentinel-query", http.StatusFound}, {"/K7M2XQ9D", http.StatusFound},
		{"/bcdfghjk", http.StatusNotFound},
		{"/mnpqrstv", http.StatusNotFound}, {"/not/a/link", http.StatusNotFound},
	} {
		if status, _ := follow(t, short+asked.path); status != asked.want {
			t.Fatalf("GET %s = %d, want %d", asked.path, status, asked.want)
		}
	}
	stop()

	// Three redirects and the 404 of a link that is disabled owe an event;
	// the two paths that name no link are counted.
	rows, err := pool.Query(t.Context(),
		`SELECT link_id::text = $1, workspace_id::text = $2, occurred_at, outcome, method, referrer_host IS NULL,
		        device, browser, class, rule, rules_version, processed_at IS NULL
		   FROM event_inbox ORDER BY outcome DESC, id`, linkID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	type stored struct {
		link, workspace, noReferrer, pending bool
		at                                   time.Time
		outcome, method, device, browser     string
		class, rule                          string
		version                              int
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
		var e stored
		err := row.Scan(&e.link, &e.workspace, &e.at, &e.outcome, &e.method, &e.noReferrer,
			&e.device, &e.browser, &e.class, &e.rule, &e.version, &e.pending)
		return e, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("the inbox has %d events, want 4; log: %s", len(events), log.String())
	}
	for i, event := range events {
		wantOutcome, wantLink := "redirected", true
		if i == 3 {
			wantOutcome, wantLink = "inactive", false
		}
		if event.outcome != wantOutcome || event.link != wantLink || !event.workspace || event.method != http.MethodGet || !event.noReferrer || !event.pending {
			t.Errorf("the event %d was stored as %+v", i, event)
		}
		// What an agent nobody declared gets from the rules.
		if event.class != "presumed_human" || event.rule != "presumed_human" || event.version < 1 || event.device != "other" || event.browser != "other" {
			t.Errorf("the event %d was classified as %+v", i, event)
		}
		if event.at.Before(before) || event.at.After(time.Now()) || event.at.Nanosecond() != 0 {
			t.Errorf("the event %d occurred at %s", i, event.at)
		}
	}
	today := time.Now().UTC().Format(time.DateOnly)
	for name, want := range map[string]int64{"served": 4, "accepted": 4, "not_found": 2, "rate_limited": 0} {
		var value int64
		if err := pool.QueryRow(t.Context(),
			`SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = $1 AND day = $2::date`, name, today).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != want {
			t.Errorf("the counter %s is at %d, want %d", name, value, want)
		}
	}
	// What the redirect logged says nothing of who asked or for what.
	if said := log.String(); strings.Contains(said, "k7m2xq9d") || strings.Contains(said, "sentinel") || strings.Contains(said, "events that the inbox did not take") {
		t.Errorf("the log of the redirect: %s", said)
	}
}
