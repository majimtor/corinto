package contract_test

import (
	"strings"
	"testing"

	"github.com/majimtor/corinto/contract"
)

// The document has to load from the files the binary embeds, with nothing
// read from anywhere else, and be valid OpenAPI 3.1.
func TestTheDocumentIsValidOpenAPI31(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(document.OpenAPI, "3.1.") {
		t.Errorf("openapi = %q, want a 3.1 version", document.OpenAPI)
	}
}
