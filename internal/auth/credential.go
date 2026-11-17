package auth

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// A credential is the text the holder of a key sends:
//
//	cor_<identifier>_<secret>
//
// The identifier is that of the row of the key, in hexadecimal, so that the
// key is found by its primary key and no search goes by the secret. The
// secret is 32 random bytes in base32. Both alphabets are lower case and
// have neither a hyphen nor an underscore: the text splits in one way only
// and a double click selects all of it. The fixed prefix lets a secret
// scanner recognise one.
const (
	credentialPrefix = "cor_"
	// identifierLength is that of the 16 bytes of an identifier in
	// hexadecimal.
	identifierLength = 32
	// secretBytes is the size of a secret: 256 bits, far beyond guessing,
	// which is why a plain hash of it is enough to store.
	secretBytes = 32
	// secretLength is that of 32 bytes in base32 without padding.
	secretLength = 52

	hexadecimal    = "0123456789abcdef"
	secretAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
)

// ErrRandomSource means that the source of randomness gave no secret.
var ErrRandomSource = errors.New("the random source failed")

var secretEncoding = base32.NewEncoding(secretAlphabet).WithPadding(base32.NoPadding)

// newSecret returns a secret read from random.
func newSecret(random io.Reader) (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("%w: %w", ErrRandomSource, err)
	}
	return secretEncoding.EncodeToString(raw), nil
}

// credential returns the text for the key of a row and its secret.
func credential(id pgtype.UUID, secret string) string {
	return credentialPrefix + hex.EncodeToString(id.Bytes[:]) + "_" + secret
}

// parseCredential splits a credential into the identifier of its key and
// its secret. Only the exact form passes: a key has one text, and anything
// else is refused before the database is asked.
func parseCredential(presented string) (pgtype.UUID, string, bool) {
	rest, found := strings.CutPrefix(presented, credentialPrefix)
	if !found || len(rest) != identifierLength+1+secretLength || rest[identifierLength] != '_' {
		return pgtype.UUID{}, "", false
	}
	identifier, secret := rest[:identifierLength], rest[identifierLength+1:]
	if strings.Trim(identifier, hexadecimal) != "" || strings.Trim(secret, secretAlphabet) != "" {
		return pgtype.UUID{}, "", false
	}
	id := pgtype.UUID{Valid: true}
	if _, err := hex.Decode(id.Bytes[:], []byte(identifier)); err != nil {
		return pgtype.UUID{}, "", false
	}
	return id, secret, true
}
