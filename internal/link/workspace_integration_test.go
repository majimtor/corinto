//go:build integration

package link_test

import (
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

func TestCreateWorkspaceStoresWhatItIsGivenAndLockWorkspaceReadsItBack(t *testing.T) {
	pool := database(t)
	draft := link.WorkspaceDraft{
		Name: "Acme", Demo: true, MayChooseAliases: true,
		Allowed:  []string{"https://docs.example.com/guides"},
		MaxLinks: 7, MaxKeys: 2,
	}
	created, err := link.CreateWorkspace(t.Context(), pool, draft)
	if err != nil {
		t.Fatal(err)
	}
	want := link.Workspace{
		ID: created.ID, Name: "Acme", Demo: true, MayChooseAliases: true,
		Allowed:  []link.AllowedPrefix{{Scheme: "https", Host: "docs.example.com", Path: "/guides"}},
		MaxLinks: 7, MaxKeys: 2,
	}
	equal := func(a, b link.Workspace) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Demo == b.Demo && a.MayChooseAliases == b.MayChooseAliases &&
			slices.Equal(a.Allowed, b.Allowed) && a.MaxLinks == b.MaxLinks && a.MaxKeys == b.MaxKeys
	}
	if created.ID == "" || !equal(created, want) {
		t.Errorf("CreateWorkspace returned %+v, want %+v", created, want)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	stored, err := link.LockWorkspace(t.Context(), tx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(stored, want) {
		t.Errorf("the workspace stored is %+v, want %+v", stored, want)
	}
}

// An entry of the allowlist is stored in the form in which a destination is
// compared with it. Otherwise an entry written with another case or with
// the default port would match nothing.
func TestTheAllowlistIsStoredAsDestinationsAreCompared(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	created, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{
		Name:     "Acme",
		Allowed:  []string{"HTTPS://Docs.Example.COM:443/guides", "http://example.org:8080"},
		MaxLinks: 10, MaxKeys: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []link.AllowedPrefix{
		{Scheme: "https", Host: "docs.example.com", Path: "/guides"},
		{Scheme: "http", Host: "example.org:8080", Path: "/"},
	}
	if !slices.Equal(created.Allowed, want) {
		t.Errorf("the allowlist is %+v, want %+v", created.Allowed, want)
	}

	for _, inside := range []string{"https://docs.example.com/guides/start", "http://example.org:8080/any"} {
		if _, err := store.Create(t.Context(), created.ID, link.Draft{Destination: inside}); err != nil {
			t.Errorf("%s: err = %v, want none", inside, err)
		}
	}
	for _, outside := range []string{"https://docs.example.com/pricing", "http://example.org/any"} {
		_, err := store.Create(t.Context(), created.ID, link.Draft{Destination: outside})
		if !errors.Is(err, link.ErrDestinationNotAllowed) {
			t.Errorf("%s: err = %v, want %v", outside, err, link.ErrDestinationNotAllowed)
		}
	}
}

func TestAWorkspaceWithoutAPolicyAllowsAnyDestination(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	created, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: "Acme", MaxLinks: 10, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	if created.Allowed == nil || len(created.Allowed) != 0 || created.Demo || created.MayChooseAliases {
		t.Errorf("the workspace is %+v, want an empty allowlist and no permission", created)
	}
	if _, err := store.Create(t.Context(), created.ID, link.Draft{Destination: "https://example.com/"}); err != nil {
		t.Errorf("err = %v, want none", err)
	}
}

func TestCreateWorkspaceRefusesWhatCannotBeAWorkspace(t *testing.T) {
	pool := database(t)
	// A character of three bytes, so that the limit is seen to count
	// characters.
	wide := string(rune(0x4e2d))
	cases := []struct {
		name  string
		draft link.WorkspaceDraft
		want  error
	}{
		{"no name", link.WorkspaceDraft{}, link.ErrWorkspaceName},
		{"a name of 101 characters", link.WorkspaceDraft{Name: strings.Repeat(wide, 101)}, link.ErrWorkspaceName},
		{"a name with a line break", link.WorkspaceDraft{Name: "Acme\nInc"}, link.ErrWorkspaceName},
		{"a name that is not text", link.WorkspaceDraft{Name: string([]byte{'a', 0xff})}, link.ErrWorkspaceName},
		{"a negative quota of links", link.WorkspaceDraft{Name: "Acme", MaxLinks: -1}, link.ErrWorkspaceQuota},
		{"a negative quota of keys", link.WorkspaceDraft{Name: "Acme", MaxKeys: -1}, link.ErrWorkspaceQuota},
		{"an entry with a query", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"https://example.com/docs?page=1"}}, link.ErrAllowedPrefix},
		{"an entry with a fragment", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"https://example.com/docs#top"}}, link.ErrAllowedPrefix},
		{"an entry without a scheme", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"example.com/docs"}}, link.ErrDestinationScheme},
		{"an entry that is an address", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"http://192.0.2.1/"}}, link.ErrDestinationHost},
		{"an entry with a dot segment", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"https://example.com/docs/../admin"}}, link.ErrDestinationDotSegment},
		{"a good entry and a bad one", link.WorkspaceDraft{Name: "Acme", Allowed: []string{"https://example.com/", "ftp://example.com/"}}, link.ErrDestinationScheme},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			created, err := link.CreateWorkspace(t.Context(), pool, c.draft)
			if !errors.Is(err, c.want) || created.ID != "" {
				t.Errorf("err = %v with the workspace %q, want %v and none", err, created.ID, c.want)
			}
		})
	}
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workspaces`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("the refused creations stored %d workspaces", stored)
	}

	t.Run("a name of 100 characters and quotas of zero are a workspace", func(t *testing.T) {
		if _, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: strings.Repeat(wide, 100)}); err != nil {
			t.Errorf("err = %v, want none", err)
		}
	})
}

// A workspace is read to be shown, without holding it, and only while it
// is not blocked.
func TestAWorkspaceIsReadUnlessItIsBlockedOrIsNotThere(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	created, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{
		Name: "Acme", Demo: true, MayChooseAliases: true, Allowed: []string{"https://docs.example.com/guides"}, MaxLinks: 7, MaxKeys: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.Workspace(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != created.ID || read.Name != "Acme" || !read.Demo || !read.MayChooseAliases || !slices.Equal(read.Allowed, created.Allowed) ||
		read.MaxLinks != 7 || read.MaxKeys != 2 {
		t.Errorf("the workspace read is %+v, want %+v", read, created)
	}
	// Reading holds nothing: the workspace is locked while it is read
	// again, and the reading does not wait.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := link.LockWorkspace(t.Context(), tx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Workspace(t.Context(), created.ID); err != nil {
		t.Errorf("a workspace that is locked is read with %v", err)
	}
	if _, _, err := link.BlockWorkspace(t.Context(), tx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	for what, c := range map[string]struct {
		id   string
		want error
	}{
		"a blocked workspace":                 {created.ID, link.ErrWorkspaceBlocked},
		"a workspace that does not exist":     {"00000000-0000-7000-8000-000000000000", link.ErrWorkspaceNotFound},
		"something that is not an identifier": {"acme", link.ErrWorkspaceNotFound},
	} {
		if read, err := store.Workspace(t.Context(), c.id); !errors.Is(err, c.want) || read.ID != "" {
			t.Errorf("%s: err = %v with the workspace %q, want %v and none", what, err, read.ID, c.want)
		}
	}
}

// The demo workspaces that can be entered: the ones that are not blocked,
// the oldest first, and no workspace that is not a demo.
func TestTheDemosAreTheDemoWorkspacesThatAreNotBlocked(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	if demos, err := store.Demos(t.Context()); err != nil || len(demos) != 0 {
		t.Fatalf("a database without workspaces has %d demos (%v)", len(demos), err)
	}
	create := func(name string, demo bool) link.Workspace {
		t.Helper()
		created, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: name, Demo: demo, MaxLinks: 100, MaxKeys: 10})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	first, _, blocked, second := create("Acme", true), create("Harbor Bakery", false), create("Summit Outdoors", true), create("Globex", true)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, _, err := link.BlockWorkspace(t.Context(), tx, blocked.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	demos, err := store.Demos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(demos) != 2 || demos[0].ID != first.ID || demos[0].Name != "Acme" || !demos[0].Demo || demos[0].MaxLinks != 100 ||
		demos[1].ID != second.ID || demos[1].Name != "Globex" {
		t.Errorf("the demos are %+v, want %s and %s in that order", demos, first.Name, second.Name)
	}
}
