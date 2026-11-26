package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxWorkspaceNameLength bounds the name of a workspace, in characters.
const maxWorkspaceNameLength = 100

// Why a workspace is not created.
var (
	ErrWorkspaceName  = errors.New("the name of a workspace has between 1 and 100 printable characters")
	ErrWorkspaceQuota = errors.New("a quota of a workspace is zero or more")
	ErrAllowedPrefix  = errors.New("an entry of the allowlist has neither a query nor a fragment")
)

// Workspace is the root of ownership and isolation: every link and every
// key belongs to exactly one.
type Workspace struct {
	ID   string
	Name string
	// Demo marks a workspace that can be entered without a secret.
	Demo             bool
	MayChooseAliases bool
	// Allowed is the destination policy: the entries a destination must
	// match. An empty list allows any destination.
	Allowed []AllowedPrefix
	// The quotas. Each bounds what the workspace holds of one kind, and the
	// package that owns the kind does the counting.
	MaxLinks int
	MaxKeys  int
}

// WorkspaceDraft is what the operator gives to create a workspace. Allowed
// holds the entries of the allowlist as they are written, each one a
// destination without query or fragment. The quotas have no default here:
// whoever creates the workspace gives them.
type WorkspaceDraft struct {
	Name             string
	Demo             bool
	MayChooseAliases bool
	Allowed          []string
	MaxLinks         int
	MaxKeys          int
}

// CreateWorkspace stores a new workspace and returns it. Creating one is
// the operator's: no principal of a workspace can.
func CreateWorkspace(ctx context.Context, pool *pgxpool.Pool, draft WorkspaceDraft) (Workspace, error) {
	// The name of a key has the same rule, in the package of the keys: two
	// rules that happen to agree, not one to share.
	if !utf8.ValidString(draft.Name) || draft.Name == "" ||
		utf8.RuneCountInString(draft.Name) > maxWorkspaceNameLength ||
		strings.ContainsFunc(draft.Name, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return Workspace{}, ErrWorkspaceName
	}
	if draft.MaxLinks < 0 || draft.MaxKeys < 0 {
		return Workspace{}, ErrWorkspaceQuota
	}
	// Never nil, so that a workspace without a policy stores an empty list.
	allowed := make([]AllowedPrefix, 0, len(draft.Allowed))
	for _, entry := range draft.Allowed {
		prefix, err := parseAllowedPrefix(entry)
		if err != nil {
			return Workspace{}, err
		}
		allowed = append(allowed, prefix)
	}
	allowlist, err := json.Marshal(allowed)
	if err != nil {
		return Workspace{}, fmt.Errorf("write the allowlist of the workspace: %w", err)
	}

	workspace := Workspace{
		Name: draft.Name, Demo: draft.Demo, MayChooseAliases: draft.MayChooseAliases,
		Allowed: allowed, MaxLinks: draft.MaxLinks, MaxKeys: draft.MaxKeys,
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, demo, may_choose_aliases, allowed_destinations, max_links, max_keys)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		draft.Name, draft.Demo, draft.MayChooseAliases, allowlist, draft.MaxLinks, draft.MaxKeys,
	).Scan(&workspace.ID); err != nil {
		return Workspace{}, fmt.Errorf("insert the workspace: %w", err)
	}
	return workspace, nil
}

// parseAllowedPrefix reads an entry of an allowlist. It is written as a
// destination and meets the rules of one, so that what an entry names is
// what a destination is compared with: the same case, the same port, the
// same path.
func parseAllowedPrefix(entry string) (AllowedPrefix, error) {
	if strings.ContainsAny(entry, "?#") {
		return AllowedPrefix{}, ErrAllowedPrefix
	}
	destination, err := ParseDestination(entry, Policy{})
	if err != nil {
		return AllowedPrefix{}, err
	}
	// A destination in its stored form always has both separators.
	scheme, rest, _ := strings.Cut(destination, "://")
	slash := strings.IndexByte(rest, '/')
	return AllowedPrefix{Scheme: scheme, Host: rest[:slash], Path: rest[slash:]}, nil
}

// LockWorkspace reads an active workspace and locks its row until the
// transaction ends. Whatever is created under a quota goes through it:
// creations in one workspace wait for each other, so a count taken after
// this call cannot be passed by two of them at once, and the workspace
// cannot be blocked halfway. A workspace that does not exist and one that
// is blocked are errors.
func LockWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (Workspace, error) {
	id, ok := parseID(workspaceID)
	if !ok {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return scanWorkspace(tx.QueryRow(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1 FOR UPDATE`, id))
}

// Workspace reads an active workspace, and holds nothing. It is for
// whoever shows a workspace; whatever is created in one goes through
// LockWorkspace. A workspace that does not exist and one that is blocked
// are the same errors as there.
func (s *Store) Workspace(ctx context.Context, workspaceID string) (Workspace, error) {
	id, ok := parseID(workspaceID)
	if !ok {
		return Workspace{}, ErrWorkspaceNotFound
	}
	return scanWorkspace(s.pool.QueryRow(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1`, id))
}

// workspaceColumns are the columns scanWorkspace reads, in its order.
const workspaceColumns = `id, name, state, demo, may_choose_aliases, allowed_destinations, max_links, max_keys`

// scanWorkspace reads a workspace from a row of workspaceColumns. No row
// is a workspace that does not exist, and a row of a blocked workspace is
// an error too: nothing reads a blocked workspace to act in it.
func scanWorkspace(row pgx.Row) (Workspace, error) {
	var (
		workspace Workspace
		state     string
		allowlist []byte
	)
	err := row.Scan(&workspace.ID, &workspace.Name, &state, &workspace.Demo, &workspace.MayChooseAliases,
		&allowlist, &workspace.MaxLinks, &workspace.MaxKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrWorkspaceNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("read the workspace: %w", err)
	}
	if state != "active" {
		return Workspace{}, ErrWorkspaceBlocked
	}
	if err := json.Unmarshal(allowlist, &workspace.Allowed); err != nil {
		return Workspace{}, fmt.Errorf("read the allowlist of the workspace: %w", err)
	}
	return workspace, nil
}
