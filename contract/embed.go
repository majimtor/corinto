// Package contract holds the contracts of the project and embeds them, so
// that the binary serves and checks the same files a reader sees here.
package contract

import "embed"

// Files holds the OpenAPI document of the management API.
//
//go:embed openapi.yaml
var Files embed.FS
