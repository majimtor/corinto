package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLinkBlocked means that the operator blocked the link. Its workspace can
// still read it and delete it, and nothing else: a block that the workspace
// could edit away would not be the operator's decision.
var ErrLinkBlocked = errors.New("the link is blocked")

// Change says what a workspace changes in a link. A nil field leaves the
// attribute as it is; the identifier of a link never changes.
type Change struct {
	Destination *string
	Campaign    *Campaign
	Title       *string
	Tags        *[]string
	// ExpiresAt sets the expiry; NoExpiry removes it. With neither, the
	// expiry stays as it is.
	ExpiresAt *time.Time
	NoExpiry  bool
}

// Update applies a change to a link of a workspace and returns the link. A
// change that leaves every attribute as it was is not a change: the version
// stays, and the replica of the redirect has nothing to apply.
func (s *Store) Update(ctx context.Context, workspaceID, id string, change Change) (Link, error) {
	return s.modify(ctx, workspaceID, id, func(ctx context.Context, tx pgx.Tx, current Link) (Link, error) {
		next := current
		if change.Destination != nil || change.Campaign != nil {
			destination := current.Destination
			if change.Destination != nil {
				policy, err := s.policy(ctx, tx, current.WorkspaceID)
				if err != nil {
					return Link{}, err
				}
				if destination, err = ParseDestination(*change.Destination, policy); err != nil {
					return Link{}, err
				}
			}
			if change.Campaign != nil {
				next.Campaign = *change.Campaign
			}
			resolved, err := Resolve(destination, next.Campaign)
			if err != nil {
				return Link{}, err
			}
			next.Destination, next.Resolved = destination, resolved
		}
		if change.Title != nil {
			next.Title = *change.Title
		}
		if change.Tags != nil {
			next.Tags = slices.Clone(*change.Tags)
			if next.Tags == nil {
				next.Tags = []string{}
			}
		}
		switch {
		case change.NoExpiry:
			next.ExpiresAt = nil
		case change.ExpiresAt != nil:
			next.ExpiresAt = change.ExpiresAt
		}
		return next, nil
	})
}

// Disable stops a link from redirecting until it is enabled again.
func (s *Store) Disable(ctx context.Context, workspaceID, id string) (Link, error) {
	return s.setState(ctx, workspaceID, id, StateDisabled)
}

// Enable makes a disabled link redirect again.
func (s *Store) Enable(ctx context.Context, workspaceID, id string) (Link, error) {
	return s.setState(ctx, workspaceID, id, StateActive)
}

func (s *Store) setState(ctx context.Context, workspaceID, id string, state State) (Link, error) {
	return s.modify(ctx, workspaceID, id, func(_ context.Context, _ pgx.Tx, current Link) (Link, error) {
		current.State = state
		return current, nil
	})
}

// Delete erases a link for good. What it led to and what described it are
// removed; the row stays as a tombstone that keeps the identifier, so that
// it is never issued again. A blocked link can be deleted.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	workspace, ok := parseID(workspaceID)
	link, ok2 := parseID(id)
	if !ok || !ok2 {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE links
		    SET state = 'deleted', destination = NULL, resolved_destination = NULL,
		        utm_source = NULL, utm_medium = NULL, utm_campaign = NULL, utm_term = NULL, utm_content = NULL,
		        title = NULL, tags = '{}', expires_at = NULL,
		        version = version + 1, updated_at = now()
		  WHERE workspace_id = $1 AND id = $2 AND state <> 'deleted'`, workspace, link)
	if err != nil {
		return fmt.Errorf("delete the link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// modify loads a link of a workspace under a lock, lets edit decide what it
// becomes and stores the result. The lock makes two changes to one link
// wait for each other, so that neither is computed from a row the other is
// about to replace.
func (s *Store) modify(
	ctx context.Context, workspaceID, id string,
	edit func(ctx context.Context, tx pgx.Tx, current Link) (Link, error),
) (Link, error) {
	workspace, ok := parseID(workspaceID)
	link, ok2 := parseID(id)
	if !ok || !ok2 {
		return Link{}, ErrNotFound
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Link{}, fmt.Errorf("begin the change of a link: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	current, err := scan(tx.QueryRow(ctx,
		`SELECT `+columns+` FROM links
		  WHERE workspace_id = $1 AND id = $2 AND state <> 'deleted' FOR UPDATE`, workspace, link))
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("read the link: %w", err)
	}
	if current.State == StateBlocked {
		return Link{}, ErrLinkBlocked
	}

	next, err := edit(ctx, tx, current)
	if err != nil {
		return Link{}, err
	}
	if same(current, next) {
		return current, nil
	}

	// The time is that of the start of the transaction. A change that
	// commits late carries a time older than polls already made, which is
	// what the overlap window of the replica exists for.
	changed, err := scan(tx.QueryRow(ctx,
		`UPDATE links
		    SET state = $2, destination = $3, resolved_destination = $4,
		        utm_source = $5, utm_medium = $6, utm_campaign = $7, utm_term = $8, utm_content = $9,
		        title = $10, tags = $11, expires_at = $12,
		        version = version + 1, updated_at = now()
		  WHERE id = $1
		  RETURNING `+columns,
		link, next.State, next.Destination, next.Resolved,
		absent(next.Campaign.Source), absent(next.Campaign.Medium), absent(next.Campaign.Name),
		absent(next.Campaign.Term), absent(next.Campaign.Content), absent(next.Title), next.Tags, next.ExpiresAt))
	if err != nil {
		return Link{}, fmt.Errorf("store the change of the link: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Link{}, fmt.Errorf("commit the change of a link: %w", err)
	}
	return changed, nil
}

// policy reads what the workspace of a link adds to the rules of a
// destination, and refuses when the workspace is blocked.
func (s *Store) policy(ctx context.Context, tx pgx.Tx, workspaceID string) (Policy, error) {
	workspace, ok := parseID(workspaceID)
	if !ok {
		return Policy{}, ErrWorkspaceNotFound
	}
	var (
		state     string
		allowlist []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT state, allowed_destinations FROM workspaces WHERE id = $1`, workspace,
	).Scan(&state, &allowlist)
	if err != nil {
		return Policy{}, fmt.Errorf("read the workspace: %w", err)
	}
	if state != "active" {
		return Policy{}, ErrWorkspaceBlocked
	}
	policy := Policy{RedirectHost: s.domain}
	if err := json.Unmarshal(allowlist, &policy.Allowed); err != nil {
		return Policy{}, fmt.Errorf("read the allowlist of the workspace: %w", err)
	}
	return policy, nil
}

// same reports whether two versions of a link hold the same attributes.
func same(a, b Link) bool {
	expiry := (a.ExpiresAt == nil) == (b.ExpiresAt == nil) &&
		(a.ExpiresAt == nil || a.ExpiresAt.Equal(*b.ExpiresAt))
	return a.State == b.State && a.Destination == b.Destination && a.Resolved == b.Resolved &&
		a.Campaign == b.Campaign && a.Title == b.Title && slices.Equal(a.Tags, b.Tags) && expiry
}
