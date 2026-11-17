package api

import "net/http"

// operations returns what serves each operation of the contract, by the
// identifier the contract gives it.
func operations(document []byte) map[string]http.Handler {
	return map[string]http.Handler{
		"getHealth":   http.HandlerFunc(getHealth),
		"getContract": getContract(document),
	}
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
