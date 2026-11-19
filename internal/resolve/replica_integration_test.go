//go:build integration

package resolve

import (
	"bytes"
	"crypto/rand"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// The host that serves the short links of these tests.
const redirectDomain = "go.example.dev"

// stage is a replica that reads from a real database, with what a test
// needs to change the links behind it.
type stage struct {
	pool    *pgxpool.Pool
	store   *link.Store
	replica *Replica
	owner   string
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
	owner, err := link.CreateWorkspace(t.Context(), pool, link.WorkspaceDraft{Name: "Acme", MayChooseAliases: true, MaxLinks: 1000, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	return &stage{
		pool: pool, store: store, owner: owner.ID,
		replica: New(store, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))),
	}
}

// create stores a link of the workspace of the stage.
func (s *stage) create(t *testing.T, draft link.Draft) link.Link {
	t.Helper()
	if draft.Destination == "" {
		draft.Destination = "https://example.com/"
	}
	created, err := s.store.Create(t.Context(), s.owner, draft)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// poll reads the changes, which must be readable.
func (s *stage) poll(t *testing.T) {
	t.Helper()
	if err := s.replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTheReplicaLoadsTheLinksOfItsDomainAsTheRedirectServesThem(t *testing.T) {
	s := newStage(t)
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	plain := s.create(t, link.Draft{Destination: "https://example.com/a", Campaign: link.Campaign{Source: "newsletter"}, ExpiresAt: &expiry})
	disabled := s.create(t, link.Draft{Alias: "spring-sale"})
	if _, err := s.store.Disable(t.Context(), s.owner, disabled.ID); err != nil {
		t.Fatal(err)
	}
	deleted := s.create(t, link.Draft{})
	if err := s.store.Delete(t.Context(), s.owner, deleted.ID); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := link.NewStore(s.pool, "other.example.dev", rand.Reader).Create(t.Context(), s.owner, link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	found, held := s.replica.Lookup(plain.Identifier)
	want := Link{ID: plain.ID, WorkspaceID: s.owner, Destination: "https://example.com/a?utm_source=newsletter",
		State: link.StateActive, ExpiresAt: found.ExpiresAt, Version: 1}
	if !held || found != want || !found.ExpiresAt.Equal(expiry) {
		t.Errorf("the link is held as %+v, %v; want %+v expiring at %v", found, held, want, expiry)
	}
	if switched, held := s.replica.Lookup("spring-sale"); !held || switched.State != link.StateDisabled || switched.Version != 2 {
		t.Errorf("the disabled link is held as %+v, %v", switched, held)
	}
	for name, identifier := range map[string]string{"a deleted link": deleted.Identifier, "a link of another domain": elsewhere.Identifier} {
		if found, held := s.replica.Lookup(identifier); held {
			t.Errorf("%s is in the copy: %+v", name, found)
		}
	}
	if s.replica.Len() != 2 {
		t.Errorf("the copy holds %d links, want 2", s.replica.Len())
	}
}

func TestEveryChangeToALinkReachesTheReplicaWithTheNextPoll(t *testing.T) {
	s := newStage(t)
	target := s.create(t, link.Draft{Destination: "https://example.com/a"})
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	held := func() Link {
		t.Helper()
		found, _ := s.replica.Lookup(target.Identifier)
		return found
	}

	// A link created after the load.
	later := s.create(t, link.Draft{})
	s.poll(t)
	if found, there := s.replica.Lookup(later.Identifier); !there || !found.Redirects(time.Now()) {
		t.Errorf("a link created after the load is %+v, %v", found, there)
	}

	destination := "https://example.com/b"
	if _, err := s.store.Update(t.Context(), s.owner, target.ID, link.Change{Destination: &destination}); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found := held(); found.Destination != destination || found.Version != 2 {
		t.Errorf("after a new destination the link is held as %+v", found)
	}

	if _, err := s.store.Disable(t.Context(), s.owner, target.ID); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found := held(); found.State != link.StateDisabled || found.Redirects(time.Now()) {
		t.Errorf("after disabling the link is held as %+v", found)
	}

	if _, err := s.store.Enable(t.Context(), s.owner, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Block(t.Context(), target.Identifier); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found := held(); found.State != link.StateBlocked || found.Version != 5 {
		t.Errorf("after enabling and blocking the link is held as %+v", found)
	}

	if err := s.store.Delete(t.Context(), s.owner, target.ID); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found, there := s.replica.Lookup(target.Identifier); there {
		t.Errorf("after deleting the link is still held: %+v", found)
	}

	// A workspace blocked with its links.
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := link.BlockWorkspace(t.Context(), tx, s.owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found, _ := s.replica.Lookup(later.Identifier); found.State != link.StateBlocked {
		t.Errorf("after its workspace was blocked the link is held as %+v", found)
	}
}

// A change carries the time at which its transaction began. One that is
// committed after a poll is older than what that poll already read, and
// asking only for what is newer than the last reading would never bring
// it. The overlap does.
func TestAChangeCommittedAfterAPollStillArrives(t *testing.T) {
	s := newStage(t)
	target := s.create(t, link.Draft{})
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The transaction begins, changes the link and stays open.
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(),
		`UPDATE links SET state = 'disabled', version = version + 1, updated_at = now() WHERE id = $1`, target.ID); err != nil {
		t.Fatal(err)
	}

	// A poll in between sees nothing of it, and moves its mark past the
	// time the change carries.
	s.poll(t)
	if found, _ := s.replica.Lookup(target.Identifier); found.State != link.StateActive {
		t.Fatalf("the replica read a change that was not committed: %+v", found)
	}

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found, _ := s.replica.Lookup(target.Identifier); found.State != link.StateDisabled {
		t.Errorf("the change committed after a poll did not arrive: the link is held as %+v", found)
	}
}

// A transaction that stays open for longer than the overlap is beyond what
// a poll reads again. The reconciliation brings its change.
func TestAChangeOlderThanTheOverlapArrivesWithTheReconciliation(t *testing.T) {
	s := newStage(t)
	target := s.create(t, link.Draft{})
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	// What a transaction that began a minute ago leaves when it commits.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE links SET state = 'disabled', version = version + 1, updated_at = now() - interval '1 minute' WHERE id = $1`,
		target.ID); err != nil {
		t.Fatal(err)
	}
	s.poll(t)
	if found, _ := s.replica.Lookup(target.Identifier); found.State != link.StateActive {
		t.Fatalf("a poll read a change older than its overlap: %+v", found)
	}
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if found, _ := s.replica.Lookup(target.Identifier); found.State != link.StateDisabled {
		t.Errorf("the reconciliation did not bring the change: the link is held as %+v", found)
	}
}

// With the database out of reach the replica answers from what it has.
func TestTheReplicaAnswersWithTheDatabaseOutOfReach(t *testing.T) {
	s := newStage(t)
	target := s.create(t, link.Draft{Destination: "https://example.com/a"})
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	moved := &clock{now: time.Now()}
	s.replica.now = moved.read
	if err := s.replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	// From here on nothing reaches the database.
	s.pool.Close()
	for range 3 {
		moved.advance(2 * time.Second)
		s.replica.refresh(t.Context())
	}
	if err := s.replica.Poll(t.Context()); err == nil {
		t.Error("a poll reached a database that is out of reach")
	}
	found, held := s.replica.Lookup(target.Identifier)
	if !held || found.Destination != "https://example.com/a" || !found.Redirects(time.Now()) {
		t.Errorf("out of reach of the database the link is held as %+v, %v", found, held)
	}
	if age := s.replica.Age(); age != 6*time.Second {
		t.Errorf("the replica says it is %s old, want 6s", age)
	}
}
