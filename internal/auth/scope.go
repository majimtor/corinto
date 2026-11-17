// Package auth resolves who a request acts as. In the first stretch that is
// an API key: a credential that a system sends in a header and that
// resolves to a workspace and to the scopes the key was issued with. The
// package owns the table of the keys and its queries.
package auth

import (
	"errors"
	"slices"
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

// Principal is who a request acts as once its key is resolved: a workspace
// and what the key may do in it.
type Principal struct {
	WorkspaceID string
	KeyID       string
	Scopes      []Scope
}

// Allows reports whether the principal holds a scope.
func (p Principal) Allows(scope Scope) bool {
	return slices.Contains(p.Scopes, scope)
}
