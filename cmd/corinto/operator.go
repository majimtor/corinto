package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
)

const operatorUsageText = `usage: corinto operator <action> [options]

actions:
  create-workspace  --name NAME [--demo] [--aliases] [--allow PREFIX]...
                    [--max-links N] [--max-keys N]
  issue-key         --workspace ID --name NAME --scope SCOPE... [--expires TIME]
  revoke-key        --key ID

An action prints one JSON object on standard output and logs on standard
error. issue-key prints the key once: only a hash of its secret is stored.
PREFIX is a destination without query or fragment, such as
https://docs.example.com/guides. TIME is written as 2027-01-31T00:00:00Z.
`

// usageError is a command line the operator has to write again. It is told
// apart from a failure so that it gets the usage text and its own exit code,
// and it is found before the database is opened.
type usageError struct{ reason string }

func (e usageError) Error() string { return e.reason }

// outcome is what an action did: what it prints and what it leaves in the
// log. They are separate so that a key is printed and never logged.
type outcome struct {
	printed any
	logged  []any
}

// action is one action of the operator, with its options already read.
type action func(ctx context.Context, pool *pgxpool.Pool) (outcome, error)

// runOperator runs one action of the operator: the principal that acts on
// the database through this command and has no HTTP endpoint.
func runOperator(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) == 0 {
		return usageError{"an action is missing"}
	}
	parse, known := map[string]func([]string) (action, error){
		"create-workspace": parseCreateWorkspace,
		"issue-key":        parseIssueKey,
		"revoke-key":       parseRevokeKey,
	}[args[0]]
	if !known {
		return usageError{fmt.Sprintf("there is no action %q", args[0])}
	}
	act, err := parse(args[1:])
	if err != nil {
		return err
	}

	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	if err := env.err(); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("configure the database pool: %w", err)
	}
	defer pool.Close()
	checkCtx, cancel := context.WithTimeout(ctx, schemaCheckTimeout)
	defer cancel()
	if err := postgres.CheckSchema(checkCtx, pool); err != nil {
		return err
	}

	done, err := act(ctx, pool)
	if err != nil {
		return err
	}
	// Logged before it is printed: if the output cannot be written, the log
	// still names what was created, and a key nobody saw can be revoked.
	logger.InfoContext(ctx, "operator action", append([]any{"action", args[0]}, done.logged...)...)
	if err := json.NewEncoder(stdout).Encode(done.printed); err != nil {
		return fmt.Errorf("print the outcome: %w", err)
	}
	return nil
}

// options returns the option set of an action. Its errors are returned, not
// printed: runOperator prints them with the usage text.
func options(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.Usage = func() {}
	return set
}

// read parses the command line of an action and requires the options that
// have no default.
func read(set *flag.FlagSet, args []string, required ...string) error {
	if err := set.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if set.NArg() > 0 {
		return usageError{fmt.Sprintf("%q is not an option", set.Arg(0))}
	}
	given := make(map[string]bool)
	set.Visit(func(option *flag.Flag) { given[option.Name] = true })
	for _, name := range required {
		if !given[name] {
			return usageError{fmt.Sprintf("%s needs --%s", set.Name(), name)}
		}
	}
	return nil
}

// repeated is an option that can be given several times.
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }

// Set adds one value of the option.
func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func parseCreateWorkspace(args []string) (action, error) {
	set := options("create-workspace")
	var (
		draft link.WorkspaceDraft
		allow repeated
	)
	set.StringVar(&draft.Name, "name", "", "")
	set.BoolVar(&draft.Demo, "demo", false, "")
	set.BoolVar(&draft.MayChooseAliases, "aliases", false, "")
	set.Var(&allow, "allow", "")
	maxLinks := set.Int("max-links", 0, "")
	maxKeys := set.Int("max-keys", 0, "")
	if err := read(set, args, "name"); err != nil {
		return nil, err
	}
	draft.Allowed = allow

	// A quota that was not given takes its initial value, which for the
	// links depends on whether the workspace is a demo.
	draft.MaxLinks, draft.MaxKeys = limits.MaxLinks, limits.MaxKeys
	if draft.Demo {
		draft.MaxLinks = limits.MaxLinksDemo
	}
	set.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "max-links":
			draft.MaxLinks = *maxLinks
		case "max-keys":
			draft.MaxKeys = *maxKeys
		}
	})

	return func(ctx context.Context, pool *pgxpool.Pool) (outcome, error) {
		workspace, err := link.CreateWorkspace(ctx, pool, draft)
		if err != nil {
			return outcome{}, fmt.Errorf("create the workspace: %w", err)
		}
		return outcome{
			printed: struct {
				ID               string               `json:"id"`
				Name             string               `json:"name"`
				Demo             bool                 `json:"demo"`
				MayChooseAliases bool                 `json:"may_choose_aliases"`
				Allowed          []link.AllowedPrefix `json:"allowed_destinations"`
				MaxLinks         int                  `json:"max_links"`
				MaxKeys          int                  `json:"max_keys"`
			}{
				workspace.ID, workspace.Name, workspace.Demo, workspace.MayChooseAliases,
				workspace.Allowed, workspace.MaxLinks, workspace.MaxKeys,
			},
			logged: []any{"workspace", workspace.ID},
		}, nil
	}, nil
}

func parseIssueKey(args []string) (action, error) {
	set := options("issue-key")
	var scopes repeated
	workspace := set.String("workspace", "", "")
	name := set.String("name", "", "")
	set.Var(&scopes, "scope", "")
	expires := set.String("expires", "", "")
	if err := read(set, args, "workspace", "name", "scope"); err != nil {
		return nil, err
	}
	draft := auth.Draft{Name: *name}
	for _, scope := range scopes {
		draft.Scopes = append(draft.Scopes, auth.Scope(scope))
	}
	if *expires != "" {
		expiry, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return nil, usageError{fmt.Sprintf("--expires is a time such as 2027-01-31T00:00:00Z, not %q", *expires)}
		}
		draft.ExpiresAt = &expiry
	}

	return func(ctx context.Context, pool *pgxpool.Pool) (outcome, error) {
		key, credential, err := auth.NewKeys(pool, rand.Reader).Issue(ctx, *workspace, draft)
		if err != nil {
			return outcome{}, fmt.Errorf("issue the key: %w", err)
		}
		return outcome{
			printed: struct {
				ID          string       `json:"id"`
				WorkspaceID string       `json:"workspace_id"`
				Name        string       `json:"name"`
				Scopes      []auth.Scope `json:"scopes"`
				ExpiresAt   *time.Time   `json:"expires_at"`
				Key         string       `json:"key"`
			}{key.ID, key.WorkspaceID, key.Name, key.Scopes, key.ExpiresAt, credential},
			logged: []any{"workspace", key.WorkspaceID, "key", key.ID},
		}, nil
	}, nil
}

func parseRevokeKey(args []string) (action, error) {
	set := options("revoke-key")
	id := set.String("key", "", "")
	if err := read(set, args, "key"); err != nil {
		return nil, err
	}

	return func(ctx context.Context, pool *pgxpool.Pool) (outcome, error) {
		key, err := auth.NewKeys(pool, rand.Reader).RevokeAnywhere(ctx, *id)
		if err != nil {
			return outcome{}, fmt.Errorf("revoke the key: %w", err)
		}
		return outcome{
			printed: struct {
				ID          string `json:"id"`
				WorkspaceID string `json:"workspace_id"`
				Name        string `json:"name"`
			}{key.ID, key.WorkspaceID, key.Name},
			logged: []any{"workspace", key.WorkspaceID, "key", key.ID},
		}, nil
	}, nil
}
