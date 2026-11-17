package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxGenerationAttempts bounds the identifiers tried for one link. With a
	// million links a collision has a probability of about four in a
	// million; five in a row mean a broken generator, and that must fail.
	maxGenerationAttempts = 5

	// MaxPage is the largest page List returns: no collection is unbounded.
	MaxPage = 100
)

// State is the state of a link. Expiry is not one: it is derived from the
// expiry time on every request.
type State string

// The states of a link.
const (
	StateActive   State = "active"
	StateDisabled State = "disabled"
	StateBlocked  State = "blocked"
	StateDeleted  State = "deleted"
)

// Why the store refuses.
var (
	// ErrNotFound covers a link that does not exist, one that is deleted and
	// one of another workspace: from outside they cannot be told apart.
	ErrNotFound          = errors.New("the workspace has no such link")
	ErrWorkspaceNotFound = errors.New("there is no such workspace")
	ErrWorkspaceBlocked  = errors.New("the workspace is blocked")
	ErrAliasNotPermitted = errors.New("the workspace cannot choose aliases")
	ErrAliasTaken        = errors.New("the alias is already in use")
	ErrQuotaExceeded     = errors.New("the workspace has reached its quota of links")
	ErrNoFreeIdentifier  = errors.New("no free identifier was generated in five attempts")
	ErrPage              = errors.New("a page has between 1 and 100 links and starts after a link identifier")
)

// Link is a link as it is stored.
type Link struct {
	ID          string
	WorkspaceID string
	Domain      string
	Identifier  string
	State       State
	Destination string
	// Resolved is what the redirect serves: the destination with the
	// campaign parameters appended, computed when the link is written.
	Resolved  string
	Campaign  Campaign
	Title     string
	Tags      []string
	ExpiresAt *time.Time
	// Version grows with every change. The replica of the redirect applies a
	// change only when its version is newer than the one it holds.
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft is what a workspace gives to create a link. An empty Alias asks for
// a generated identifier. Title, Tags and ExpiresAt are stored as given: the
// operations that take them from outside state their limits.
type Draft struct {
	Alias       string
	Destination string
	Campaign    Campaign
	Title       string
	Tags        []string
	ExpiresAt   *time.Time
}

// Store reads and writes links. Every method takes the workspace and loads a
// link by workspace and identifier, so that no query crosses workspaces.
type Store struct {
	pool   *pgxpool.Pool
	domain string
	random io.Reader
}

// NewStore returns a store whose links live under one redirect domain.
// random is the source of the generated identifiers; everything but a test
// passes crypto/rand.Reader.
func NewStore(pool *pgxpool.Pool, redirectDomain string, random io.Reader) *Store {
	return &Store{pool: pool, domain: redirectDomain, random: random}
}

const columns = `id, workspace_id, domain, identifier, state, destination, resolved_destination,
	utm_source, utm_medium, utm_campaign, utm_term, utm_content, title, tags, expires_at,
	version, created_at, updated_at`

// Create stores a new link of a workspace and returns it.
func (s *Store) Create(ctx context.Context, workspaceID string, draft Draft) (Link, error) {
	workspace, ok := parseID(workspaceID)
	if !ok {
		return Link{}, ErrWorkspaceNotFound
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Link{}, fmt.Errorf("begin the creation of a link: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The row of the workspace is locked until the commit, so the count
	// below cannot be passed by two creations at once.
	settings, err := LockWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return Link{}, err
	}

	identifier := ""
	if draft.Alias != "" {
		if !settings.MayChooseAliases {
			return Link{}, ErrAliasNotPermitted
		}
		if identifier, err = ParseAlias(draft.Alias); err != nil {
			return Link{}, err
		}
	}

	destination, err := ParseDestination(draft.Destination, Policy{RedirectHost: s.domain, Allowed: settings.Allowed})
	if err != nil {
		return Link{}, err
	}
	resolved, err := Resolve(destination, draft.Campaign)
	if err != nil {
		return Link{}, err
	}

	// A deleted link is a tombstone and takes no room in the quota.
	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM links WHERE workspace_id = $1 AND state <> 'deleted'`, workspace,
	).Scan(&live); err != nil {
		return Link{}, fmt.Errorf("count the links of the workspace: %w", err)
	}
	if live >= settings.MaxLinks {
		return Link{}, ErrQuotaExceeded
	}

	tags := draft.Tags
	if tags == nil {
		tags = []string{}
	}
	attempts := 1
	if identifier == "" {
		attempts = maxGenerationAttempts
	}
	for range attempts {
		candidate := identifier
		if candidate == "" {
			if candidate, err = NewIdentifier(s.random); err != nil {
				return Link{}, err
			}
		}
		// A taken identifier inserts nothing instead of failing, which would
		// end the transaction: the next attempt runs in the same one.
		row := tx.QueryRow(ctx,
			`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination,
				utm_source, utm_medium, utm_campaign, utm_term, utm_content, title, tags, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			 ON CONFLICT (domain, identifier) DO NOTHING
			 RETURNING `+columns,
			workspace, s.domain, candidate, destination, resolved,
			absent(draft.Campaign.Source), absent(draft.Campaign.Medium), absent(draft.Campaign.Name),
			absent(draft.Campaign.Term), absent(draft.Campaign.Content), absent(draft.Title), tags, draft.ExpiresAt,
		)
		created, err := scan(row)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Link{}, fmt.Errorf("insert the link: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Link{}, fmt.Errorf("commit the creation of a link: %w", err)
		}
		return created, nil
	}
	if identifier != "" {
		return Link{}, ErrAliasTaken
	}
	return Link{}, ErrNoFreeIdentifier
}

// Get returns a link of a workspace.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (Link, error) {
	workspace, ok := parseID(workspaceID)
	link, ok2 := parseID(id)
	if !ok || !ok2 {
		return Link{}, ErrNotFound
	}
	found, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM links WHERE workspace_id = $1 AND id = $2 AND state <> 'deleted'`,
		workspace, link))
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("read the link: %w", err)
	}
	return found, nil
}

// List returns up to limit links of a workspace in the order in which they
// were created, starting after the link whose identifier is after; an empty
// after starts at the beginning. The order is that of the identifiers, which
// the database generates in time order. Creations in one workspace wait for
// each other, so its identifiers are also in the order of the commits, and
// a page neither repeats nor skips a link created between two calls.
func (s *Store) List(ctx context.Context, workspaceID, after string, limit int) ([]Link, error) {
	workspace, ok := parseID(workspaceID)
	if !ok {
		return nil, ErrWorkspaceNotFound
	}
	if limit < 1 || limit > MaxPage {
		return nil, ErrPage
	}
	// The zero identifier sorts before every generated one.
	start := pgtype.UUID{Valid: true}
	if after != "" {
		if start, ok = parseID(after); !ok {
			return nil, ErrPage
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM links
		  WHERE workspace_id = $1 AND state <> 'deleted' AND id > $2
		  ORDER BY id LIMIT $3`, workspace, start, limit)
	if err != nil {
		return nil, fmt.Errorf("list the links: %w", err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Link, error) { return scan(row) })
	if err != nil {
		return nil, fmt.Errorf("list the links: %w", err)
	}
	return links, nil
}

// parseID reads an identifier of the database. One that is not well formed
// names nothing, which is how the caller treats it. The package of the keys
// has the same few lines: sharing them would tie the two packages for less
// than it saves.
func parseID(id string) (pgtype.UUID, bool) {
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil || !parsed.Valid {
		return pgtype.UUID{}, false
	}
	return parsed, true
}

// absent turns an empty value into the NULL that the schema keeps for a
// field the link does not set.
func absent(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func present(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// scan reads one row with the columns above.
func scan(row pgx.Row) (Link, error) {
	var (
		link                                Link
		id, workspace                       pgtype.UUID
		source, medium, name, term, content *string
		title                               *string
	)
	err := row.Scan(&id, &workspace, &link.Domain, &link.Identifier, &link.State, &link.Destination, &link.Resolved,
		&source, &medium, &name, &term, &content, &title, &link.Tags, &link.ExpiresAt,
		&link.Version, &link.CreatedAt, &link.UpdatedAt)
	if err != nil {
		return Link{}, err
	}
	link.ID, link.WorkspaceID = id.String(), workspace.String()
	link.Campaign = Campaign{
		Source: present(source), Medium: present(medium), Name: present(name),
		Term: present(term), Content: present(content),
	}
	link.Title = present(title)
	return link, nil
}
