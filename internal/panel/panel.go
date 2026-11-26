// Package panel serves the pages of the panel: HTML that the server renders
// from templates, with no script (ADR 0014). A page reaches the domain
// through the packages the API uses, and holds no query of its own. Who
// asks for a page is said by a session, which a cookie carries (ADR 0012).
package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/netip"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// The templates: one layout, and a file for each page.
//
//go:embed templates/*.html
var templateFiles embed.FS

// stylesheet is the one stylesheet of the panel.
//
//go:embed assets/panel.css
var stylesheet []byte

// policy is the content security policy of every page: nothing but the
// stylesheet of the panel and the forms that post to it. No script, no
// object and no base URL (the engineering rules).
const policy = "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Settings is what the panel takes from the process that serves it.
type Settings struct {
	Logger *slog.Logger
	Links  *link.Store
	// Inbox says how far the events are processed.
	Inbox      *ingest.Store
	Aggregates *rollup.Reader
	// Sessions says who a request acts as.
	Sessions *auth.Sessions
	// Keys are the keys of the API, which a workspace issues and revokes
	// in its panel.
	Keys *auth.Keys
	// Cookie is the form of the session cookie, which the installation
	// chooses.
	Cookie CookieForm
	// TrustedProxies are the proxies whose forwarding header is believed
	// when the address of a client is asked for, which is what the limit
	// of the demo entry counts by.
	TrustedProxies []netip.Prefix
	// Now is the clock of the pages, or nil for the one of the system.
	Now func() time.Time
}

// The pages of the panel, by the file that holds each.
const (
	pageEntry      = "entry.html"
	pageLinks      = "links.html"
	pageLink       = "link.html"
	pageLinkForm   = "linkform.html"
	pageLinkDelete = "linkdelete.html"
	pageKeys       = "keys.html"
	pageKeyForm    = "keyform.html"
	pageKeyIssued  = "keyissued.html"
	pageKeyRevoke  = "keyrevoke.html"
	pageNotFound   = "notfound.html"
	pageFailed     = "failed.html"
	pageRefused    = "refused.html"
	pageLimited    = "limited.html"
	pageBusy       = "busy.html"
	pageInsecure   = "insecure.html"
)

var pages = []string{
	pageEntry, pageLinks, pageLink, pageLinkForm, pageLinkDelete,
	pageKeys, pageKeyForm, pageKeyIssued, pageKeyRevoke,
	pageNotFound, pageFailed, pageRefused, pageLimited, pageBusy, pageInsecure,
}

// panel is the pages of the panel and what they are rendered from.
type panel struct {
	settings Settings
	texts    catalogs
	// pages holds each page parsed once for each language, with the
	// functions that write in it.
	pages map[language]map[string]*template.Template
	// stylesheetPath has the hash of the stylesheet in it, so that a
	// browser may keep it for good: another stylesheet is another path.
	stylesheetPath string
	// entries counts the sessions each client address opens, by a key
	// that addresses gives it and that cannot be turned back into one.
	addresses *limits.Addresses
	entries   *limits.Limiter
	// writes counts the changes each session makes.
	writes *limits.Limiter
}

// New returns the handler of the pages of the panel. It serves until ctx is
// done, which is when it stops forgetting the addresses it limited.
func New(ctx context.Context, settings Settings) (http.Handler, error) {
	served, err := newPanel(ctx, settings)
	if err != nil {
		return nil, err
	}
	return served.handler(), nil
}

// newPanel reads what the pages are rendered from.
func newPanel(ctx context.Context, settings Settings) (*panel, error) {
	if settings.Now == nil {
		settings.Now = time.Now
	}
	if settings.Cookie != CookieSecure && settings.Cookie != CookieLoopback {
		return nil, fmt.Errorf("the form of the session cookie is %q, and it is %q or %q", settings.Cookie, CookieSecure, CookieLoopback)
	}
	texts, err := readCatalogs()
	if err != nil {
		return nil, err
	}
	addresses, err := limits.NewAddresses()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(stylesheet)
	served := &panel{
		settings: settings, texts: texts,
		pages:          make(map[language]map[string]*template.Template, len(languages)),
		stylesheetPath: "/assets/panel-" + hex.EncodeToString(hash[:8]) + ".css",
		addresses:      addresses,
		entries:        limits.NewLimiter(limits.DemoEntriesPerMinute, time.Minute, limits.DemoEntriesPerMinute, settings.Now),
		writes:         limits.NewLimiter(limits.APIWritesPerMinute, time.Minute, limits.APIWritesPerMinute, settings.Now),
	}
	go served.entries.Sweep(ctx)
	go served.writes.Sweep(ctx)
	for _, lang := range languages {
		served.pages[lang] = make(map[string]*template.Template)
		for _, page := range pages {
			parsed, err := template.New("layout").Funcs(served.functions(lang)).
				ParseFS(templateFiles, "templates/layout.html", "templates/"+page)
			if err != nil {
				return nil, fmt.Errorf("parse the page %s: %w", page, err)
			}
			served.pages[lang][page] = parsed
		}
	}
	return served, nil
}

// handler routes the paths of the panel to what serves them.
func (p *panel) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p.stylesheetPath, p.serveStylesheet)
	mux.HandleFunc("GET /{$}", p.root)
	for _, lang := range languages {
		door := "/" + string(lang) + "/"
		mux.HandleFunc("GET /"+string(lang), func(w http.ResponseWriter, _ *http.Request) {
			redirect(w, door, http.StatusPermanentRedirect)
		})
		mux.HandleFunc("GET "+door+"{$}", p.entryPage(lang))
		mux.HandleFunc("POST "+door+"session", p.enter(lang))
		mux.HandleFunc("POST "+door+"logout", p.leave(lang))
		mux.HandleFunc("GET "+door+"links", p.inside(lang, p.linksPage(lang)))
		mux.HandleFunc("POST "+door+"links", p.changing(lang, p.createLink(lang)))
		mux.HandleFunc("GET "+door+"links/new", p.inside(lang, p.newLinkPage(lang)))
		mux.HandleFunc("GET "+door+"links/{id}", p.inside(lang, p.linkPage(lang)))
		mux.HandleFunc("POST "+door+"links/{id}", p.changing(lang, p.updateLink(lang)))
		mux.HandleFunc("GET "+door+"links/{id}/edit", p.inside(lang, p.editLinkPage(lang)))
		mux.HandleFunc("POST "+door+"links/{id}/disable", p.changing(lang, p.switchLink(lang, (*link.Store).Disable)))
		mux.HandleFunc("POST "+door+"links/{id}/enable", p.changing(lang, p.switchLink(lang, (*link.Store).Enable)))
		mux.HandleFunc("GET "+door+"links/{id}/delete", p.inside(lang, p.deleteLinkPage(lang)))
		mux.HandleFunc("POST "+door+"links/{id}/delete", p.changing(lang, p.deleteLink(lang)))
		mux.HandleFunc("GET "+door+"keys", p.inside(lang, p.keysPage(lang)))
		mux.HandleFunc("POST "+door+"keys", p.changing(lang, p.issueKey(lang)))
		mux.HandleFunc("GET "+door+"keys/new", p.inside(lang, p.newKeyPage(lang)))
		mux.HandleFunc("GET "+door+"keys/{id}/revoke", p.inside(lang, p.revokeKeyPage(lang)))
		mux.HandleFunc("POST "+door+"keys/{id}/revoke", p.changing(lang, p.revokeKey(lang)))
	}
	// Every other path and every other method: there is no page there.
	mux.HandleFunc("/", p.notFound)

	// Nothing changes state on a GET, and a request that may change it
	// comes from a page of the panel itself or is refused (ADR 0012).
	sameOrigin := http.NewCrossOriginProtection()
	sameOrigin.SetDenyHandler(http.HandlerFunc(p.refused))
	return p.observe(secure(p.onePath(sameOrigin.Handler(mux))))
}

// note is what a request leaves for the line of the log that names it.
type note struct {
	id string
	// session names the session of the request, when it brought one.
	session string
}

type noteKey struct{}

// noteOf returns the note of a request.
func noteOf(ctx context.Context) *note {
	left, _ := ctx.Value(noteKey{}).(*note)
	if left == nil {
		// Only a handler called outside observe gets here: a test.
		return &note{}
	}
	return left
}

// recorder remembers the status of a response for the log.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

// Unwrap gives http.ResponseController the writer underneath.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe is the outermost layer of every request to the panel. It gives
// the request an identifier, turns a panic into the page of a failure and
// logs one line, with the fields of the line the API logs (internal/api,
// observe): the two write the same log and share no code, because what
// each answers to a failure differs.
func (p *panel) observe(next http.Handler) http.Handler {
	logger := p.settings.Logger
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Generated here and never taken from the client.
		id := rand.Text()
		left := &note{id: id}
		ctx := context.WithValue(r.Context(), noteKey{}, left)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", id)

		response := &recorder{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(ctx, "a page panicked",
					"request", id, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				if response.status == 0 {
					lang := languageOf(r)
					p.render(response, r, http.StatusInternalServerError, lang, pageFailed, p.viewOf(r, lang))
				}
			}
			// The pattern of the route, never the path: a path carries
			// identifiers, and a query string is not logged at all.
			route := r.Pattern
			if route == "/" {
				route = ""
			}
			line := []any{
				"request", id, "method", r.Method, "route", route,
				"status", response.status, "duration", time.Since(start).String(),
			}
			if left.session != "" {
				line = append(line, "session", left.session)
			}
			logger.InfoContext(ctx, "request", line...)
		}()
		next.ServeHTTP(response, r)
	})
}

// onePath answers that there is no page at a path with an empty segment or
// a dot segment. The mux would redirect it to its clean form with a page of
// its own, which no catalogue wrote.
func (p *panel) onePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked := r.URL.Path
		// The door of a language ends in a slash, which cleaning takes away.
		if clean := path.Clean(asked); !strings.HasPrefix(asked, "/") || (asked != clean && asked != clean+"/") {
			p.notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// redirect sends a request elsewhere in the panel. It writes no body: the
// one the standard library writes has words of its own.
func redirect(w http.ResponseWriter, to string, status int) {
	header := w.Header()
	header.Set("Location", to)
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}

// secure writes the headers every response of the panel carries.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", policy)
		header.Set("X-Content-Type-Options", "nosniff")
		// A page names a link in its address and shows a destination that
		// leads outside: neither is told to the site it leads to.
		header.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// functions returns what a template writes with, in one language. Every
// text of a page comes through them from a catalogue.
func (p *panel) functions(lang language) template.FuncMap {
	return template.FuncMap{
		"t": func(key string, pairs ...any) (string, error) { return p.texts.text(lang, key, pairs...) },
		"plural": func(key string, n int64, pairs ...any) (string, error) {
			return p.texts.plural(lang, key, n, pairs...)
		},
		"number":  func(n int64) string { return p.texts.number(lang, n) },
		"percent": func(part, whole int64) (string, error) { return p.texts.percent(lang, part, whole) },
		"day":     func(at time.Time) (string, error) { return p.texts.day(lang, at) },
		"date":    func(at time.Time) (string, error) { return p.texts.date(lang, at) },
		"moment":  func(at time.Time) (string, error) { return p.texts.moment(lang, at) },
		// What a time element carries for a program: the day, as ISO 8601
		// writes it.
		"isoday": func(at time.Time) string { return at.UTC().Format(time.DateOnly) },
	}
}

// view is what every page is rendered with.
type view struct {
	// Lang is the language of the page, and Other the one it can be read
	// in instead, at OtherPath and under the name OtherName gives it.
	Lang, Other language
	OtherPath   string
	OtherName   string
	Stylesheet  string
	// Home is the door of the panel in the language of the page.
	Home string
	// Workspace is the name of the workspace the request acts in, or empty
	// when it acts in none.
	Workspace string
	// Bar is where the bar of a page behind a session leads. It is a field
	// of its own: a page that shows its links or its keys under those
	// names would otherwise take the place of where the bar leads.
	Bar bar
}

// bar is where the bar of a page behind a session leads: the links of the
// workspace, its keys, and where its session is closed.
type bar struct {
	Links, Keys, Leave string
}

// viewOf returns what every page of a request is rendered with. The other
// language is at the same path, with its own first segment.
func (p *panel) viewOf(r *http.Request, lang language) view {
	other := spanish
	if lang == spanish {
		other = english
	}
	rest := strings.TrimPrefix(r.URL.Path, "/"+string(lang))
	if rest == r.URL.Path {
		// A path with no language in it, which only the page of what is
		// not found can have.
		rest = "/"
	}
	door := "/" + string(lang) + "/"
	return view{
		Lang: lang, Other: other, OtherPath: "/" + string(other) + rest,
		OtherName: p.texts.plain(other, "language.name"), Stylesheet: p.stylesheetPath,
		Home: door, Bar: bar{Links: door + "links", Keys: door + "keys", Leave: door + "logout"},
	}
}

// languageOf returns the language the first segment of a path names, or
// Spanish when it names none.
func languageOf(r *http.Request) language {
	for _, known := range languages {
		if r.URL.Path == "/"+string(known) || strings.HasPrefix(r.URL.Path, "/"+string(known)+"/") {
			return known
		}
	}
	return spanish
}

// render writes a page. It is rendered whole before a byte is sent, so a
// page that fails halfway is answered as a failure and not as half a page.
func (p *panel) render(w http.ResponseWriter, r *http.Request, status int, lang language, page string, shown any) {
	var body bytes.Buffer
	if err := p.pages[lang][page].ExecuteTemplate(&body, "layout", shown); err != nil {
		if page == pageFailed {
			// The page that says so cannot be rendered either.
			p.settings.Logger.ErrorContext(r.Context(), "the page of a failure could not be rendered", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		p.fail(w, r, lang, fmt.Errorf("render %s: %w", page, err))
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	// A page shows what a workspace holds: no cache keeps it.
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

// fail answers a request the panel could not serve, and logs why.
func (p *panel) fail(w http.ResponseWriter, r *http.Request, lang language, err error) {
	// The pattern of the route and never the path, as in the line of the
	// request, which the identifier leads to.
	p.settings.Logger.ErrorContext(r.Context(), "a page of the panel failed",
		"request", noteOf(r.Context()).id, "route", r.Pattern, "error", err)
	p.render(w, r, http.StatusInternalServerError, lang, pageFailed, p.viewOf(r, lang))
}

// missing answers a request for what a workspace does not have. A link of
// another workspace is answered like one that does not exist.
func (p *panel) missing(w http.ResponseWriter, r *http.Request, lang language) {
	p.render(w, r, http.StatusNotFound, lang, pageNotFound, p.viewOf(r, lang))
}

// notFound answers every path that is not a page, in the language its
// first segment names, or in Spanish when it names none.
func (p *panel) notFound(w http.ResponseWriter, r *http.Request) {
	p.missing(w, r, languageOf(r))
}

// refused answers a request that could change something and does not come
// from a page of the panel.
func (p *panel) refused(w http.ResponseWriter, r *http.Request) {
	lang := languageOf(r)
	p.render(w, r, http.StatusForbidden, lang, pageRefused, p.viewOf(r, lang))
}

func (p *panel) serveStylesheet(w http.ResponseWriter, _ *http.Request) {
	header := w.Header()
	header.Set("Content-Type", "text/css; charset=utf-8")
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(stylesheet)
}
