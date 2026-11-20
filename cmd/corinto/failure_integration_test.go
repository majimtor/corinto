//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// The failure tests: what docs/delivery-semantics.md says happens when a
// part of the path of an event fails, with the real processes and a real
// database between them.

// ground is what every failure test stands on: a workspace with a link and
// a key that reads its metrics.
type ground struct {
	pool                *pgxpool.Pool
	workspaceID, linkID string
	credential          string
}

const groundLink = "k7m2xq9d"

// newGround prepares a migrated database. The operator and the test reach
// it directly, whatever stands between it and the processes of the test.
func newGround(t *testing.T, databaseURL string) *ground {
	t.Helper()
	env := environmentFor(databaseURL)
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("migrate exit code = %d", code)
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	g := &ground{pool: pool}
	g.workspaceID, _ = printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	g.credential, _ = printed(t, env, "issue-key", "--workspace", g.workspaceID, "--name", "Reports", "--scope", "metrics:read")["key"].(string)
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		 VALUES ($1, $2, $3, 'https://example.com/', 'https://example.com/') RETURNING id::text`,
		g.workspaceID, redirectDomain, groundLink).Scan(&g.linkID); err != nil {
		t.Fatal(err)
	}
	return g
}

// counts is what the database says of the events of the link.
type counts struct {
	// events are the rows of the inbox, and pending the ones that wait.
	events, pending int64
	// counted is what the time series adds up to, and processed and
	// accepted what the counters say.
	counted, processed, accepted int64
	// retried is how many events have a failed attempt, and dead how many
	// were set aside.
	retried, dead int64
}

func (g *ground) counts(t *testing.T) counts {
	t.Helper()
	var c counts
	err := g.pool.QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM event_inbox),
		        (SELECT count(*) FROM event_inbox WHERE processed_at IS NULL),
		        (SELECT coalesce(sum(events), 0) FROM link_hourly),
		        (SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'processed'),
		        (SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'accepted'),
		        (SELECT count(*) FROM event_inbox WHERE attempts <> 0),
		        (SELECT count(*) FROM event_dead_letters)`).
		Scan(&c.events, &c.pending, &c.counted, &c.processed, &c.accepted, &c.retried, &c.dead)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// settle waits until the database says what a test expects of the events,
// and fails with what it says when it does not within the time.
func (g *ground) settle(t *testing.T, within time.Duration, what string, want func(counts) bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := g.counts(t)
		if want(got) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the events are not %s: %+v", within, what, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// accept writes events of the link to the inbox, as the redirect does, and
// returns them.
func (g *ground) accept(t *testing.T, n int) []ingest.Event {
	t.Helper()
	events := make([]ingest.Event, n)
	for i := range events {
		events[i] = ingest.Event{
			ID: uuid.NewV7().String(), LinkID: g.linkID, WorkspaceID: g.workspaceID,
			OccurredAt: time.Now().UTC().Truncate(time.Second), Outcome: ingest.OutcomeRedirected, Method: "GET",
			Classification: classify.Classification{
				Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
				Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
			},
		}
	}
	g.resend(t, events)
	return events
}

// resend writes events to the inbox through its store.
func (g *ground) resend(t *testing.T, events []ingest.Event) {
	t.Helper()
	inbox := ingest.NewStore(g.pool, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if settled, err := inbox.Accept(t.Context(), events); err != nil || settled != len(events) {
		t.Fatalf("Accept settled %d of %d events with the error %v", settled, len(events), err)
	}
}

// ingestion is what the status of the ingestion says of two days added
// up, in case a test runs across midnight.
type ingestion struct {
	status                                                   int
	code                                                     string
	through                                                  time.Time
	served, accepted, lost, processed, pending, deadLettered int64
}

// asked asks the API for the status of the ingestion with the key of the
// ground. The client checks every answer against the contract.
func (g *ground) asked(t *testing.T, api string) ingestion {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, api+"/v1/ingestion?days=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+g.credential)
	response, err := contracttest.Client(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var said struct {
		Code    string    `json:"code"`
		Through time.Time `json:"processed_through"`
		Days    []struct {
			Served, Accepted, Lost, Processed, Pending int64
			DeadLettered                               int64 `json:"dead_lettered"`
		} `json:"days"`
	}
	if err := json.NewDecoder(response.Body).Decode(&said); err != nil {
		t.Fatalf("the status of the ingestion = %d with a body that cannot be read: %v", response.StatusCode, err)
	}
	total := ingestion{status: response.StatusCode, code: said.Code, through: said.Through}
	for _, day := range said.Days {
		total.served, total.accepted, total.lost = total.served+day.Served, total.accepted+day.Accepted, total.lost+day.Lost
		total.processed, total.pending = total.processed+day.Processed, total.pending+day.Pending
		total.deadLettered += day.DeadLettered
	}
	return total
}

// logged waits until a process has logged something, and fails when it has
// not within fifteen seconds: several times what any of these processes
// takes to notice that its database went or came back.
func logged(t *testing.T, log *lockedBuffer, message string) {
	t.Helper()
	const within = 15 * time.Second
	deadline := time.Now().Add(within)
	for !strings.Contains(log.String(), message) {
		if time.Now().After(deadline) {
			t.Fatalf("after %s the process has not logged %q: %s", within, message, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The database is down: redirects continue from the replica, the events
// wait in the buffer, and the API answers 503. When the database returns,
// what waited is written and counted, and nothing was lost.
func TestTheDatabaseAbsent(t *testing.T) {
	databaseURL := postgrestest.NewDatabase(t)
	g := newGround(t, databaseURL)
	between, through := newRelay(t, databaseURL)
	env := environmentFor(through)

	workerLog := process(t, env, "worker")
	apiLog := process(t, env, "api")
	redirectLog := process(t, env, "redirect")
	api := "http://" + announced(t, apiLog, "api listening")
	short := "http://" + announced(t, redirectLog, "redirect listening") + "/" + groundLink
	probes := map[string]string{}
	for name, log := range map[string]*lockedBuffer{"worker": workerLog, "api": apiLog, "redirect": redirectLog} {
		probes[name] = "http://" + announced(t, log, "health endpoints listening")
	}

	// With the database there, an access is counted.
	if status, _ := follow(t, short); status != http.StatusFound {
		t.Fatalf("with the database there the short link = %d, want 302", status)
	}
	g.settle(t, 15*time.Second, "one, counted", func(c counts) bool { return c.counted == 1 && c.pending == 0 })

	between.cut()
	// The redirect answers from its copy, a link it has and one it has not.
	for range 5 {
		if status, destination := follow(t, short); status != http.StatusFound || destination != "https://example.com/" {
			t.Fatalf("without the database the short link = %d to %q, want 302 to its destination", status, destination)
		}
	}
	if status, _ := follow(t, short+"x"); status != http.StatusNotFound {
		t.Errorf("without the database a link nobody has = %d, want 404", status)
	}
	logged(t, redirectLog, "could not be refreshed")
	logged(t, redirectLog, "the inbox did not take what the buffer holds")
	logged(t, workerLog, "could not be processed")
	// It shows where the table of failures says: the copy of the redirect
	// gets older for as long as the database is away, and only the redirect
	// is still ready.
	age, links := replicaStatus(t, probes["redirect"])
	time.Sleep(1500 * time.Millisecond)
	if older, _ := replicaStatus(t, probes["redirect"]); older < age+1 || older < 3 || links != 1 {
		t.Errorf("without the database the copy was %.3f and then %.3f seconds old, with %d links; want it over a second older, past 3 seconds, and its link", age, older, links)
	}
	for name, want := range map[string]int{"redirect": http.StatusOK, "api": http.StatusServiceUnavailable, "worker": http.StatusServiceUnavailable} {
		if got := status(t, probes[name]+"/readyz"); got != want {
			t.Errorf("without the database /readyz of the %s = %d, want %d", name, got, want)
		}
	}
	// The API answers that it cannot reach its database, in the shape of
	// the contract.
	if said := g.asked(t, api); said.status != http.StatusServiceUnavailable || said.code != "unavailable" {
		t.Errorf("without the database the API = %d %s, want 503 unavailable", said.status, said.code)
	}
	// The five events wait in the memory of the redirect: nothing reached
	// the inbox.
	if got := g.counts(t); got.events != 1 || got.accepted != 1 {
		t.Errorf("without the database the inbox went to %+v, want the one event of before", got)
	}

	between.restore(t)
	// What waited is written, with the count of the responses that owed
	// it, and counted. Nothing was lost.
	g.settle(t, 30*time.Second, "six, all counted", func(c counts) bool { return c.events == 6 && c.counted == 6 && c.pending == 0 })
	logged(t, redirectLog, "up to date again")
	logged(t, redirectLog, "the inbox takes events again")
	logged(t, workerLog, "the inbox is being processed again")
	if said, want := g.asked(t, api), (ingestion{served: 6, accepted: 6, processed: 6}); said.status != http.StatusOK ||
		said.served != want.served || said.accepted != want.accepted || said.lost != 0 || said.processed != want.processed || said.pending != 0 || said.deadLettered != 0 {
		t.Errorf("with the database back the status of the ingestion is %+v, want 6 served, accepted and processed, and nothing lost", said)
	}
	if age, _ := replicaStatus(t, probes["redirect"]); age > 4 {
		t.Errorf("with the database back the copy is %.3f seconds old, want under 4", age)
	}
	for name := range probes {
		if got := status(t, probes[name]+"/readyz"); got != http.StatusOK {
			t.Errorf("with the database back /readyz of the %s = %d, want 200", name, got)
		}
	}
	// An outage is said once when it begins and once when it ends, by each
	// part that noticed, and nothing of a request is in any log.
	for message, log := range map[string]*lockedBuffer{
		"could not be refreshed": redirectLog, "up to date again": redirectLog,
		"the inbox did not take what the buffer holds": redirectLog, "the inbox takes events again": redirectLog,
		"could not be processed": workerLog, "the inbox is being processed again": workerLog,
	} {
		if said := strings.Count(log.String(), message); said != 1 {
			t.Errorf("%q is logged %d times, want once", message, said)
		}
	}
	for name, log := range map[string]*lockedBuffer{"worker": workerLog, "api": apiLog, "redirect": redirectLog} {
		if said := log.String(); strings.Contains(said, groundLink) || strings.Contains(said, "sentinel") {
			t.Errorf("the log of the %s carries something of a request: %s", name, said)
		}
	}
}

// A consumer that is stopped in the middle of a batch leaves its events
// pending for another attempt, and each is counted once. While the worker
// is stopped, what is accepted waits, nothing is lost, and the moment the
// figures reach stops where the oldest of them was accepted.
func TestAConsumerStoppedMidBatch(t *testing.T) {
	g := newGround(t, postgrestest.NewDatabase(t))
	env := environmentFor(g.pool.Config().ConnString())
	api := "http://" + announced(t, process(t, env, "api"), "api listening")
	const first, second = 20, 7
	g.accept(t, first)

	// The link is held: a consumer that has taken the events of the batch
	// cannot add them to the time series, which refers to the link, and
	// waits in the middle of its batch.
	holder, err := g.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := holder.Exec(t.Context(), `SELECT FROM links WHERE id = $1::uuid FOR UPDATE`, g.linkID); err != nil {
		t.Fatal(err)
	}
	workerLog, stopWorker := start(t, env, "worker")
	var session int
	deadline := time.Now().Add(15 * time.Second)
	for session == 0 {
		err := g.pool.QueryRow(t.Context(),
			`SELECT pid FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'INSERT INTO link_hourly%'`).Scan(&session)
		if err != nil && err != pgx.ErrNoRows { //nolint:errorlint // the row is asked for directly: the error is not wrapped
			t.Fatal(err)
		}
		if session == 0 && time.Now().After(deadline) {
			t.Fatalf("the worker never waited in the middle of a batch; log: %s", workerLog.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Its session ends there, as it does when the process dies: the
	// database undoes what the batch had done.
	if _, err := g.pool.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, session); err != nil {
		t.Fatal(err)
	}
	logged(t, workerLog, "could not be processed")
	if got := g.counts(t); got.events != first || got.pending != first || got.counted != 0 || got.processed != 0 || got.retried != 0 || got.dead != 0 {
		t.Errorf("after the batch was cut off the events are %+v, want the %d pending, none counted and none with a failed attempt", got, first)
	}

	// The link is let go, and the next attempt counts every event, once.
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.settle(t, 20*time.Second, "all counted", func(c counts) bool { return c.pending == 0 })
	if got := g.counts(t); got.counted != first || got.processed != first || got.retried != 0 || got.dead != 0 {
		t.Errorf("after the next attempt the events are %+v, want the %d counted once", got, first)
	}
	if said := strings.Count(workerLog.String(), "could not be processed"); said != 1 {
		t.Errorf("the batch that was cut off is logged %d times, want once: %s", said, workerLog.String())
	}

	// The worker is stopped. What is accepted from now on waits.
	stopWorker()
	waiting := g.accept(t, second)
	var oldest time.Time
	if err := g.pool.QueryRow(t.Context(), `SELECT min(accepted_at) FROM event_inbox WHERE id = ANY($1::uuid[])`,
		[]string{waiting[0].ID, waiting[second-1].ID}).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	for reading := range 2 {
		said := g.asked(t, api)
		if said.status != http.StatusOK || said.accepted != first+second || said.processed != first || said.pending != second || said.deadLettered != 0 {
			t.Errorf("with the worker stopped the status of the ingestion is %+v, want %d accepted, %d processed and %d pending", said, first+second, first, second)
		}
		// The moment the figures reach does not advance.
		if !said.through.Equal(oldest) {
			t.Errorf("reading %d: the events are processed through %s, want %s, when the oldest that waits was accepted", reading, said.through, oldest)
		}
		time.Sleep(1200 * time.Millisecond)
	}

	// A worker again: everything is counted once, and the moment moves on.
	process(t, env, "worker")
	g.settle(t, 20*time.Second, "all counted", func(c counts) bool { return c.pending == 0 })
	if got := g.counts(t); got.events != first+second || got.counted != first+second || got.processed != first+second || got.accepted != first+second || got.retried != 0 || got.dead != 0 {
		t.Errorf("with a worker again the events are %+v, want the %d counted once", got, first+second)
	}
	if said := g.asked(t, api); !said.through.After(oldest) || said.pending != 0 || said.processed != first+second {
		t.Errorf("with a worker again the status of the ingestion is %+v, want nothing pending and a moment past %s", said, oldest)
	}
}

// A batch that is written twice, or an event that arrives twice, is counted
// once: the second copy is ignored by its identifier, before it is
// processed and after.
func TestADuplicatedEvent(t *testing.T) {
	g := newGround(t, postgrestest.NewDatabase(t))
	env := environmentFor(g.pool.Config().ConnString())
	redirectLog := process(t, env, "redirect")
	short := "http://" + announced(t, redirectLog, "redirect listening") + "/" + groundLink
	// written returns the events the redirect wrote, as the inbox holds
	// them.
	written := func() []ingest.Event {
		t.Helper()
		rows, err := g.pool.Query(t.Context(),
			`SELECT id::text, link_id::text, workspace_id::text, occurred_at, outcome, method, coalesce(referrer_host, ''),
			        device, browser, class, rule, rules_version
			   FROM event_inbox ORDER BY accepted_at, id`)
		if err != nil {
			t.Fatal(err)
		}
		var (
			events []ingest.Event
			event  ingest.Event
		)
		_, err = pgx.ForEachRow(rows, []any{
			&event.ID, &event.LinkID, &event.WorkspaceID, &event.OccurredAt, &event.Outcome, &event.Method, &event.ReferrerHost,
			&event.Classification.Device, &event.Classification.Browser, &event.Classification.Class,
			&event.Classification.Rule, &event.Classification.Version,
		}, func() error {
			events = append(events, event)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return events
	}

	// Three accesses that wait: no worker yet. Their batch is written
	// again, as it would be if the answer of the database had been lost.
	for range 3 {
		if status, _ := follow(t, short); status != http.StatusFound {
			t.Fatalf("GET = %d, want 302", status)
		}
	}
	g.settle(t, 15*time.Second, "three, accepted", func(c counts) bool { return c.events == 3 })
	g.resend(t, written())
	if got := g.counts(t); got.events != 3 || got.accepted != 3 || got.pending != 3 {
		t.Fatalf("after the batch was written twice the inbox is %+v, want its 3 events, accepted once", got)
	}

	// The worker counts them, once.
	process(t, env, "worker")
	g.settle(t, 15*time.Second, "three, counted", func(c counts) bool { return c.counted == 3 && c.pending == 0 })

	// An event that arrives again after it was processed is still the same
	// event: it is not accepted, and it is not counted a second time.
	g.resend(t, written())
	if status, _ := follow(t, short); status != http.StatusFound {
		t.Fatalf("GET = %d, want 302", status)
	}
	g.settle(t, 15*time.Second, "four, counted", func(c counts) bool { return c.counted == 4 && c.pending == 0 })
	time.Sleep(2 * time.Second)
	if got := g.counts(t); got.events != 4 || got.accepted != 4 || got.counted != 4 || got.processed != 4 || got.dead != 0 {
		t.Errorf("after every event arrived twice the events are %+v, want 4, each accepted and counted once", got)
	}
}

// A change that is committed late still reaches the redirect: its
// transaction began before a poll and ended after it, so the change is
// older than what that poll had already read (ADR 0006).
func TestAChangeCommittedLateReachesTheRedirect(t *testing.T) {
	g := newGround(t, postgrestest.NewDatabase(t))
	env := environmentFor(g.pool.Config().ConnString())
	short := "http://" + announced(t, process(t, env, "redirect"), "redirect listening") + "/" + groundLink
	if status, _ := follow(t, short); status != http.StatusFound {
		t.Fatalf("GET = %d, want 302", status)
	}

	// A transaction disables the link, as the store does, and stays open
	// while the redirect polls more than once: every two seconds.
	late, err := g.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := late.Exec(t.Context(),
		`UPDATE links SET state = 'disabled', version = version + 1, updated_at = now() WHERE id = $1::uuid`, g.linkID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	// Nothing is committed: the link still redirects.
	if status, _ := follow(t, short); status != http.StatusFound {
		t.Fatalf("with the change not committed yet the short link = %d, want 302", status)
	}
	if err := late.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	// It arrives within the time of any other change, though its moment is
	// five seconds before the last one the redirect had read.
	t.Logf("a change committed five seconds late stops the link after %s", until(t, short, http.StatusNotFound).Round(time.Millisecond))
}
