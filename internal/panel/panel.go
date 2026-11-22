// Package panel serves the pages of the panel: HTML that the server renders
// from templates, with no script (ADR 0014). A page reaches the domain
// through the packages the API uses, and holds no query of its own.
package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/ingest"
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
	// Now is the clock of the pages, or nil for the one of the system.
	Now func() time.Time
}

// The pages of the panel, by the file that holds each.
const (
	pageLink     = "link.html"
	pageNotFound = "notfound.html"
	pageFailed   = "failed.html"
)

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
}

// New returns the handler of the pages of the panel.
func New(settings Settings) (http.Handler, error) {
	if settings.Now == nil {
		settings.Now = time.Now
	}
	texts, err := readCatalogs()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(stylesheet)
	served := &panel{
		settings: settings, texts: texts,
		pages:          make(map[language]map[string]*template.Template, len(languages)),
		stylesheetPath: "/assets/panel-" + hex.EncodeToString(hash[:8]) + ".css",
	}
	for _, lang := range languages {
		served.pages[lang] = make(map[string]*template.Template)
		for _, page := range []string{pageLink, pageNotFound, pageFailed} {
			parsed, err := template.New("layout").Funcs(served.functions(lang)).
				ParseFS(templateFiles, "templates/layout.html", "templates/"+page)
			if err != nil {
				return nil, fmt.Errorf("parse the page %s: %w", page, err)
			}
			served.pages[lang][page] = parsed
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+served.stylesheetPath, served.serveStylesheet)
	for _, lang := range languages {
		mux.HandleFunc("GET /"+string(lang)+"/links/{id}", served.linkPage(lang))
	}
	mux.HandleFunc("/", served.notFound)
	return secure(mux), nil
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
	return view{
		Lang: lang, Other: other, OtherPath: "/" + string(other) + rest,
		OtherName: p.texts.plain(other, "language.name"), Stylesheet: p.stylesheetPath,
	}
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
	p.settings.Logger.ErrorContext(r.Context(), "a page of the panel failed", "path", r.URL.Path, "error", err)
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
	lang := spanish
	for _, known := range languages {
		if r.URL.Path == "/"+string(known) || strings.HasPrefix(r.URL.Path, "/"+string(known)+"/") {
			lang = known
		}
	}
	p.missing(w, r, lang)
}

func (p *panel) serveStylesheet(w http.ResponseWriter, _ *http.Request) {
	header := w.Header()
	header.Set("Content-Type", "text/css; charset=utf-8")
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(stylesheet)
}

// principalKey is the key under which who a request acts as travels in its
// context.
type principalKey struct{}

// principalOf returns who a request acts as, and whether it acts as
// anybody. The session of a visitor is what will say it (ADR 0012). Until
// it exists nothing in the process can, so no request from outside acts as
// anybody, and the process routes no page.
func principalOf(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(auth.Principal)
	return principal, ok
}
