package main

import (
	"bytes"
	"strings"
	"testing"
)

// A command line that has to be written again is found before anything
// else: no variable is set here, and none is asked for.
func TestTheOperatorRefusesWrongUsageBeforeItReadsItsEnvironment(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no action", nil, "an action is missing"},
		{"an action that does not exist", []string{"delete-workspace"}, `there is no action "delete-workspace"`},
		{"an option that does not exist", []string{"create-workspace", "--name", "Acme", "--admin"}, "admin"},
		{"a workspace without a name", []string{"create-workspace", "--demo"}, "create-workspace needs --name"},
		{"a quota that is not a number", []string{"create-workspace", "--name", "Acme", "--max-links", "many"}, "max-links"},
		{"a word that is not an option", []string{"create-workspace", "--name", "Acme", "now"}, `"now" is not an option`},
		{"a key without a workspace", []string{"issue-key", "--name", "Deploy pipeline", "--scope", "links:read"}, "issue-key needs --workspace"},
		{"a key without a name", []string{"issue-key", "--workspace", "w", "--scope", "links:read"}, "issue-key needs --name"},
		{"a key without a scope", []string{"issue-key", "--workspace", "w", "--name", "Deploy pipeline"}, "issue-key needs --scope"},
		{"an expiry that is not a time", []string{"issue-key", "--workspace", "w", "--name", "n", "--scope", "links:read", "--expires", "tomorrow"}, "--expires is a time"},
		{"a revocation without a key", []string{"revoke-key"}, "revoke-key needs --key"},
		{"a block without a link", []string{"block-link"}, "block-link needs --identifier"},
		{"a block without a workspace", []string{"block-workspace"}, "block-workspace needs --workspace"},
		{"dead letters without saying which", []string{"retry-dead-letters"}, "retry-dead-letters needs --event or --all, and not both"},
		{"one dead letter and all of them", []string{"retry-dead-letters", "--event", "e", "--all"}, "retry-dead-letters needs --event or --all, and not both"},
		{"a dead letter named without its option", []string{"retry-dead-letters", "e"}, `"e" is not an option`},
		{"a request for help", []string{"issue-key", "--help"}, "help"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"operator"}, c.args...)
			if code := run(t.Context(), args, environmentOf(nil), &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(stderr.String(), "usage: corinto operator") {
				t.Errorf("stderr does not start with the usage text of the operator: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr does not say %q: %q", c.want, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("something was printed: %q", stdout.String())
			}
		})
	}
}

func TestTheOperatorNamesItsMissingVariable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"operator", "create-workspace", "--name", "Acme"}
	if code := run(t.Context(), args, environmentOf(nil), &stdout, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "CORINTO_DATABASE_URL") {
		t.Errorf("stderr does not name CORINTO_DATABASE_URL: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("something was printed: %q", stdout.String())
	}
}

// block-link is the one action that needs the redirect domain, and it says
// so together with the variable every action needs.
func TestBlockLinkNamesEveryMissingVariable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"operator", "block-link", "--identifier", "k7m2xq9d"}
	if code := run(t.Context(), args, environmentOf(nil), &stdout, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_REDIRECT_DOMAIN"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr does not name %s: %q", name, stderr.String())
		}
	}
}

func TestTheOtherActionsDoNotAskForTheRedirectDomain(t *testing.T) {
	for _, args := range [][]string{
		{"operator", "block-workspace", "--workspace", "00000000-0000-7000-8000-000000000000"},
		{"operator", "retry-dead-letters", "--all"},
		{"operator", "retry-dead-letters", "--event", "00000000-0000-7000-8000-000000000000"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, environmentOf(nil), &stdout, &stderr); code != exitFailure {
			t.Errorf("%s: exit code = %d, want %d", args[1], code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "CORINTO_DATABASE_URL") || strings.Contains(stderr.String(), "CORINTO_REDIRECT_DOMAIN") {
			t.Errorf("%s does not ask for the database alone: %q", args[1], stderr.String())
		}
	}
}
