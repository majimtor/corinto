package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// source is where the replica of these tests reads from: what a test put
// there, at the time of the database a test set, failing when a test says
// so. It remembers what it was asked.
type source struct {
	mu      sync.Mutex
	links   map[string]link.Served
	at      time.Time
	failure error
	// asked holds what each reading asked for: "all", or the moment since
	// which changes were wanted.
	asked []string
}

func (s *source) Served(context.Context) ([]link.Served, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "all")
	if s.failure != nil {
		return nil, time.Time{}, s.failure
	}
	var live []link.Served
	for _, row := range s.links {
		if row.State != link.StateDeleted {
			live = append(live, row)
		}
	}
	return live, s.at, nil
}

// ServedSince returns every link whatever the moment: which changes fall in
// a stretch of time is the store's to decide and is tested with it. What
// the replica asks for is in asked.
func (s *source) ServedSince(_ context.Context, since time.Time) ([]link.Served, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, since.Format(time.RFC3339))
	if s.failure != nil {
		return nil, time.Time{}, s.failure
	}
	var all []link.Served
	for _, row := range s.links {
		all = append(all, row)
	}
	return all, s.at, nil
}

// put stores a link in the source, as a change to it does.
func (s *source) put(row link.Served) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[row.Identifier] = row
}

// The time of the database and the time of the process at the start of
// these tests. They differ on purpose: the replica must not mix them.
var (
	databaseTime = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	processTime  = time.Date(2026, 11, 19, 9, 7, 30, 0, time.UTC)
)

// clock is the time of the process, which a test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// loaded returns a replica that has read a source with two links, with the
// source, the clock of the process and the log.
func loaded(t *testing.T) (*Replica, *source, *clock, *bytes.Buffer) {
	t.Helper()
	read := &source{at: databaseTime, links: map[string]link.Served{
		"k7m2xq9d": {ID: "link-1", WorkspaceID: "workspace-1", Identifier: "k7m2xq9d", State: link.StateActive,
			Resolved: "https://example.com/a?utm_source=newsletter", Version: 1},
		"spring-sale": {ID: "link-2", WorkspaceID: "workspace-2", Identifier: "spring-sale", State: link.StateDisabled,
			Resolved: "https://example.com/b", Version: 3},
	}}
	moved := &clock{now: processTime}
	written := &bytes.Buffer{}
	replica := New(read, slog.New(slog.NewJSONHandler(written, nil)))
	replica.now = moved.read
	if err := replica.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return replica, read, moved, written
}

func TestTheReplicaAnswersFromWhatItLoaded(t *testing.T) {
	replica, _, _, _ := loaded(t)

	found, held := replica.Lookup("k7m2xq9d")
	want := Link{ID: "link-1", WorkspaceID: "workspace-1", Destination: "https://example.com/a?utm_source=newsletter",
		State: link.StateActive, Version: 1}
	if !held || found != want {
		t.Errorf("Lookup = %+v, %v; want %+v", found, held, want)
	}
	if disabled, held := replica.Lookup("spring-sale"); !held || disabled.State != link.StateDisabled {
		t.Errorf("a disabled link is %+v, %v; want it there and disabled", disabled, held)
	}
	for _, identifier := range []string{"nothing", "", "K7M2XQ9D", "link-1"} {
		if found, held := replica.Lookup(identifier); held {
			t.Errorf("Lookup(%q) found %+v", identifier, found)
		}
	}
	if replica.Len() != 2 || replica.Age() != 0 {
		t.Errorf("the replica holds %d links and is %s old, want 2 and no age", replica.Len(), replica.Age())
	}
}

func TestALinkRedirectsWhileItIsActiveAndNotExpired(t *testing.T) {
	expiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		link Link
		at   time.Time
		want bool
	}{
		{"active, with no expiry", Link{State: link.StateActive}, databaseTime, true},
		{"active, before its expiry", Link{State: link.StateActive, ExpiresAt: expiry}, expiry.Add(-time.Second), true},
		{"active, at its expiry", Link{State: link.StateActive, ExpiresAt: expiry}, expiry, false},
		{"active, after its expiry", Link{State: link.StateActive, ExpiresAt: expiry}, expiry.Add(time.Second), false},
		{"disabled", Link{State: link.StateDisabled}, databaseTime, false},
		{"blocked", Link{State: link.StateBlocked}, databaseTime, false},
		{"disabled, before its expiry", Link{State: link.StateDisabled, ExpiresAt: expiry}, databaseTime, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.link.Redirects(c.at); got != c.want {
				t.Errorf("Redirects = %v, want %v", got, c.want)
			}
		})
	}
}

// A change is applied when its version is newer than the one the copy
// holds, and only then: every poll reads again what the one before it read.
func TestAPollAppliesAChangeOnceByItsVersion(t *testing.T) {
	replica, read, _, _ := loaded(t)

	// A new destination, a link enabled, a new link.
	read.put(link.Served{ID: "link-1", WorkspaceID: "workspace-1", Identifier: "k7m2xq9d", State: link.StateActive, Resolved: "https://example.com/c", Version: 2})
	read.put(link.Served{ID: "link-2", WorkspaceID: "workspace-2", Identifier: "spring-sale", State: link.StateActive, Resolved: "https://example.com/b", Version: 4})
	read.put(link.Served{ID: "link-3", WorkspaceID: "workspace-1", Identifier: "bcdfghjk", State: link.StateActive, Resolved: "https://example.com/d", Version: 1})
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	for identifier, want := range map[string]Link{
		"k7m2xq9d":    {ID: "link-1", WorkspaceID: "workspace-1", Destination: "https://example.com/c", State: link.StateActive, Version: 2},
		"spring-sale": {ID: "link-2", WorkspaceID: "workspace-2", Destination: "https://example.com/b", State: link.StateActive, Version: 4},
		"bcdfghjk":    {ID: "link-3", WorkspaceID: "workspace-1", Destination: "https://example.com/d", State: link.StateActive, Version: 1},
	} {
		if found, held := replica.Lookup(identifier); !held || found != want {
			t.Errorf("after the poll %s is %+v, %v; want %+v", identifier, found, held, want)
		}
	}

	// The same changes again, and an older state of a link: nothing moves.
	read.put(link.Served{ID: "link-1", WorkspaceID: "workspace-1", Identifier: "k7m2xq9d", State: link.StateDisabled, Resolved: "https://example.com/older", Version: 1})
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if found, _ := replica.Lookup("k7m2xq9d"); found.Destination != "https://example.com/c" || found.Version != 2 {
		t.Errorf("an older version replaced a newer one: %+v", found)
	}

	// A blocked link stays in the copy, as one that does not redirect; a
	// deleted one leaves it.
	read.put(link.Served{ID: "link-3", WorkspaceID: "workspace-1", Identifier: "bcdfghjk", State: link.StateBlocked, Resolved: "https://example.com/d", Version: 2})
	read.put(link.Served{ID: "link-2", WorkspaceID: "workspace-2", Identifier: "spring-sale", State: link.StateDeleted, Version: 5})
	read.put(link.Served{ID: "link-4", WorkspaceID: "workspace-1", Identifier: "never-seen", State: link.StateDeleted, Version: 2})
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if blocked, held := replica.Lookup("bcdfghjk"); !held || blocked.State != link.StateBlocked || blocked.Redirects(databaseTime) {
		t.Errorf("a blocked link is %+v, %v; want it there and not redirecting", blocked, held)
	}
	for _, identifier := range []string{"spring-sale", "never-seen"} {
		if found, held := replica.Lookup(identifier); held {
			t.Errorf("the deleted link %s is still in the copy: %+v", identifier, found)
		}
	}
	// The tombstone is read again by the next polls, and stays out.
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if replica.Len() != 2 {
		t.Errorf("the copy holds %d links, want 2", replica.Len())
	}
}

// What to ask for is decided with the clock of the database alone: each
// poll asks from thirty seconds before the reading that came before it.
func TestAPollReadsAgainThirtySecondsBeforeTheLastReading(t *testing.T) {
	replica, read, moved, _ := loaded(t)

	read.at = databaseTime.Add(2 * time.Second)
	moved.advance(time.Hour)
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	read.at = databaseTime.Add(4 * time.Second)
	if err := replica.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"all",
		databaseTime.Add(-30 * time.Second).Format(time.RFC3339),
		databaseTime.Add(-28 * time.Second).Format(time.RFC3339),
	}
	if strings.Join(read.asked, " ") != strings.Join(want, " ") {
		t.Errorf("the source was asked for %v, want %v", read.asked, want)
	}
}

// With the database away the replica keeps what it has, says how old it is,
// and catches up when the database is back.
func TestTheReplicaKeepsItsLastStateWhenAReadingFails(t *testing.T) {
	replica, read, moved, written := loaded(t)
	read.failure = errors.New("connection refused")
	read.put(link.Served{ID: "link-1", WorkspaceID: "workspace-1", Identifier: "k7m2xq9d", State: link.StateDisabled, Resolved: "https://example.com/a", Version: 2})

	for range 3 {
		moved.advance(2 * time.Second)
		replica.refresh(t.Context())
	}
	if found, held := replica.Lookup("k7m2xq9d"); !held || found.State != link.StateActive || replica.Len() != 2 {
		t.Errorf("after three failed readings the link is %+v, %v", found, held)
	}
	if age := replica.Age(); age != 6*time.Second {
		t.Errorf("the replica says it is %s old, want 6s", age)
	}
	// What it says of itself to whoever operates the process: its age to
	// the millisecond, and the links it holds.
	moved.advance(1234567 * time.Microsecond)
	if status, want := replica.Status(), (Status{AgeSeconds: 7.234, Links: 2}); status != want {
		t.Errorf("the replica says of itself %+v, want %+v", status, want)
	}
	if written, err := json.Marshal(replica.Status()); err != nil || string(written) != `{"age_seconds":7.234,"links":2}` {
		t.Errorf("the status is written as %s (%v)", written, err)
	}
	moved.advance(-1234567 * time.Microsecond)
	if err := replica.Poll(t.Context()); err == nil {
		t.Error("a poll that failed returned no error")
	}

	read.failure = nil
	moved.advance(2 * time.Second)
	replica.refresh(t.Context())
	if found, _ := replica.Lookup("k7m2xq9d"); found.State != link.StateDisabled || replica.Age() != 0 {
		t.Errorf("after the database came back the link is %+v and the replica is %s old", found, replica.Age())
	}
	// The age is counted from the last reading, in part or in whole, that
	// succeeded: this one, not the one that loaded the copy.
	if status := replica.Status(); status != (Status{AgeSeconds: 0, Links: 2}) {
		t.Errorf("after the database came back the replica says of itself %+v", status)
	}
	// The polls after the first one ask again from where the last good
	// reading was: nothing that happened during the outage is skipped.
	if last := read.asked[len(read.asked)-1]; last != databaseTime.Add(-30*time.Second).Format(time.RFC3339) {
		t.Errorf("the first poll after the outage asked from %s", last)
	}

	// An outage is two lines in the log, not one for every poll.
	log := written.String()
	if strings.Count(log, "could not be refreshed") != 1 || strings.Count(log, "up to date again") != 1 ||
		!strings.Contains(log, `"stale_for":"8s"`) || !strings.Contains(log, "connection refused") {
		t.Errorf("the outage is logged as:\n%s", log)
	}
}

// Every five minutes the whole copy is read again. That brings what no
// poll could: a change older than the stretch a poll reads again.
func TestTheWholeCopyIsReadAgainEveryFiveMinutes(t *testing.T) {
	replica, read, moved, _ := loaded(t)

	moved.advance(4*time.Minute + 58*time.Second)
	replica.refresh(t.Context())
	if last := read.asked[len(read.asked)-1]; last == "all" {
		t.Error("the whole copy was read again before five minutes")
	}

	// A link that left without a change the polls could see.
	delete(read.links, "spring-sale")
	moved.advance(2 * time.Second)
	replica.refresh(t.Context())
	if last := read.asked[len(read.asked)-1]; last != "all" {
		t.Errorf("after five minutes the source was asked for %s, want the whole copy", last)
	}
	if _, held := replica.Lookup("spring-sale"); held || replica.Len() != 1 {
		t.Errorf("after the reconciliation the copy holds %d links", replica.Len())
	}

	// And the count starts again.
	moved.advance(2 * time.Second)
	replica.refresh(t.Context())
	if last := read.asked[len(read.asked)-1]; last == "all" {
		t.Error("the whole copy was read twice in a row")
	}
	// A reconciliation that fails leaves the copy, and is due again.
	moved.advance(5 * time.Minute)
	read.failure = errors.New("connection refused")
	replica.refresh(t.Context())
	read.failure = nil
	if replica.Len() != 1 {
		t.Errorf("a failed reconciliation left %d links", replica.Len())
	}
	moved.advance(2 * time.Second)
	replica.refresh(t.Context())
	if last := read.asked[len(read.asked)-1]; last != "all" {
		t.Errorf("after a failed reconciliation the next reading asked for %s", last)
	}
}

func TestLoadFailsWithoutTheDatabaseAndRunStopsWithItsContext(t *testing.T) {
	read := &source{failure: errors.New("connection refused")}
	replica := New(read, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err := replica.Load(t.Context()); err == nil {
		t.Error("a replica loaded without its source")
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		replica.Run(ctx)
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

// Lookups go on while changes are applied. The race detector is the
// assertion.
func TestLookupsAndChangesAtTheSameTime(t *testing.T) {
	replica, read, _, _ := loaded(t)
	var readers sync.WaitGroup
	done := make(chan struct{})
	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					replica.Lookup("k7m2xq9d")
					replica.Age()
					replica.Len()
				}
			}
		})
	}
	for version := int64(2); version < 200; version++ {
		read.put(link.Served{ID: "link-1", Identifier: "k7m2xq9d", State: link.StateActive, Resolved: "https://example.com/a", Version: version})
		if err := replica.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		if version%50 == 0 {
			if err := replica.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(done)
	readers.Wait()
	if found, _ := replica.Lookup("k7m2xq9d"); found.Version != 199 {
		t.Errorf("the last version applied is %d, want 199", found.Version)
	}
}
