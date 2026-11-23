package auth

import (
	"crypto/rand"
	"strings"
	"testing"
)

// A token is refused by its form before the database is asked: only the
// 52 characters of a secret in its alphabet are one.
func TestOnlyTheExactFormIsAToken(t *testing.T) {
	token, err := newSecret(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !wellFormedToken(token) {
		t.Fatalf("a token as it is made does not have the form of one (%d characters)", len(token))
	}
	for name, presented := range map[string]string{
		"nothing":                       "",
		"a token in upper case":         strings.ToUpper(token),
		"a token one character short":   token[1:],
		"a token one character long":    token + "a",
		"a digit outside its alphabet":  token[:51] + "1",
		"a hyphen":                      token[:51] + "-",
		"a space where a character was": token[:51] + " ",
		"a line break at its end":       token[:51] + "\n",
		"a letter that is not ASCII":    token[:50] + "ß",
	} {
		if wellFormedToken(presented) {
			t.Errorf("%s has the form of a token", name)
		}
	}
}
