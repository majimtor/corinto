package api

import (
	"net/http"
	"time"

	"github.com/majimtor/corinto/internal/auth"
)

// operations returns what serves each operation of the contract, by the
// identifier the contract gives it.
func operations(document []byte) map[string]http.Handler {
	return map[string]http.Handler{
		"getHealth":   http.HandlerFunc(getHealth),
		"getContract": getContract(document),
		"getKey":      http.HandlerFunc(getKey),
	}
}

// getKey says which key the request was made with: what a client asks to
// check its key before it relies on it.
func getKey(w http.ResponseWriter, r *http.Request) {
	principal := principalOf(r.Context())
	writeJSON(w, http.StatusOK, struct {
		ID          string       `json:"id"`
		WorkspaceID string       `json:"workspace_id"`
		Scopes      []auth.Scope `json:"scopes"`
		ExpiresAt   *time.Time   `json:"expires_at"`
	}{principal.KeyID, principal.WorkspaceID, principal.Scopes, principal.ExpiresAt})
}

// getHealth says that the process answers. Whether it can do its work is
// what the readiness endpoint says, on the port of the probes.
func getHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{"ok"})
}

// getContract serves the document the requests are validated against, byte
// for byte as the binary embeds it.
func getContract(document []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// A client that went away cannot be told so.
		_, _ = w.Write(document)
	})
}
