// Package redirect serves the host of the short links. What it answers is
// the contract in contract/redirect.md, and nothing here decides anything
// that file does not state.
package redirect

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed" // the 404 page is part of the binary
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
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

// Settings is what the host of the short links takes from the process that
// runs it.
type Settings struct {
	// Links is the copy of the links, which somebody else keeps up to date.
	Links  *resolve.Replica
	Logger *slog.Logger
	// Now is the clock that says whether a link has expired and how fast a
	// client asks. Everything but a test passes time.Now.
	Now func() time.Time
	// TrustedProxies are the proxies whose forwarding header is believed
	// when the address of a client is asked for. Empty trusts none.
	TrustedProxies []netip.Prefix
	// Rules say what a request is, from its method and its headers.
	Rules *classify.Classifier
	// Events takes what the responses leave behind.
	Events Recorder
}

// Recorder takes the event of each response that owes one and counts the
// requests that owe none. It never makes a response wait: *ingest.Buffer,
// the one of the process, keeps what it is given in memory.
type Recorder interface {
	Record(event ingest.Event)
	Count(name ingest.Counter, at time.Time)
}

type handler struct {
	Settings
	// requests counts what each client address asks for.
	requests  *limits.Limiter
	addresses *limits.Addresses
	// security is the security.txt of this process.
	security []byte
}

// New returns what answers on the host of the short links. What it starts
// stops when ctx is done.
func New(ctx context.Context, settings Settings) (http.Handler, error) {
	addresses, err := limits.NewAddresses()
	if err != nil {
		return nil, err
	}
	requests := limits.NewLimiter(limits.RedirectRequestsPerSecond, time.Second, limits.RedirectBurst, settings.Now)
	go requests.Sweep(ctx)
	security := fmt.Sprintf(
		"Contact: %s/security/advisories/new\nExpires: %s\nPolicy: %s/blob/main/SECURITY.md\nPreferred-Languages: en, es\n",
		projectPage, settings.Now().Add(securityFor).UTC().Format(time.RFC3339), projectPage)
	return &handler{Settings: settings, requests: requests, addresses: addresses, security: []byte(security)}, nil
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
			h.Logger.ErrorContext(ctx, "a redirect panicked",
				"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	// Every request counts against the address it comes from, whatever it
	// asks for: whoever tries identifiers one after another is told to wait
	// like anyone else, before anything is looked up. Only a hash of the
	// address is kept, and only while it says something.
	client := h.addresses.Key(limits.ClientAddress(r, h.TrustedProxies))
	if wait := h.requests.Take(client); wait > 0 {
		// In whole seconds, rounded up: sooner would be refused again.
		header.Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		w.WriteHeader(http.StatusTooManyRequests)
		h.leave(ctx, func(at time.Time) { h.Events.Count(ingest.CounterRateLimited, at) })
		return
	}

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
		held, known := h.Links.Lookup(strings.TrimPrefix(path, "/"))
		if known && held.Redirects(h.Now()) {
			found(w, held.Destination)
			h.leave(ctx, func(at time.Time) { h.Events.Record(h.event(r, held, ingest.OutcomeRedirected, at)) })
			return
		}
		// One answer for a link that does not redirect and for one that
		// does not exist: it does not say whether the link ever existed.
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("Content-Security-Policy", notFoundPolicy)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(notFoundPage)
		// What it leaves behind does say so, to whoever owns the link: a
		// link that stopped redirecting owes an event, and a path that
		// names none is only counted.
		if known {
			h.leave(ctx, func(at time.Time) { h.Events.Record(h.event(r, held, ingest.OutcomeInactive, at)) })
		} else {
			h.leave(ctx, func(at time.Time) { h.Events.Count(ingest.CounterNotFound, at) })
		}
	}
}

// event describes an access to a link in what an event may carry
// (PLAN.md section 5). The user agent is read here, to be classified, and
// goes no further; of the referrer only the host does.
func (h *handler) event(r *http.Request, held resolve.Link, outcome ingest.Outcome, at time.Time) ingest.Event {
	return ingest.Event{
		LinkID: held.ID, WorkspaceID: held.WorkspaceID, OccurredAt: at,
		Outcome: outcome, Method: r.Method,
		ReferrerHost:   ingest.ReferrerHost(r.Header.Get("Referer")),
		Classification: h.Rules.Classify(r.Method, r.Header),
	}
}

// leave hands over what a response leaves behind, once the response is
// written. The redirect always answers, and analytics give way: a failure
// here is logged and changes nothing of what the visitor got.
func (h *handler) leave(ctx context.Context, behind func(at time.Time)) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.Logger.ErrorContext(ctx, "what a response leaves behind could not be handed over",
				"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
		}
	}()
	behind(h.Now())
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
