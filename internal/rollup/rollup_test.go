package rollup

import (
	"os"
	"strings"
	"testing"
)

// docs/delivery-semantics.md is the only source of the parameters. Whoever
// changes one changes it there, and here.
func TestTheBatchOfAConsumerIsTheOneTheDocumentStates(t *testing.T) {
	if BatchSize != 500 {
		t.Errorf("a consumer takes %d events at once", BatchSize)
	}
	document, err := os.ReadFile("../../docs/delivery-semantics.md")
	if err != nil {
		t.Fatal(err)
	}
	if row := "| Consumer batch | 500 events |"; !strings.Contains(string(document), row) {
		t.Errorf("docs/delivery-semantics.md does not have the row %q", row)
	}
}
