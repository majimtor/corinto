// Package resolve holds the copy of the links that the redirect answers
// from. Every lookup is served from memory: once the copy is loaded, the
// redirect depends on nothing else to answer, and keeps answering with its
// last known state when the database is away
// (docs/adr/0006-in-memory-replica-for-link-resolution.md).
package resolve

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// The times of the replica (PLAN.md section 3).
const (
	// pollEvery is how often the replica asks for what changed.
	pollEvery = 2 * time.Second
	// overlap is how far back each poll reads again. A change carries the
	// time at which its transaction began, so one that was committed after
	// a poll can be older than what that poll already read: the overlap is
	// how long a transaction may stay open and still be seen by the next
	// poll.
	overlap = 30 * time.Second
	// reconcileEvery is how often the whole copy is read again, which
	// brings what a transaction open for longer than the overlap changed.
	reconcileEvery = 5 * time.Minute
	// refreshTimeout bounds one reading of the database, so that one that
	// hangs is a failed reading and not a replica that stopped asking.
	refreshTimeout = 10 * time.Second
)

// Link is what the redirect needs of a link.
type Link struct {
	ID          string
	WorkspaceID string
	// Destination is what the redirect answers with: the destination of
	// the link with its campaign parameters.
	Destination string
	State       link.State
	// ExpiresAt is when the link stops redirecting; zero when it does not
	// expire.
	ExpiresAt time.Time
	// Version tells an older state of the link from a newer one.
	Version int64
}

// Redirects reports whether the link redirects at a moment: it is active
// and not past its expiry. Expiry is read from the clock on every request,
// so it needs no change to arrive.
func (l Link) Redirects(at time.Time) bool {
	return l.State == link.StateActive && (l.ExpiresAt.IsZero() || at.Before(l.ExpiresAt))
}

// Source is where the replica reads the links from. In the process it is
// *link.Store; it is an interface so that a test can make a reading fail or
// make a change arrive late.
type Source interface {
	Served(ctx context.Context) ([]link.Served, time.Time, error)
	ServedSince(ctx context.Context, since time.Time) ([]link.Served, time.Time, error)
}

// Replica is the copy of the links, by short identifier.
type Replica struct {
	source Source
	logger *slog.Logger
	// now is the clock of the process. It measures how old the copy is and
	// when the next reconciliation is due; what to ask the database for is
	// decided with the clock of the database alone.
	now func() time.Time

	mu    sync.RWMutex
	links map[string]Link
	// mark is the time of the database at the last reading that succeeded.
	mark time.Time
	// refreshed and reconciled are when the copy was last read, in part or
	// in whole, by the clock of the process.
	refreshed, reconciled time.Time
	// stale says that the last reading failed, so that an outage is logged
	// when it begins and when it ends, not on every poll in between.
	stale bool
}

// New returns a replica with nothing in it. Load fills it.
func New(source Source, logger *slog.Logger) *Replica {
	return &Replica{source: source, logger: logger, now: time.Now, links: make(map[string]Link)}
}

// Lookup returns the link with a short identifier, if there is one. A
// deleted link is not there.
func (r *Replica) Lookup(identifier string) (Link, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	found, held := r.links[identifier]
	return found, held
}

// Len returns how many links the copy holds.
func (r *Replica) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.links)
}

// Age returns how long ago the copy was last read from the database. While
// the database answers it stays under the time between two polls; when it
// does not, it grows, and it is how stale the answers of the redirect can
// be.
func (r *Replica) Age() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.now().Sub(r.refreshed)
}

// Load reads every link. The process calls it before it serves and does
// not start when it fails: a redirect without its links would answer that
// none exists. It is also the reconciliation: what it reads replaces the
// whole copy, so whatever the polls missed is there afterwards.
func (r *Replica) Load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	served, at, err := r.source.Served(ctx)
	if err != nil {
		return err
	}
	links := make(map[string]Link, len(served))
	for _, row := range served {
		links[row.Identifier] = held(row)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links, r.mark = links, at
	r.refreshed = r.now()
	r.reconciled = r.refreshed
	return nil
}

// Poll reads what changed since the last reading, and a stretch before it
// again, and applies it. A change is applied only when its version is newer
// than the one the copy holds, so reading the same change twice changes
// nothing.
func (r *Replica) Poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	r.mu.RLock()
	since := r.mark.Add(-overlap)
	r.mu.RUnlock()

	served, at, err := r.source.ServedSince(ctx, since)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range served {
		current, known := r.links[row.Identifier]
		switch {
		case known && current.Version >= row.Version:
			// Already applied, by this poll or by one before it.
		case row.State == link.StateDeleted:
			delete(r.links, row.Identifier)
		default:
			r.links[row.Identifier] = held(row)
		}
	}
	r.mark = at
	r.refreshed = r.now()
	return nil
}

// Run keeps the copy up to date until ctx is done: it polls for changes
// every two seconds and reads the whole copy again every five minutes. A
// reading that fails changes nothing: the copy stays as it was and its age
// grows until one succeeds.
func (r *Replica) Run(ctx context.Context) {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh does what is due: the whole copy when the last one is old enough,
// and otherwise only what changed.
func (r *Replica) refresh(ctx context.Context) {
	r.mu.RLock()
	due := r.now().Sub(r.reconciled) >= reconcileEvery
	age := r.now().Sub(r.refreshed)
	r.mu.RUnlock()
	read := r.Poll
	if due {
		read = r.Load
	}
	err := read(ctx)

	r.mu.Lock()
	wasStale := r.stale
	r.stale = err != nil
	r.mu.Unlock()
	switch {
	case err != nil && ctx.Err() != nil:
		// The process is stopping: the reading was cut off, not refused.
	case err != nil && !wasStale:
		r.logger.WarnContext(ctx, "the replica could not be refreshed and keeps its last state", "error", err.Error())
	case err == nil && wasStale:
		r.logger.InfoContext(ctx, "the replica is up to date again", "stale_for", age.String())
	}
}

// held turns a link as the store reads it into what the copy keeps.
func held(row link.Served) Link {
	kept := Link{
		ID: row.ID, WorkspaceID: row.WorkspaceID, Destination: row.Resolved,
		State: row.State, Version: row.Version,
	}
	if row.ExpiresAt != nil {
		kept.ExpiresAt = *row.ExpiresAt
	}
	return kept
}
