// Package auth resolves who a request acts as. For a system that is an API
// key: a credential it sends in a header, which resolves to a workspace and
// to the scopes the key was issued with. For a person in the panel it is a
// session: a token their browser sends in a cookie, which resolves to a
// workspace. The package owns the tables of the keys and of the sessions,
// and their queries.
package auth

import (
	"errors"
	"slices"
	"time"
)

// Scope is one thing a key may do in its workspace. Every management
// operation of the API declares the scope it requires.
type Scope string

// The scopes. They are code, not rows; the schema repeats the list in a
// check, so that the database refuses what the code would not write.
const (
	ScopeLinksRead     Scope = "links:read"
	ScopeLinksWrite    Scope = "links:write"
	ScopeMetricsRead   Scope = "metrics:read"
	ScopeWebhooksRead  Scope = "webhooks:read"
	ScopeWebhooksWrite Scope = "webhooks:write"
)

// ErrScopes means that a key was asked for with no scope or with one that
// does not exist.
var ErrScopes = errors.New("a key has at least one scope, and each of them is one of the known scopes")

// Scopes returns every scope, in the order in which a key stores its own.
func Scopes() []Scope {
	return []Scope{ScopeLinksRead, ScopeLinksWrite, ScopeMetricsRead, ScopeWebhooksRead, ScopeWebhooksWrite}
}

// canonical returns the scopes that were asked for, each once and in the
// order of Scopes, so that two keys with the same scopes store the same
// list.
func canonical(asked []Scope) ([]Scope, error) {
	if len(asked) == 0 {
		return nil, ErrScopes
	}
	known := Scopes()
	for _, scope := range asked {
		if !slices.Contains(known, scope) {
			return nil, ErrScopes
		}
	}
	return slices.DeleteFunc(known, func(scope Scope) bool { return !slices.Contains(asked, scope) }), nil
}

// Principal is who a request acts as once its key or its session is
// resolved: a workspace and what may be done in it.
type Principal struct {
	WorkspaceID string
	// KeyID names the key the request brought, and SessionID the session.
	// A principal has one of the two.
	KeyID     string
	SessionID string
	Scopes    []Scope
	// ExpiresAt is when the key or the session stops authenticating, or
	// nil for a key that has no expiry.
	ExpiresAt *time.Time
}

// Allows reports whether the principal holds a scope.
func (p Principal) Allows(scope Scope) bool {
	return slices.Contains(p.Scopes, scope)
}
