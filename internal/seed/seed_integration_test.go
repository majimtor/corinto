//go:build integration

package seed

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
	"github.com/majimtor/corinto/internal/rollup"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// stage is a migrated database with the stores a seed writes through.
type stage struct {
	pool  *pgxpool.Pool
	links *link.Store
	keys  *auth.Keys
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
	return &stage{pool: pool, links: link.NewStore(pool, "go.example.dev", rand.Reader), keys: auth.NewKeys(pool, rand.Reader)}
}

func (s *stage) run(t *testing.T, now time.Time) []Workspace {
	t.Helper()
	seeded, err := Run(t.Context(), s.pool, s.links, s.keys, now)
	if err != nil {
		t.Fatal(err)
	}
	return seeded
}

func (s *stage) number(t *testing.T, query string, arguments ...any) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(t.Context(), query, arguments...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The tables a workspace has rows in, in the order in which a count of
// them is written down.
var tables = []string{"links", "api_keys", "event_inbox", "link_hourly", "link_daily_dimension", "pipeline_counters"}

// held returns how many rows a workspace has in each table.
func (s *stage) held(t *testing.T, workspaceID string) []int64 {
	t.Helper()
	rows := make([]int64, len(tables))
	for i, table := range tables {
		rows[i] = s.number(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1::uuid`, workspaceID)
	}
	return rows
}

// traffic accepts three clicks on a link and has two of them counted: the
// link is left with events, one of them waiting, aggregates and counts.
func (s *stage) traffic(t *testing.T, target link.Link) {
	t.Helper()
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	inbox, consumer := ingest.NewStore(s.pool, quiet), rollup.NewConsumer(s.pool, quiet)
	click := func() ingest.Event {
		return ingest.Event{
			ID: uuid.NewV7().String(), LinkID: target.ID, WorkspaceID: target.WorkspaceID,
			OccurredAt: time.Date(2026, time.November, 19, 10, 0, 0, 0, time.UTC),
			Outcome:    ingest.OutcomeRedirected, Method: "GET",
			Classification: classify.Classification{
				Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
				Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
			},
		}
	}
	if settled, err := inbox.Accept(t.Context(), []ingest.Event{click(), click()}); err != nil || settled != 2 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	for {
		processed, err := consumer.Batch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if processed == 0 {
			break
		}
	}
	if settled, err := inbox.Accept(t.Context(), []ingest.Event{click()}); err != nil || settled != 1 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
}

var seedDay = time.Date(2026, time.November, 20, 15, 4, 5, 0, time.UTC)

func TestTheSeedCreatesTheDemoWorkspacesWithTheirLinksAndAKeyEach(t *testing.T) {
	s := newStage(t)
	seeded := s.run(t, seedDay)
	if len(seeded) != 2 || seeded[0].Name != "Lighthouse Books" || seeded[1].Name != "Cascade Cycles" || seeded[0].ID == seeded[1].ID {
		t.Fatalf("the seed left %+v, want its two workspaces", seeded)
	}
	for n, workspace := range seeded {
		// A demo, which may choose the identifiers of its links and send
		// them anywhere, with the quotas of one.
		if ok := s.number(t, `SELECT count(*) FROM workspaces WHERE id = $1::uuid AND name = $2 AND demo AND may_choose_aliases
		                         AND state = 'active' AND allowed_destinations = '[]'::jsonb AND max_links = $3 AND max_keys = $4`,
			workspace.ID, workspace.Name, limits.MaxLinksDemo, limits.MaxKeys); ok != 1 {
			t.Errorf("%s is not stored as a demo workspace with its quotas", workspace.Name)
		}
		// Its links are the ones of the seed, each by its alias.
		want := make([]string, len(demos[n].links))
		for i, entry := range demos[n].links {
			want[i] = entry.alias
		}
		if !slices.Equal(workspace.Links, want) || s.number(t, `SELECT count(*) FROM links WHERE workspace_id = $1::uuid AND state = 'active'`, workspace.ID) != int64(len(want)) {
			t.Errorf("%s has the links %v, want %v", workspace.Name, workspace.Links, want)
		}
		// Its key opens it, and may do everything in it. Nothing but a hash
		// of the key is stored.
		principal, err := s.keys.Authenticate(t.Context(), workspace.Key)
		if err != nil || principal.WorkspaceID != workspace.ID || principal.KeyID != workspace.KeyID || !slices.Equal(principal.Scopes, auth.Scopes()) || principal.ExpiresAt != nil {
			t.Errorf("the key of %s authenticates as %+v (%v), want its workspace with every scope and no expiry", workspace.Name, principal, err)
		}
		if !strings.HasPrefix(workspace.Key, "cor_") || s.number(t, `SELECT count(*) FROM api_keys WHERE workspace_id = $1::uuid AND name = 'Seed key' AND revoked_at IS NULL`, workspace.ID) != 1 {
			t.Errorf("%s does not have its one key", workspace.Name)
		}
	}

	// A link of a campaign redirects to its destination with the
	// parameters of the campaign.
	var resolved, title string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT resolved_destination, title FROM links WHERE identifier = 'spring-reading-ig' AND workspace_id = $1::uuid`, seeded[0].ID).Scan(&resolved, &title); err != nil ||
		resolved != "https://www.example.com/books/spring-reading?utm_source=instagram&utm_medium=social&utm_campaign=spring-reading" || title != "Spring reading list, Instagram" {
		t.Errorf("the link of a campaign redirects to %q with the title %q (%v)", resolved, title, err)
	}
	// The link that has expired did so twenty days before the day of the
	// seed, at its midnight in UTC, whatever the hour of the run.
	var expiry time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT expires_at FROM links WHERE identifier = 'winter-clearance'`).Scan(&expiry); err != nil ||
		!expiry.Equal(time.Date(2026, time.October, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the link that has expired did so at %s (%v), want twenty days before the day of the seed", expiry, err)
	}
	if others := s.number(t, `SELECT count(*) FROM links WHERE expires_at IS NOT NULL AND identifier <> 'winter-clearance'`); others != 0 {
		t.Errorf("%d other links expire", others)
	}

	// The two workspaces are there so that one can be tried against the
	// other: a link of one is not found by the other.
	first, _, err := s.links.List(t.Context(), seeded[0].ID, "", 50)
	if err != nil || len(first) != 10 {
		t.Fatalf("the first workspace lists %d links (%v), want 10", len(first), err)
	}
	if _, err := s.links.Get(t.Context(), seeded[1].ID, first[0].ID); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("a link of one demo workspace read from the other: %v, want %v", err, link.ErrNotFound)
	}
}

func TestTheSeedRecreatesTheDemoWorkspacesAndTouchesNoOther(t *testing.T) {
	s := newStage(t)
	// A workspace that is not a demo, with everything a workspace can
	// have: links, a key, events, aggregates and counts.
	ordinary, err := link.CreateWorkspace(t.Context(), s.pool, link.WorkspaceDraft{Name: "Acme", MayChooseAliases: true, MaxLinks: 1000, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.links.Create(t.Context(), ordinary.ID, link.Draft{Alias: "keep-me", Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	_, theirKey, err := s.keys.Issue(t.Context(), ordinary.ID, auth.Draft{Name: "Deploy pipeline", Scopes: []auth.Scope{auth.ScopeLinksRead}})
	if err != nil {
		t.Fatal(err)
	}
	s.traffic(t, kept)

	first := s.run(t, seedDay)
	// What somebody did in a demo workspace after the seed: traffic on one
	// of its links, a link of their own and another key.
	var seededLink link.Link
	listed, _, err := s.links.List(t.Context(), first[0].ID, "", 50)
	if err != nil || len(listed) == 0 {
		t.Fatalf("the first demo workspace lists %d links (%v)", len(listed), err)
	}
	seededLink = listed[0]
	s.traffic(t, seededLink)
	if _, err := s.links.Create(t.Context(), first[0].ID, link.Draft{Alias: "my-own-link", Destination: "https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.keys.Issue(t.Context(), first[0].ID, auth.Draft{Name: "Mine", Scopes: []auth.Scope{auth.ScopeLinksRead}}); err != nil {
		t.Fatal(err)
	}
	// 11 links, two keys, three events, an hour, three values of the
	// breakdowns and two counts: accepted and processed.
	if before, want := s.held(t, first[0].ID), []int64{11, 2, 3, 1, 3, 2}; !slices.Equal(before, want) {
		t.Fatalf("before the second seed the demo workspace holds %v in %v, want %v", before, tables, want)
	}
	theirs := s.held(t, ordinary.ID)
	if want := []int64{1, 1, 3, 1, 3, 2}; !slices.Equal(theirs, want) {
		t.Fatalf("the workspace that is not a demo holds %v in %v, want %v", theirs, tables, want)
	}
	versions := s.number(t, `SELECT count(*) FROM rule_versions`)

	second := s.run(t, seedDay.Add(24*time.Hour))
	// The demo workspaces are new ones, with the same names and links.
	if len(second) != 2 || second[0].ID == first[0].ID || second[1].ID == first[1].ID || second[0].Name != first[0].Name ||
		!slices.Equal(second[0].Links, first[0].Links) || !slices.Equal(second[1].Links, first[1].Links) {
		t.Fatalf("the second seed left %+v after %+v", second, first)
	}
	if workspaces, demoLinks := s.number(t, `SELECT count(*) FROM workspaces`), s.number(t, `SELECT count(*) FROM links AS l JOIN workspaces AS w ON w.id = l.workspace_id WHERE w.demo`); workspaces != 3 || demoLinks != 16 {
		t.Errorf("there are %d workspaces and %d links in the demos, want 3 and 16", workspaces, demoLinks)
	}
	// Nothing is left of the old ones: no row of theirs in any table, and
	// their keys open nothing.
	for _, old := range first {
		if rows := s.held(t, old.ID); !slices.Equal(rows, make([]int64, len(tables))) || s.number(t, `SELECT count(*) FROM workspaces WHERE id = $1::uuid`, old.ID) != 0 {
			t.Errorf("the old demo workspace %s still holds %v in %v", old.Name, rows, tables)
		}
		if _, err := s.keys.Authenticate(t.Context(), old.Key); !errors.Is(err, auth.ErrInvalidKey) {
			t.Errorf("the old key of %s: %v, want %v", old.Name, err, auth.ErrInvalidKey)
		}
	}
	// The new ones hold their links and their key, and nothing of what
	// happened in the old ones.
	if rows, want := s.held(t, second[0].ID), []int64{10, 1, 0, 0, 0, 0}; !slices.Equal(rows, want) {
		t.Errorf("the new demo workspace holds %v in %v, want %v", rows, tables, want)
	}
	for _, workspace := range second {
		if _, err := s.keys.Authenticate(t.Context(), workspace.Key); err != nil {
			t.Errorf("the new key of %s is refused: %v", workspace.Name, err)
		}
	}
	// The link that has expired did so twenty days before the new day.
	if moved := s.number(t, `SELECT count(*) FROM links WHERE identifier = 'winter-clearance' AND expires_at = '2026-11-01T00:00:00Z'`); moved != 1 {
		t.Error("the link that has expired did not do so twenty days before the day of the second seed")
	}

	// The workspace that is not a demo is as it was, and so is what
	// belongs to no workspace.
	if after := s.held(t, ordinary.ID); !slices.Equal(after, theirs) {
		t.Errorf("the workspace that is not a demo holds %v in %v, want %v as before", after, tables, theirs)
	}
	if _, err := s.keys.Authenticate(t.Context(), theirKey); err != nil {
		t.Errorf("the key of the workspace that is not a demo is refused: %v", err)
	}
	if found, err := s.links.Get(t.Context(), ordinary.ID, kept.ID); err != nil || found.Identifier != "keep-me" {
		t.Errorf("the link of the workspace that is not a demo is %+v, %v", found, err)
	}
	if after := s.number(t, `SELECT count(*) FROM rule_versions`); after != versions || versions != 1 {
		t.Errorf("the versions of the rules went from %d rows to %d", versions, after)
	}
}

// One seed runs at a time. A second one does nothing and says why.
func TestOneSeedRunsAtATime(t *testing.T) {
	s := newStage(t)
	other, err := s.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			other.Release()
		}
	}()
	if _, err := other.Exec(t.Context(), `SELECT pg_advisory_lock($1, $2)`, turnOfTheJobs, turnOfTheSeed); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if seeded, err := Run(bounded, s.pool, s.links, s.keys, seedDay); !errors.Is(err, ErrBusy) || seeded != nil {
		t.Fatalf("with another seed running, Run = %v, %v; want %v", seeded, err, ErrBusy)
	}
	if workspaces := s.number(t, `SELECT count(*) FROM workspaces`); workspaces != 0 {
		t.Errorf("a seed that did not have the turn created %d workspaces", workspaces)
	}
	if _, err := other.Exec(t.Context(), `SELECT pg_advisory_unlock($1, $2)`, turnOfTheJobs, turnOfTheSeed); err != nil {
		t.Fatal(err)
	}
	other.Release()
	released = true

	// With the turn free it runs, and gives the turn back: a session that
	// is not of its pool can take it afterwards.
	s.run(t, seedDay)
	s.run(t, seedDay)
	if workspaces := s.number(t, `SELECT count(*) FROM workspaces`); workspaces != 2 {
		t.Errorf("after two seeds there are %d workspaces, want the two of the seed", workspaces)
	}
	outsider, err := pgx.Connect(t.Context(), s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outsider.Close(context.WithoutCancel(t.Context())) }()
	var free bool
	if err := outsider.QueryRow(t.Context(), `SELECT pg_try_advisory_lock($1, $2)`, turnOfTheJobs, turnOfTheSeed).Scan(&free); err != nil || !free {
		t.Errorf("after a seed its turn is still taken (%v)", err)
	}
}

// The identifiers of the seed are its own. When a workspace that is not a
// demo holds one, the seed says which and stops: it does not take it, and
// the next run starts from nothing again.
func TestTheSeedStopsAtAnIdentifierAnotherWorkspaceHolds(t *testing.T) {
	s := newStage(t)
	ordinary, err := link.CreateWorkspace(t.Context(), s.pool, link.WorkspaceDraft{Name: "Acme", MayChooseAliases: true, MaxLinks: 1000, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	taken, err := s.links.Create(t.Context(), ordinary.ID, link.Draft{Alias: "summer-sale", Destination: "https://example.com/theirs"})
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := Run(t.Context(), s.pool, s.links, s.keys, seedDay)
	if !errors.Is(err, link.ErrAliasTaken) || seeded != nil || !strings.Contains(err.Error(), "Cascade Cycles") || !strings.Contains(err.Error(), "summer-sale") {
		t.Fatalf("Run = %v, %v; want it to stop at the identifier that is taken, and to name it", seeded, err)
	}
	if found, err := s.links.Get(t.Context(), ordinary.ID, taken.ID); err != nil || found.Destination != "https://example.com/theirs" {
		t.Errorf("the link that held the identifier is %+v, %v", found, err)
	}
	// With the identifier free, the next run leaves the two workspaces and
	// nothing of the one that stopped halfway.
	if err := s.links.Delete(t.Context(), ordinary.ID, taken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM links WHERE id = $1::uuid`, taken.ID); err != nil {
		t.Fatal(err)
	}
	s.run(t, seedDay)
	if demos, links := s.number(t, `SELECT count(*) FROM workspaces WHERE demo`), s.number(t, `SELECT count(*) FROM links AS l JOIN workspaces AS w ON w.id = l.workspace_id WHERE w.demo`); demos != 2 || links != 16 {
		t.Errorf("after the run that could finish there are %d demo workspaces with %d links, want 2 and 16", demos, links)
	}
}
