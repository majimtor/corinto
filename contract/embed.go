// Package contract holds the contracts of the project and embeds them, so
// that the binary serves and checks the same files a reader sees here.
package contract

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// document is the file of the management API.
const document = "openapi.yaml"

// Files holds the OpenAPI document of the management API.
//
//go:embed openapi.yaml
var Files embed.FS

// Document returns the OpenAPI document as it is written.
func Document() ([]byte, error) {
	return Files.ReadFile(document)
}

// Load reads the OpenAPI document from the embedded files, with nothing
// taken from anywhere else, and fails unless it is valid. The server and the
// tests that check its answers both load it here, so that they read the
// same contract in the same way.
func Load(ctx context.Context) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	// A reference to another file is read from the embedded files too.
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
		return fs.ReadFile(Files, strings.TrimPrefix(location.Path, "/"))
	}
	data, err := Document()
	if err != nil {
		return nil, fmt.Errorf("read the contract: %w", err)
	}
	loaded, err := loader.LoadFromDataWithPath(data, &url.URL{Path: document})
	if err != nil {
		return nil, fmt.Errorf("load the contract: %w", err)
	}
	if err := loaded.Validate(ctx); err != nil {
		return nil, fmt.Errorf("the contract is not valid: %w", err)
	}
	return loaded, nil
}
