//go:build integration

package link_test

import (
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// pointer returns the address of a value, for the optional fields of a
// change.
func pointer[T any](value T) *T { return &value }

func TestUpdateChangesWhatItIsGivenAndNothingElse(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	created, err := store.Create(t.Context(), owner, link.Draft{
		Destination: "https://example.com/a",
		Campaign:    link.Campaign{Source: "newsletter"},
		Title:       "First",
		Tags:        []string{"q4"},
		ExpiresAt:   &expiry,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A new destination keeps the campaign, and what is served follows.
	moved, err := store.Update(t.Context(), owner, created.ID, link.Change{Destination: pointer("HTTPS://Example.com/b")})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Destination != "https://example.com/b" || moved.Resolved != "https://example.com/b?utm_source=newsletter" {
		t.Errorf("destination = %q, resolved = %q", moved.Destination, moved.Resolved)
	}
	if moved.Version != 2 || !moved.UpdatedAt.After(created.UpdatedAt) || !moved.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("version = %d, updated %v, created %v", moved.Version, moved.UpdatedAt, moved.CreatedAt)
	}
	if moved.Identifier != created.Identifier || moved.Title != "First" || !slices.Equal(moved.Tags, []string{"q4"}) || moved.ExpiresAt == nil {
		t.Errorf("something else changed: %+v", moved)
	}

	// A new campaign keeps the destination.
	retagged, err := store.Update(t.Context(), owner, created.ID,
		link.Change{Campaign: link.CampaignChange{Source: pointer(""), Medium: pointer("email")}})
	if err != nil {
		t.Fatal(err)
	}
	if retagged.Resolved != "https://example.com/b?utm_medium=email" || retagged.Campaign.Source != "" || retagged.Version != 3 {
		t.Errorf("resolved = %q, campaign = %+v, version = %d", retagged.Resolved, retagged.Campaign, retagged.Version)
	}

	// A change that names one parameter leaves the others as they are.
	termed, err := store.Update(t.Context(), owner, created.ID, link.Change{Campaign: link.CampaignChange{Term: pointer("spring")}})
	if err != nil {
		t.Fatal(err)
	}
	if termed.Resolved != "https://example.com/b?utm_medium=email&utm_term=spring" || termed.Campaign.Medium != "email" {
		t.Errorf("resolved = %q, campaign = %+v", termed.Resolved, termed.Campaign)
	}
	if _, err := store.Update(t.Context(), owner, created.ID, link.Change{Campaign: link.CampaignChange{Term: pointer("")}}); err != nil {
		t.Fatal(err)
	}

	// The rest, at once: a title, no tags and no expiry.
	plain, err := store.Update(t.Context(), owner, created.ID, link.Change{
		Title: pointer(""), Tags: pointer([]string(nil)), NoExpiry: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Title != "" || len(plain.Tags) != 0 || plain.ExpiresAt != nil || plain.Version != 6 {
		t.Errorf("title = %q, tags = %v, expiry = %v, version = %d", plain.Title, plain.Tags, plain.ExpiresAt, plain.Version)
	}
	read, err := store.Get(t.Context(), owner, created.ID)
	if err != nil || read.Version != 6 || read.Resolved != retagged.Resolved {
		t.Errorf("what is read is not the last change: %+v, %v", read, err)
	}
}

// The replica applies a change when its version is newer. A request that
// changes nothing must not look like one that does.
func TestAnUpdateThatChangesNothingIsNotAChange(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	created, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/a", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]link.Change{
		"nothing at all":       {},
		"the same destination": {Destination: pointer("https://EXAMPLE.com/a")},
		"the same title":       {Title: pointer("First")},
		"no expiry, as before": {NoExpiry: true},
	} {
		same, err := store.Update(t.Context(), owner, created.ID, change)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if same.Version != 1 || !same.UpdatedAt.Equal(created.UpdatedAt) {
			t.Errorf("%s: version = %d, want 1 and the same time of change", name, same.Version)
		}
	}
}

func TestAnUpdateThatIsRefusedLeavesTheLinkAsItWas(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	demo := workspace(t, pool, ", allowed_destinations",
		`, '[{"scheme": "https", "host": "example.com", "path": "/docs"}]'`)
	created, err := store.Create(t.Context(), demo, link.Draft{Destination: "https://example.com/docs/a"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change link.Change
		want   error
	}{
		{"outside the allowlist", link.Change{Destination: pointer("https://example.com/admin")}, link.ErrDestinationNotAllowed},
		{"a destination the rules refuse", link.Change{Destination: pointer("http://localhost/")}, link.ErrDestinationHost},
		{"a campaign value over the limit", link.Change{Campaign: link.CampaignChange{Name: pointer(string(make([]byte, 201)))}}, link.ErrCampaignValue},
		{"a title over the limit", link.Change{Title: pointer(strings.Repeat("a", link.MaxTitleLength+1))}, link.ErrTitle},
		{"a tag twice", link.Change{Tags: pointer([]string{"q4", "q4"})}, link.ErrTags},
		{
			"a title with a destination that is refused",
			link.Change{Title: pointer("New"), Destination: pointer("https://go.example.dev/x")},
			link.ErrDestinationRedirects,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := store.Update(t.Context(), demo, created.ID, c.change); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
	read, err := store.Get(t.Context(), demo, created.ID)
	if err != nil || read.Version != 1 || read.Destination != "https://example.com/docs/a" || read.Title != "" {
		t.Errorf("the link changed: %+v, %v", read, err)
	}
}

func TestDisableAndEnableAreReversibleAndRepeatable(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	created, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name    string
		act     func() (link.Link, error)
		state   link.State
		version int64
	}{
		{"disable", func() (link.Link, error) { return store.Disable(t.Context(), owner, created.ID) }, link.StateDisabled, 2},
		{"disable again", func() (link.Link, error) { return store.Disable(t.Context(), owner, created.ID) }, link.StateDisabled, 2},
		{"enable", func() (link.Link, error) { return store.Enable(t.Context(), owner, created.ID) }, link.StateActive, 3},
		{"enable again", func() (link.Link, error) { return store.Enable(t.Context(), owner, created.ID) }, link.StateActive, 3},
	}
	for _, step := range steps {
		got, err := step.act()
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got.State != step.state || got.Version != step.version || got.Resolved != created.Resolved {
			t.Errorf("%s: state = %s, version = %d, want %s and %d", step.name, got.State, got.Version, step.state, step.version)
		}
	}
}

func TestABlockedLinkCanOnlyBeReadAndDeleted(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")
	created, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Block(t.Context(), created.Identifier); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(t.Context(), owner, created.ID, link.Change{Destination: pointer("https://example.com/other")}); !errors.Is(err, link.ErrLinkBlocked) {
		t.Errorf("update: err = %v, want %v", err, link.ErrLinkBlocked)
	}
	if _, err := store.Enable(t.Context(), owner, created.ID); !errors.Is(err, link.ErrLinkBlocked) {
		t.Errorf("enable: err = %v, want %v", err, link.ErrLinkBlocked)
	}
	if _, err := store.Disable(t.Context(), owner, created.ID); !errors.Is(err, link.ErrLinkBlocked) {
		t.Errorf("disable: err = %v, want %v", err, link.ErrLinkBlocked)
	}
	read, err := store.Get(t.Context(), owner, created.ID)
	if err != nil || read.State != link.StateBlocked || read.Destination != "https://example.com/" {
		t.Errorf("read: %+v, %v; want it blocked and untouched", read, err)
	}
	if err := store.Delete(t.Context(), owner, created.ID); err != nil {
		t.Errorf("delete: err = %v, want none", err)
	}
}

func TestDeleteLeavesATombstoneThatKeepsTheIdentifier(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, ", may_choose_aliases", ", true")
	created, err := store.Create(t.Context(), owner, link.Draft{
		Alias: "black-friday", Destination: "https://example.com/", Title: "Sale",
		Campaign: link.Campaign{Source: "newsletter"}, Tags: []string{"q4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), owner, created.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get(t.Context(), owner, created.ID); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("read after delete: err = %v, want %v", err, link.ErrNotFound)
	}
	if links, _, err := store.List(t.Context(), owner, "", link.MaxPage); err != nil || len(links) != 0 {
		t.Errorf("list after delete: %d links, %v; want none", len(links), err)
	}
	if err := store.Delete(t.Context(), owner, created.ID); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("a second delete: err = %v, want %v", err, link.ErrNotFound)
	}
	for name, act := range map[string]func() (link.Link, error){
		"update": func() (link.Link, error) {
			return store.Update(t.Context(), owner, created.ID, link.Change{Title: pointer("x")})
		},
		"enable": func() (link.Link, error) { return store.Enable(t.Context(), owner, created.ID) },
	} {
		if _, err := act(); !errors.Is(err, link.ErrNotFound) {
			t.Errorf("%s after delete: err = %v, want %v", name, err, link.ErrNotFound)
		}
	}

	// What stays is the identifier and nothing of what the link was.
	var (
		state       string
		version     int64
		erased      bool
		destination *string
	)
	err = pool.QueryRow(t.Context(),
		`SELECT state, version, destination,
		        resolved_destination IS NULL AND title IS NULL AND utm_source IS NULL AND tags = '{}'
		   FROM links WHERE domain = $1 AND identifier = 'black-friday'`, redirectDomain,
	).Scan(&state, &version, &destination, &erased)
	if err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || version != 2 || destination != nil || !erased {
		t.Errorf("tombstone: state = %s, version = %d, destination = %v, erased = %v", state, version, destination, erased)
	}
	// The identifier is never issued again, not even to the workspace that had it.
	_, err = store.Create(t.Context(), owner, link.Draft{Alias: "black-friday", Destination: "https://example.com/"})
	if !errors.Is(err, link.ErrAliasTaken) {
		t.Errorf("the alias of a deleted link: err = %v, want %v", err, link.ErrAliasTaken)
	}
}

func TestNoChangeCrossesWorkspaces(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner, stranger := workspace(t, pool, "", ""), workspace(t, pool, "", "")
	created, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(t.Context(), stranger, created.ID, link.Change{Title: pointer("mine now")}); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("update: err = %v, want %v", err, link.ErrNotFound)
	}
	if _, err := store.Disable(t.Context(), stranger, created.ID); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("disable: err = %v, want %v", err, link.ErrNotFound)
	}
	if err := store.Delete(t.Context(), stranger, created.ID); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("delete: err = %v, want %v", err, link.ErrNotFound)
	}
	read, err := store.Get(t.Context(), owner, created.ID)
	if err != nil || read.Version != 1 || read.State != link.StateActive || read.Title != "" {
		t.Errorf("the link of the owner changed: %+v, %v", read, err)
	}
}

// Two changes to different attributes of one link, at the same time, must
// both stay. Each is computed from the row and writes the whole row back, so
// without the lock the second to write would restore what the first changed.
func TestTwoChangesAtOnceBothStay(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")

	var ids []string
	for range 25 {
		created, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}
	var wait sync.WaitGroup
	for _, id := range ids {
		wait.Go(func() {
			if _, err := store.Update(t.Context(), owner, id, link.Change{Title: pointer("Titled")}); err != nil {
				t.Errorf("title: %v", err)
			}
		})
		wait.Go(func() {
			if _, err := store.Update(t.Context(), owner, id, link.Change{Tags: pointer([]string{"tagged"})}); err != nil {
				t.Errorf("tags: %v", err)
			}
		})
	}
	wait.Wait()

	for _, id := range ids {
		read, err := store.Get(t.Context(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if read.Title != "Titled" || !slices.Equal(read.Tags, []string{"tagged"}) || read.Version != 3 {
			t.Errorf("link %s: title = %q, tags = %v, version = %d; want both changes and version 3", id, read.Title, read.Tags, read.Version)
		}
	}
}
