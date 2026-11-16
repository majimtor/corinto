package link

import "testing"

// A reserved word that the grammar already refuses would be a dead entry:
// nobody could ask for it.
func TestEveryReservedWordCouldBeAnAlias(t *testing.T) {
	if len(reserved) == 0 {
		t.Fatal("the reserved list is empty")
	}
	for word := range reserved {
		if !WellFormedIdentifier(word) {
			t.Errorf("%q is reserved and could never be an alias", word)
		}
	}
}
