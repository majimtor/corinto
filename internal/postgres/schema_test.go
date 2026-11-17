package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// A migration is immutable once merged. migrations/SUMS holds the checksum
// of each one, so that editing a merged migration fails here unless its line
// is edited too, which a review sees.
func TestEveryMigrationMatchesItsChecksum(t *testing.T) {
	sums, err := os.ReadFile("migrations/SUMS")
	if err != nil {
		t.Fatal(err)
	}
	recorded := make(map[string]string)
	for line := range strings.Lines(string(sums)) {
		sum, name, found := strings.Cut(strings.TrimSpace(line), "  ")
		if !found {
			t.Fatalf("a line of migrations/SUMS is not a checksum, two spaces and a name: %q", line)
		}
		recorded[name] = sum
	}

	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		content, err := fs.ReadFile(migrations(), entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		switch recorded[entry.Name()] {
		case hex.EncodeToString(sum[:]):
		case "":
			t.Errorf("%s has no line in migrations/SUMS; a new migration adds one", entry.Name())
		default:
			t.Errorf("%s does not match its checksum: a merged migration is not edited, a correction is a new migration", entry.Name())
		}
		delete(recorded, entry.Name())
	}
	for name := range recorded {
		t.Errorf("migrations/SUMS names %s, which is not embedded", name)
	}
}

// The versions are 1, 2, 3 and so on, with no gap and no repeat, so that the
// order in which migrations run is the order in which they were merged.
func TestTheMigrationsAreNumberedInARow(t *testing.T) {
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		want := fmt.Sprintf("%05d_", i+1)
		if !strings.HasPrefix(entry.Name(), want) || !strings.HasSuffix(entry.Name(), ".sql") {
			t.Errorf("migration %d is named %q, want it to start with %q and end in .sql", i+1, entry.Name(), want)
		}
	}
	built, err := BuiltVersion()
	if err != nil {
		t.Fatal(err)
	}
	if built != int64(len(entries)) {
		t.Errorf("BuiltVersion() = %d with %d migrations", built, len(entries))
	}
}
