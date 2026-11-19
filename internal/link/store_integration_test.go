//go:build integration

package link_test

import (
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

const redirectDomain = "go.example.dev"

func TestMain(m *testing.M) { postgrestest.Main(m) }

// database returns a pool on a migrated database of its own.
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// workspace inserts a workspace and returns its identifier. settings is the
// rest of an INSERT, so that each test states only what it cares about.
func workspace(t *testing.T, pool *pgxpool.Pool, columns, values string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO workspaces (name, max_links, max_keys`+columns+`) VALUES ('Acme', 1000, 10`+values+`) RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sequence is a source of randomness that makes the generator produce the
// given symbols, one identifier after another. Every byte below 27 maps to
// the symbol at its position in the alphabet.
type sequence struct{ identifiers []string }

func (s *sequence) Read(p []byte) (int, error) {
	if len(s.identifiers) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	next := s.identifiers[0]
	s.identifiers = s.identifiers[1:]
	for i := range p {
		p[i] = indexInAlphabet(next[i%len(next)])
	}
	return len(p), nil
}

func indexInAlphabet(symbol byte) byte {
	for i := range byte(len(alphabet)) {
		if alphabet[i] == symbol {
			return i
		}
	}
	panic("a symbol outside the alphabet in a test")
}

func TestCreateStoresALinkAndWhatTheRedirectServes(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)

	created, err := store.Create(t.Context(), owner, link.Draft{
		Destination: "HTTPS://Example.com/landing?ref=home#top",
		Campaign:    link.Campaign{Source: "newsletter", Name: "launch"},
		Title:       "Launch",
		Tags:        []string{"q4", "email"},
		ExpiresAt:   &expiry,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !link.WellFormedIdentifier(created.Identifier) || len(created.Identifier) != 8 {
		t.Errorf("identifier = %q, want a generated one", created.Identifier)
	}
	if created.Domain != redirectDomain || created.WorkspaceID != owner || created.State != link.StateActive || created.Version != 1 {
		t.Errorf("created = %+v", created)
	}
	if created.Destination != "https://example.com/landing?ref=home#top" {
		t.Errorf("destination = %q, want it normalised", created.Destination)
	}
	if want := "https://example.com/landing?ref=home&utm_source=newsletter&utm_campaign=launch#top"; created.Resolved != want {
		t.Errorf("resolved = %q, want %q", created.Resolved, want)
	}
	if created.Title != "Launch" || !slices.Equal(created.Tags, []string{"q4", "email"}) || created.ExpiresAt == nil || !created.ExpiresAt.Equal(expiry) {
		t.Errorf("title, tags or expiry were not stored as given: %+v", created)
	}

	read, err := store.Get(t.Context(), owner, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != created.ID || read.Resolved != created.Resolved || read.Campaign != created.Campaign || read.Version != 1 {
		t.Errorf("what is read is not what was created: %+v", read)
	}
}

func TestCreateTriesAnotherIdentifierWhenTheGeneratedOneIsTaken(t *testing.T) {
	pool := database(t)
	owner := workspace(t, pool, "", "")
	draft := link.Draft{Destination: "https://example.com/"}

	first, err := link.NewStore(pool, redirectDomain, &sequence{[]string{"bbbbbbbb"}}).Create(t.Context(), owner, draft)
	if err != nil {
		t.Fatal(err)
	}
	// The generator repeats the taken identifier twice before a free one.
	second, err := link.NewStore(pool, redirectDomain, &sequence{[]string{"bbbbbbbb", "bbbbbbbb", "cccccccc"}}).Create(t.Context(), owner, draft)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identifier != "bbbbbbbb" || second.Identifier != "cccccccc" {
		t.Errorf("identifiers = %q and %q, want bbbbbbbb and cccccccc", first.Identifier, second.Identifier)
	}

	// A generator that only produces the taken identifier is broken, and the
	// creation says so after five attempts instead of trying for ever.
	stuck := &sequence{slices.Repeat([]string{"bbbbbbbb"}, 10)}
	if _, err := link.NewStore(pool, redirectDomain, stuck).Create(t.Context(), owner, draft); !errors.Is(err, link.ErrNoFreeIdentifier) {
		t.Errorf("err = %v, want %v", err, link.ErrNoFreeIdentifier)
	}
	if left := len(stuck.identifiers); left != 5 {
		t.Errorf("the generator was asked %d times, want 5", 10-left)
	}
}

func TestAnAliasNeedsThePermissionAndMustBeFree(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	plain := workspace(t, pool, "", "")
	trusted := workspace(t, pool, ", may_choose_aliases", ", true")
	draft := link.Draft{Alias: "black-friday", Destination: "https://example.com/"}

	if _, err := store.Create(t.Context(), plain, draft); !errors.Is(err, link.ErrAliasNotPermitted) {
		t.Errorf("without the permission: err = %v, want %v", err, link.ErrAliasNotPermitted)
	}
	created, err := store.Create(t.Context(), trusted, draft)
	if err != nil {
		t.Fatal(err)
	}
	if created.Identifier != "black-friday" {
		t.Errorf("identifier = %q, want the alias", created.Identifier)
	}
	if _, err := store.Create(t.Context(), trusted, draft); !errors.Is(err, link.ErrAliasTaken) {
		t.Errorf("a second time: err = %v, want %v", err, link.ErrAliasTaken)
	}
	reserved := link.Draft{Alias: "login", Destination: "https://example.com/"}
	if _, err := store.Create(t.Context(), trusted, reserved); !errors.Is(err, link.ErrAliasReserved) {
		t.Errorf("a reserved word: err = %v, want %v", err, link.ErrAliasReserved)
	}
}

func TestCreateAppliesThePolicyOfTheWorkspace(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	demo := workspace(t, pool, ", allowed_destinations",
		`, '[{"scheme": "https", "host": "example.com", "path": "/docs"}]'`)

	cases := []struct {
		name        string
		destination string
		want        error
	}{
		{"inside the allowlist", "https://example.com/docs/guide", nil},
		{"outside the allowlist", "https://example.com/admin", link.ErrDestinationNotAllowed},
		{"a climb out of the allowlist", "https://example.com/docs/../admin", link.ErrDestinationDotSegment},
		{"the redirect domain itself", "https://go.example.dev/k7m2xq9d", link.ErrDestinationRedirects},
		{"a destination the rules refuse", "http://localhost/", link.ErrDestinationHost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := store.Create(t.Context(), demo, link.Draft{Destination: c.destination})
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
	// A campaign that collides with the destination is refused as well.
	_, err := store.Create(t.Context(), demo, link.Draft{
		Destination: "https://example.com/docs?utm_source=x", Campaign: link.Campaign{Source: "y"},
	})
	if !errors.Is(err, link.ErrCampaignConflict) {
		t.Errorf("err = %v, want %v", err, link.ErrCampaignConflict)
	}
}

// Twenty creations at once in a workspace with room for five: exactly five
// succeed. Without the lock on the workspace, several would count four
// links and all would insert.
func TestTheQuotaHoldsUnderConcurrencyAndTombstonesTakeNoRoom(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	var owner string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO workspaces (name, max_links, max_keys) VALUES ('Small', 5, 10) RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}

	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		created   []string
		overQuota int
	)
	for range 20 {
		wait.Go(func() {
			made, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created = append(created, made.ID)
			case errors.Is(err, link.ErrQuotaExceeded):
				overQuota++
			default:
				t.Errorf("an unexpected error: %v", err)
			}
		})
	}
	wait.Wait()
	if len(created) != 5 || overQuota != 15 {
		t.Fatalf("%d created and %d over the quota, want 5 and 15", len(created), overQuota)
	}

	// Deleting one, as the store will, frees its place.
	if _, err := pool.Exec(t.Context(),
		`UPDATE links SET state = 'deleted', destination = NULL, resolved_destination = NULL WHERE id = $1`, created[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"}); err != nil {
		t.Errorf("after a deletion: err = %v, want room for one more", err)
	}
	if _, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"}); !errors.Is(err, link.ErrQuotaExceeded) {
		t.Errorf("and then: err = %v, want %v", err, link.ErrQuotaExceeded)
	}
}

func TestAWorkspaceThatIsBlockedOrMissingCreatesNothing(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	blocked := workspace(t, pool, ", state", ", 'blocked'")
	draft := link.Draft{Destination: "https://example.com/"}

	if _, err := store.Create(t.Context(), blocked, draft); !errors.Is(err, link.ErrWorkspaceBlocked) {
		t.Errorf("a blocked workspace: err = %v, want %v", err, link.ErrWorkspaceBlocked)
	}
	for _, missing := range []string{"00000000-0000-7000-8000-000000000000", "not-an-identifier", ""} {
		if _, err := store.Create(t.Context(), missing, draft); !errors.Is(err, link.ErrWorkspaceNotFound) {
			t.Errorf("workspace %q: err = %v, want %v", missing, err, link.ErrWorkspaceNotFound)
		}
	}
	var links int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM links`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Errorf("%d links were stored, want none", links)
	}
}

// A link of another workspace, a deleted one and one that never existed
// give the same answer.
func TestGetOnlySeesTheLiveLinksOfItsWorkspace(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner, stranger := workspace(t, pool, "", ""), workspace(t, pool, "", "")
	mine, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE links SET state = 'deleted', destination = NULL, resolved_destination = NULL WHERE id = $1`, deleted.ID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		workspace string
		id        string
	}{
		{"the link of another workspace", stranger, mine.ID},
		{"a deleted link", owner, deleted.ID},
		{"a link that never existed", owner, "00000000-0000-7000-8000-000000000000"},
		{"something that is not an identifier", owner, "k7m2xq9d"},
		{"a workspace that is not an identifier", "acme", mine.ID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := store.Get(t.Context(), c.workspace, c.id); !errors.Is(err, link.ErrNotFound) {
				t.Errorf("err = %v, want %v", err, link.ErrNotFound)
			}
		})
	}
	if _, err := store.Get(t.Context(), owner, mine.ID); err != nil {
		t.Errorf("the owner cannot read its own link: %v", err)
	}
}

func TestListPagesInTheOrderOfCreationWithoutRepeatingOrSkipping(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner, stranger := workspace(t, pool, "", ""), workspace(t, pool, "", "")
	create := func(workspace string) string {
		t.Helper()
		made, err := store.Create(t.Context(), workspace, link.Draft{Destination: "https://example.com/"})
		if err != nil {
			t.Fatal(err)
		}
		return made.ID
	}
	var want []string
	for range 7 {
		want = append(want, create(owner))
		create(stranger)
	}

	var (
		got   []string
		pages []int
	)
	after := ""
	for page := 1; ; page++ {
		links, more, err := store.List(t.Context(), owner, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			if l.WorkspaceID != owner {
				t.Fatalf("page %d holds a link of another workspace", page)
			}
			got = append(got, l.ID)
		}
		pages = append(pages, len(links))
		if !more {
			break
		}
		after = links[len(links)-1].ID
		if page == 1 {
			// A link created between two pages appears once, at the end.
			want = append(want, create(owner))
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("listed %v\nwant   %v", got, want)
	}
	// Eight links in pages of four: the second page is the last and says
	// so, with no empty page after it.
	if !slices.Equal(pages, []int{4, 4}) {
		t.Errorf("the pages held %v links, want 4 and 4", pages)
	}

	for _, limit := range []int{0, -1, link.MaxPage + 1} {
		if _, _, err := store.List(t.Context(), owner, "", limit); !errors.Is(err, link.ErrPage) {
			t.Errorf("limit %d: err = %v, want %v", limit, err, link.ErrPage)
		}
	}
	if _, _, err := store.List(t.Context(), owner, "not-an-identifier", 3); !errors.Is(err, link.ErrPage) {
		t.Errorf("a start that is not an identifier: err = %v, want %v", err, link.ErrPage)
	}
	if full, more, err := store.List(t.Context(), owner, "", link.MaxPage); err != nil || len(full) != 8 || more {
		t.Errorf("the largest page: %d links, more %v, %v; want 8 and no more", len(full), more, err)
	}
	if none, more, err := store.List(t.Context(), owner, want[len(want)-1], 4); err != nil || len(none) != 0 || more {
		t.Errorf("after the last link: %d links, more %v, %v; want none", len(none), more, err)
	}
}

func TestCreateBoundsWhatDescribesALink(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	// A character of three bytes, so that the limits are seen to count
	// characters.
	wide := string(rune(0x4e2d))
	many := make([]string, link.MaxTags+1)
	for i := range many {
		many[i] = "tag-" + strconv.Itoa(i)
	}

	cases := []struct {
		name  string
		draft link.Draft
		want  error
	}{
		{"a title of 201 characters", link.Draft{Title: strings.Repeat(wide, link.MaxTitleLength+1)}, link.ErrTitle},
		{"a title with a line break", link.Draft{Title: "Launch\nweek"}, link.ErrTitle},
		{"a title that is not text", link.Draft{Title: string([]byte{'a', 0xff})}, link.ErrTitle},
		{"21 tags", link.Draft{Tags: many}, link.ErrTags},
		{"an empty tag", link.Draft{Tags: []string{"q4", ""}}, link.ErrTags},
		{"a tag of 41 characters", link.Draft{Tags: []string{strings.Repeat(wide, link.MaxTagLength+1)}}, link.ErrTags},
		{"a tag with a tab", link.Draft{Tags: []string{"q4\temail"}}, link.ErrTags},
		{"a tag twice", link.Draft{Tags: []string{"q4", "email", "q4"}}, link.ErrTags},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.draft.Destination = "https://example.com/"
			if created, err := store.Create(t.Context(), owner, c.draft); !errors.Is(err, c.want) || created.ID != "" {
				t.Errorf("err = %v with the link %q, want %v and none", err, created.ID, c.want)
			}
		})
	}
	if links, _, err := store.List(t.Context(), owner, "", link.MaxPage); err != nil || len(links) != 0 {
		t.Errorf("the refused creations stored %d links, %v", len(links), err)
	}

	widest := link.Draft{
		Destination: "https://example.com/",
		Title:       strings.Repeat(wide, link.MaxTitleLength),
		Tags:        append(many[:link.MaxTags-1:link.MaxTags-1], strings.Repeat(wide, link.MaxTagLength)),
	}
	if _, err := store.Create(t.Context(), owner, widest); err != nil {
		t.Errorf("the longest title and the most tags: err = %v, want none", err)
	}
}

// What the replica of the redirect reads: the links of the domain, live or
// changed since a moment, with the time of the database.
func TestServedReadsWhatTheRedirectNeeds(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	create := func(from *link.Store) link.Link {
		t.Helper()
		created, err := from.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/a", Campaign: link.Campaign{Source: "newsletter"}})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	live, gone := create(store), create(store)
	elsewhere := create(link.NewStore(pool, "other.example.dev", rand.Reader))
	if err := store.Delete(t.Context(), owner, gone.ID); err != nil {
		t.Fatal(err)
	}
	var databaseNow time.Time
	if err := pool.QueryRow(t.Context(), `SELECT now()`).Scan(&databaseNow); err != nil {
		t.Fatal(err)
	}

	all, at, err := store.Served(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := link.Served{ID: live.ID, WorkspaceID: owner, Identifier: live.Identifier, State: link.StateActive,
		Resolved: "https://example.com/a?utm_source=newsletter", Version: 1}
	if len(all) != 1 || all[0] != want {
		t.Errorf("Served = %+v, want only %+v", all, want)
	}
	if at.Before(databaseNow) || at.Sub(databaseNow) > time.Minute {
		t.Errorf("Served says it read at %v, and the database said %v just before", at, databaseNow)
	}

	// Since a moment: the deleted link too, as a tombstone, and nothing of
	// another domain or older than the moment.
	changed, _, err := store.ServedSince(t.Context(), gone.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].ID != gone.ID || changed[0].State != link.StateDeleted || changed[0].Resolved != "" || changed[0].Version != 2 {
		t.Errorf("ServedSince the second link = %+v, want its tombstone at version 2", changed)
	}
	everything, _, err := store.ServedSince(t.Context(), live.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(everything) != 2 {
		t.Errorf("ServedSince the first link = %d links, want 2", len(everything))
	}
	for _, served := range everything {
		if served.ID == elsewhere.ID {
			t.Error("a link of another domain was read")
		}
	}
	if none, _, err := store.ServedSince(t.Context(), databaseNow.Add(time.Hour)); err != nil || len(none) != 0 {
		t.Errorf("ServedSince a moment that has not come = %d links, %v", len(none), err)
	}
}
