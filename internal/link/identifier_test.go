package link_test

import (
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

// The alphabet and the grammar, written here a second time on purpose: the
// tests state the rule of the plan instead of reading it from the code.
const alphabet = "23456789bcdfghjkmnpqrstvwxz"

var grammar = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// constant is a source of randomness that returns the same byte for ever.
type constant byte

func (c constant) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}
	return len(p), nil
}

// failing is a source of randomness that cannot be read.
type failing struct{}

var errNoEntropy = errors.New("no entropy")

func (failing) Read([]byte) (int, error) { return 0, errNoEntropy }

func TestAGeneratedIdentifierStaysInsideTheAlphabet(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		identifier, err := link.NewIdentifier(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(identifier) != 8 {
			t.Fatalf("%q has %d characters, want 8", identifier, len(identifier))
		}
		if strings.Trim(identifier, alphabet) != "" {
			t.Fatalf("%q has a character outside the alphabet", identifier)
		}
		if !link.WellFormedIdentifier(identifier) {
			t.Fatalf("%q is generated and not well formed", identifier)
		}
		seen[identifier] = true
	}
	// A repeat among a thousand has a probability of about two in a million.
	if len(seen) != 1000 {
		t.Errorf("%d distinct identifiers out of 1000", len(seen))
	}
}

// Every symbol must come from the same number of byte values. With 27
// symbols and 256 values that is nine each, and thirteen values discarded.
func TestTheGeneratorHasNoModuloBias(t *testing.T) {
	values := make(map[byte]int)
	discarded := 0
	for b := range 256 {
		identifier, err := link.NewIdentifier(constant(b))
		if errors.Is(err, link.ErrRandomSource) {
			discarded++
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Trim(identifier, identifier[:1]) != "" {
			t.Fatalf("byte %d gave %q, want one symbol repeated", b, identifier)
		}
		values[identifier[0]]++
	}
	if discarded != 13 {
		t.Errorf("%d byte values discarded, want 13", discarded)
	}
	if len(values) != len(alphabet) {
		t.Errorf("%d symbols produced, want %d", len(values), len(alphabet))
	}
	for symbol, count := range values {
		if count != 9 {
			t.Errorf("symbol %q comes from %d byte values, want 9", symbol, count)
		}
	}
}

func TestTheGeneratorReportsASourceItCannotRead(t *testing.T) {
	if _, err := link.NewIdentifier(failing{}); !errors.Is(err, errNoEntropy) {
		t.Errorf("err = %v, want it to wrap the error of the source", err)
	}
}

func TestWellFormedIdentifier(t *testing.T) {
	cases := []struct {
		name       string
		identifier string
		want       bool
	}{
		{"three characters", "abc", true},
		{"forty characters", strings.Repeat("a", 40), true},
		{"digits only", "2026", true},
		{"a hyphen inside", "black-friday", true},
		{"two hyphens together", "a--b", true},
		{"a generated identifier", "k7m2xq9d", true},
		{"empty", "", false},
		{"two characters", "ab", false},
		{"forty-one characters", strings.Repeat("a", 41), false},
		{"a hyphen at the start", "-abc", false},
		{"a hyphen at the end", "abc-", false},
		{"upper case", "Abc", false},
		{"an underscore", "a_b", false},
		{"a dot", "a.b", false},
		{"a slash", "a/b", false},
		{"a space", "a b", false},
		{"a percent escape", "a%20b", false},
		{"outside ASCII", "a\u00e9b", false},
		{"a path of the host", ".well-known", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := link.WellFormedIdentifier(c.identifier); got != c.want {
				t.Errorf("WellFormedIdentifier(%q) = %v, want %v", c.identifier, got, c.want)
			}
		})
	}
}

func TestParseAlias(t *testing.T) {
	cases := []struct {
		name  string
		alias string
		want  error
	}{
		{"accepted", "black-friday", nil},
		{"the shortest", "abc", nil},
		{"the longest", strings.Repeat("a", 40), nil},
		{"too short", "ab", link.ErrAliasLength},
		{"too long", strings.Repeat("a", 41), link.ErrAliasLength},
		{"upper case is refused, not lowered", "Black-Friday", link.ErrAliasCharacters},
		{"a hyphen at the end", "black-", link.ErrAliasCharacters},
		{"a character outside the grammar", "black_friday", link.ErrAliasCharacters},
		{"a path the host may serve", "status", link.ErrAliasReserved},
		{"a word that reads as a page of the service", "login", link.ErrAliasReserved},
		{"a reserved word inside a longer alias", "login-help", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := link.ParseAlias(c.alias)
			if !errors.Is(err, c.want) {
				t.Fatalf("ParseAlias(%q) error = %v, want %v", c.alias, err, c.want)
			}
			if err == nil && got != c.alias {
				t.Errorf("ParseAlias(%q) = %q, want the input unchanged", c.alias, got)
			}
			if err != nil && got != "" {
				t.Errorf("ParseAlias(%q) = %q with an error, want nothing", c.alias, got)
			}
		})
	}
}

// Whatever the input, the two functions agree with the grammar of the plan.
func FuzzIdentifierGrammar(f *testing.F) {
	for _, seed := range []string{"abc", "black-friday", "-abc", "Abc", "a.b", "login", "", "a\u00e9b", "a\x00b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		wellFormed := link.WellFormedIdentifier(s)
		if wellFormed != grammar.MatchString(s) {
			t.Fatalf("WellFormedIdentifier(%q) = %v, and the grammar says the opposite", s, wellFormed)
		}
		alias, err := link.ParseAlias(s)
		if err == nil && (!wellFormed || alias != s) {
			t.Fatalf("ParseAlias(%q) accepted %q, which is not the input or not well formed", s, alias)
		}
		if err != nil && !wellFormed && errors.Is(err, link.ErrAliasReserved) {
			t.Fatalf("ParseAlias(%q) calls reserved something that is not even well formed", s)
		}
	})
}
