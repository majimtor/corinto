// Package redirect serves the host of the short links. What it answers is
// the contract in contract/redirect.md, and nothing here decides anything
// that file does not state.
package redirect

import (
	"bytes"
	"crypto/sha256"
	_ "embed" // the 404 page is part of the binary
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/resolve"
)

const (
	// projectPage is where the root of the host leads.
	projectPage = "https://github.com/majimtor/corinto"
	// securityFor is how long the security.txt of a process is good for,
	// counted from its start. A process that runs for longer without being
	// deployed again says, truthfully, that nobody has reviewed the file.
	securityFor = 90 * 24 * time.Hour
)

// notFoundPage is the one page of every 404.
//
//go:embed notfound.html
var notFoundPage []byte

// notFoundPolicy is the content security policy of the 404 page: nothing
// but its own inline style, allowed by its hash. It is computed from the
// page, so the two cannot drift apart.
var notFoundPolicy = policyOf(notFoundPage)

func policyOf(page []byte) string {
	const opening, closing = "<style>", "</style>"
	start, end := bytes.Index(page, []byte(opening)), bytes.Index(page, []byte(closing))
	if start < 0 || end < start {
		// The page is embedded: one without its style is a broken build.
		panic("the 404 page has no inline style")
	}
	hash := sha256.Sum256(page[start+len(opening) : end])
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

type handler struct {
	links  *resolve.Replica
	logger *slog.Logger
	now    func() time.Time
	// security is the security.txt of this process.
	security []byte
}

// New returns what answers on the host of the short links, from a copy of
// the links that somebody else keeps up to date. now is the clock that
// says whether a link has expired; everything but a test passes time.Now.
func New(links *resolve.Replica, logger *slog.Logger, now func() time.Time) http.Handler {
	security := fmt.Sprintf(
		"Contact: %s/security/advisories/new\nExpires: %s\nPolicy: %s/blob/main/SECURITY.md\nPreferred-Languages: en, es\n",
		projectPage, now().Add(securityFor).UTC().Format(time.RFC3339), projectPage)
	return &handler{links: links, logger: logger, now: now, security: []byte(security)}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	// A link that is disabled stops redirecting for everyone: nothing
	// between this host and the visitor keeps an answer.
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	ctx := r.Context()
	defer func() {
		if recovered := recover(); recovered != nil {
			// Nothing of the request goes to the log: not its path, not
			// where it came from.
			h.logger.ErrorContext(ctx, "a redirect panicked",
				"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// The query string plays no part, and neither does the host.
	switch path := lowerASCII(r.URL.Path); path {
	case "/":
		found(w, projectPage)
	case "/.well-known/security.txt":
		header.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// A visitor that went away cannot be told so.
		_, _ = w.Write(h.security)
	default:
		// The copy is keyed by identifier, so only a path that is exactly a
		// slash and an identifier finds anything.
		if held, known := h.links.Lookup(strings.TrimPrefix(path, "/")); known && held.Redirects(h.now()) {
			found(w, held.Destination)
			return
		}
		// One answer for a link that does not redirect and for one that
		// does not exist: it does not say whether the link ever existed.
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("Content-Security-Policy", notFoundPolicy)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(notFoundPage)
	}
}

// lowerASCII lowers the letters A to Z and leaves every other byte as it
// is. An identifier is written in ASCII; lowering by the rules of Unicode
// would turn characters that are not letters of it into ones that are, and
// a path that names no link would find one.
func lowerASCII(path string) string {
	lowered := []byte(path)
	for i, c := range lowered {
		if c >= 'A' && c <= 'Z' {
			lowered[i] = c + ('a' - 'A')
		}
	}
	return string(lowered)
}

// found answers with a redirect and no body. The helper of the standard
// library writes a page with a link in it, which the contract does not
// have.
func found(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}
