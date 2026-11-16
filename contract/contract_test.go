package contract_test

import (
	"io/fs"
	"net/url"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/majimtor/corinto/contract"
)

// The document has to load from the files the binary embeds, with nothing
// read from anywhere else, and be valid OpenAPI 3.1.
func TestTheDocumentIsValidOpenAPI31(t *testing.T) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
		return fs.ReadFile(contract.Files, strings.TrimPrefix(location.Path, "/"))
	}

	data, err := contract.Files.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document, err := loader.LoadFromDataWithPath(data, &url.URL{Path: "openapi.yaml"})
	if err != nil {
		t.Fatalf("the document does not load: %v", err)
	}
	if err := document.Validate(t.Context()); err != nil {
		t.Errorf("the document is not valid: %v", err)
	}
	if !strings.HasPrefix(document.OpenAPI, "3.1.") {
		t.Errorf("openapi = %q, want a 3.1 version", document.OpenAPI)
	}
}
