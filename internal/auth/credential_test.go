package auth

import (
	"bytes"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/jackc/pgx/v5/pgtype"
)

// The form of a credential, written here a second time on purpose: the
// tests must not take it from the code they check.
var credentialForm = regexp.MustCompile(`^cor_[0-9a-f]{32}_[a-z2-7]{52}$`)

// example returns a credential made for this run, with its parts. No
// credential is written in a test, not even one that opens nothing.
func example(t testing.TB) (pgtype.UUID, string, string) {
	t.Helper()
	id := pgtype.UUID{Valid: true}
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		t.Fatal(err)
	}
	secret, err := newSecret(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return id, secret, credential(id, secret)
}

func TestACredentialHasTheStatedForm(t *testing.T) {
	seen := make(map[string]bool)
	for range 200 {
		_, secret, text := example(t)
		if !credentialForm.MatchString(text) {
			t.Fatalf("a credential does not have the stated form (%d characters)", len(text))
		}
		if seen[secret] {
			t.Fatal("the same secret was generated twice")
		}
		seen[secret] = true
	}
}

func TestACredentialSplitsBackIntoItsParts(t *testing.T) {
	for range 200 {
		id, secret, text := example(t)
		gotID, gotSecret, ok := parseCredential(text)
		if !ok || gotID != id || gotSecret != secret {
			t.Fatalf("parseCredential did not return the parts the credential was made from (ok = %v)", ok)
		}
	}
}

func TestOnlyTheExactFormIsACredential(t *testing.T) {
	id, secret, text := example(t)
	identifier := text[len("cor_") : len("cor_")+32]
	cases := []struct {
		name      string
		presented string
	}{
		{"nothing", ""},
		{"the prefix alone", "cor_"},
		{"no prefix", identifier + "_" + secret},
		{"the prefix in upper case", "COR_" + identifier + "_" + secret},
		{"another prefix", "key_" + identifier + "_" + secret},
		{"an identifier with hyphens", "cor_" + id.String() + "_" + secret},
		{"an identifier in upper case", "cor_" + strings.ToUpper(identifier) + "_" + secret},
		{"an identifier one character short", "cor_" + identifier[1:] + "_" + secret},
		{"an identifier one character long", "cor_" + identifier + "0_" + secret},
		{"an identifier that is not hexadecimal", "cor_" + identifier[:31] + "g_" + secret},
		{"no separator", "cor_" + identifier + secret},
		{"another separator", "cor_" + identifier + "-" + secret},
		{"a secret one character short", "cor_" + identifier + "_" + secret[1:]},
		{"a secret one character long", text + "a"},
		{"a secret in upper case", "cor_" + identifier + "_" + strings.ToUpper(secret)},
		{"a secret with a digit outside its alphabet", "cor_" + identifier + "_" + secret[:51] + "1"},
		{"a secret with padding", "cor_" + identifier + "_" + secret[:51] + "="},
		{"a space before", " " + text},
		{"a line break after", text + "\n"},
		{"the scheme of the header", "Bearer " + text},
		{"two credentials", text + text},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, ok := parseCredential(c.presented); ok {
				t.Error("parseCredential accepted it")
			}
		})
	}
}

func TestASecretTakesThirtyTwoBytesFromTheSource(t *testing.T) {
	source := bytes.NewReader(bytes.Repeat([]byte{0xa5}, 40))
	secret, err := newSecret(source)
	if err != nil {
		t.Fatal(err)
	}
	if source.Len() != 8 {
		t.Errorf("the secret took %d bytes, want 32", 40-source.Len())
	}
	if len(secret) != 52 || strings.Trim(secret, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Errorf("the secret has %d characters or one outside base32, want 52 of it", len(secret))
	}
}

func TestASourceThatFailsGivesNoSecret(t *testing.T) {
	sources := map[string]*bytes.Reader{
		"a source that ends early": bytes.NewReader(make([]byte, 31)),
		"an empty source":          bytes.NewReader(nil),
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			if secret, err := newSecret(source); !errors.Is(err, ErrRandomSource) || secret != "" {
				t.Errorf("err = %v with a secret of %d characters, want %v and none", err, len(secret), ErrRandomSource)
			}
		})
	}
	t.Run("a source that returns an error", func(t *testing.T) {
		broken := errors.New("no entropy")
		if _, err := newSecret(iotest.ErrReader(broken)); !errors.Is(err, ErrRandomSource) || !errors.Is(err, broken) {
			t.Errorf("err = %v, want %v wrapping the error of the source", err, ErrRandomSource)
		}
	})
}

// Whatever is accepted is the one text of its key: built again from its
// parts it is the same, so no two texts name the same key and secret.
func FuzzParseCredential(f *testing.F) {
	_, _, text := example(f)
	f.Add(text)
	f.Add(strings.ToUpper(text))
	f.Add(text[:len(text)-1])
	f.Add("cor__")
	f.Add("")
	f.Fuzz(func(t *testing.T, presented string) {
		id, secret, ok := parseCredential(presented)
		if !ok {
			return
		}
		if !credentialForm.MatchString(presented) {
			t.Errorf("accepted a text of %d characters that does not have the stated form", len(presented))
		}
		if credential(id, secret) != presented {
			t.Error("the parts of an accepted credential do not build it again")
		}
	})
}
