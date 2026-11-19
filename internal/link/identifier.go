// Package link holds the link of the domain: its short identifier, its
// destination and the rules both must meet. Nothing here touches the
// database.
package link

import (
	"errors"
	"fmt"
	"io"
)

const (
	// alphabet is what a generated identifier is made of. It has no vowels,
	// so that no word appears; no 0, 1 or l, which are confused when read
	// aloud; and no upper case, which some channels alter.
	alphabet = "23456789bcdfghjkmnpqrstvwxz"

	// generatedLength gives 27^8 identifiers, about 2.8 × 10^11: the length,
	// not a rate limit, is what makes guessing one impractical.
	generatedLength = 8

	// Bounds of an identifier chosen by a workspace.
	minAliasLength = 3
	maxAliasLength = 40

	// unbiasedLimit is the largest multiple of the alphabet size that fits in
	// a byte. A random byte at or above it is discarded: reducing it modulo
	// the alphabet size would make the first symbols more likely.
	unbiasedLimit = 256 - 256%len(alphabet)

	// maxRandomReads bounds the reads for one identifier. One read is almost
	// always enough; a source that needs more than this is broken, and the
	// generator says so instead of reading forever.
	maxRandomReads = 16
)

// ErrRandomSource means that the source of randomness kept returning bytes
// that had to be discarded.
var ErrRandomSource = errors.New("the random source returns no usable bytes")

// Why an alias is refused. The API maps each one to a code of the contract.
var (
	ErrAliasLength     = errors.New("an alias has between 3 and 40 characters")
	ErrAliasCharacters = errors.New("an alias is made of a-z, 0-9 and hyphens, with no hyphen at either end")
	ErrAliasReserved   = errors.New("the alias is reserved")
)

// NewIdentifier returns a generated identifier: eight symbols of the
// alphabet, each equally likely. random is the source of randomness;
// everything but a test passes crypto/rand.Reader.
func NewIdentifier(random io.Reader) (string, error) {
	identifier := make([]byte, 0, generatedLength)
	// A few bytes more than needed, since about one in twenty is discarded.
	buffer := make([]byte, 2*generatedLength)
	for range maxRandomReads {
		if _, err := io.ReadFull(random, buffer); err != nil {
			return "", fmt.Errorf("read random bytes for an identifier: %w", err)
		}
		for _, b := range buffer {
			if int(b) >= unbiasedLimit {
				continue
			}
			identifier = append(identifier, alphabet[int(b)%len(alphabet)])
			if len(identifier) == generatedLength {
				return string(identifier), nil
			}
		}
	}
	return "", ErrRandomSource
}

// WellFormedIdentifier reports whether s can be the identifier of a link at
// all, generated or chosen.
func WellFormedIdentifier(s string) bool {
	if len(s) < minAliasLength || len(s) > maxAliasLength {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// ParseAlias checks an identifier chosen by a workspace and returns it. It
// does not lower the case of its input: an alias is written in lower case,
// and one that is not is refused instead of silently changed.
func ParseAlias(s string) (string, error) {
	if len(s) < minAliasLength || len(s) > maxAliasLength {
		return "", ErrAliasLength
	}
	if !WellFormedIdentifier(s) {
		return "", ErrAliasCharacters
	}
	if _, found := reserved[s]; found {
		return "", ErrAliasReserved
	}
	return s, nil
}
