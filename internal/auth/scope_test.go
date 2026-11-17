package auth

import (
	"errors"
	"slices"
	"testing"
)

// The scopes, written here a second time on purpose: PLAN.md section 9
// lists these five, and a sixth is a decision, not an edit.
var planned = []Scope{"links:read", "links:write", "metrics:read", "webhooks:read", "webhooks:write"}

func TestTheScopesAreTheFiveOfThePlan(t *testing.T) {
	if got := Scopes(); !slices.Equal(got, planned) {
		t.Errorf("Scopes() = %v, want %v", got, planned)
	}
}

func TestTheScopesOfAKeyAreStoredOnceAndInOrder(t *testing.T) {
	cases := []struct {
		name  string
		asked []Scope
		want  []Scope
	}{
		{"one", []Scope{ScopeMetricsRead}, []Scope{ScopeMetricsRead}},
		{"out of order", []Scope{ScopeWebhooksWrite, ScopeLinksRead}, []Scope{ScopeLinksRead, ScopeWebhooksWrite}},
		{"repeated", []Scope{ScopeLinksWrite, ScopeLinksRead, ScopeLinksWrite}, []Scope{ScopeLinksRead, ScopeLinksWrite}},
		{"all of them, backwards", []Scope{
			ScopeWebhooksWrite, ScopeWebhooksRead, ScopeMetricsRead, ScopeLinksWrite, ScopeLinksRead,
		}, planned},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asked := slices.Clone(c.asked)
			got, err := canonical(asked)
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("canonical(%v) = %v, %v; want %v", c.asked, got, err, c.want)
			}
			if !slices.Equal(asked, c.asked) {
				t.Errorf("canonical changed what it was given: %v", asked)
			}
		})
	}
	if got := Scopes(); !slices.Equal(got, planned) {
		t.Errorf("after canonical, Scopes() = %v, want %v", got, planned)
	}
}

func TestAKeyNeedsScopesThatExist(t *testing.T) {
	cases := map[string][]Scope{
		"none":                      nil,
		"an empty list":             {},
		"one that does not exist":   {"links:delete"},
		"a known one and one not":   {ScopeLinksRead, "admin"},
		"an empty scope":            {""},
		"a known one in upper case": {"LINKS:READ"},
		"every scope at once":       {"*"},
	}
	for name, asked := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := canonical(asked); !errors.Is(err, ErrScopes) || got != nil {
				t.Errorf("canonical(%v) = %v, %v; want %v", asked, got, err, ErrScopes)
			}
		})
	}
}

func TestAPrincipalAllowsOnlyTheScopesItHolds(t *testing.T) {
	principal := Principal{Scopes: []Scope{ScopeLinksRead, ScopeMetricsRead}}
	for _, scope := range planned {
		want := scope == ScopeLinksRead || scope == ScopeMetricsRead
		if got := principal.Allows(scope); got != want {
			t.Errorf("Allows(%s) = %v, want %v", scope, got, want)
		}
	}
	if (Principal{}).Allows(ScopeLinksRead) {
		t.Error("a principal with no scope allows one")
	}
	// Writing does not include reading: each scope is asked for by name.
	if (Principal{Scopes: []Scope{ScopeLinksWrite}}).Allows(ScopeLinksRead) {
		t.Error("links:write allowed links:read")
	}
}
