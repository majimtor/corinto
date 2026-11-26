//go:build integration

package panel

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
	"github.com/majimtor/corinto/internal/rollup"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// The host that serves the short links of these tests.
const redirectDomain = "go.example.dev"

// today is the day the pages of these tests are asked for: the last of the
// thirty they show.
var today = time.Date(2026, time.November, 19, 0, 0, 0, 0, time.UTC)

// daysAgo returns a moment of a day before today, in its afternoon.
func daysAgo(days int) time.Time {
	return today.AddDate(0, 0, -days).Add(15 * time.Hour)
}

// stage is the panel on a database of its own, with the pieces a test
// needs to prepare what a page shows. Its helpers are the ones of the
// tests of the API, written again: the two packages share no test code.
type stage struct {
	handler  http.Handler
	served   *panel
	pool     *pgxpool.Pool
	store    *link.Store
	sessions *auth.Sessions
	texts    catalogs
	// log is what the panel logged, and clock the time its pages are
	// asked at, which a test moves.
	log   *lockedLog
	clock *clock
}

// lockedLog is a log that requests write while a test reads.
type lockedLog struct {
	mu      sync.Mutex
	written bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.String()
}

// clock is a time that stands still until a test moves it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(by)
}

// newStage returns the panel of an installation a browser reaches over
// HTTPS, which is what most tests ask their pages of.
func newStage(t *testing.T) *stage {
	t.Helper()
	return newStageWith(t, CookieSecure)
}

func newStageWith(t *testing.T, form CookieForm) *stage {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	built := &stage{
		pool: pool, store: link.NewStore(pool, redirectDomain, rand.Reader), sessions: auth.NewSessions(pool, rand.Reader),
		log: &lockedLog{}, clock: &clock{at: today.Add(16 * time.Hour)},
	}
	built.served, err = newPanel(t.Context(), Settings{
		Logger: slog.New(slog.NewJSONHandler(built.log, nil)),
		Links:  built.store, Inbox: ingest.NewStore(pool, quiet), Aggregates: rollup.NewReader(pool),
		Sessions: built.sessions, Cookie: form, Now: built.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	built.handler = built.served.handler()
	if built.texts, err = readCatalogs(); err != nil {
		t.Fatal(err)
	}
	return built
}

// workspace creates a workspace and returns its identifier.
func (s *stage) workspace(t *testing.T, demo bool) string {
	t.Helper()
	created, err := link.CreateWorkspace(t.Context(), s.pool, link.WorkspaceDraft{Name: "Acme", Demo: demo, MaxLinks: 100, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// link creates a link of a workspace.
func (s *stage) link(t *testing.T, workspaceID string, draft link.Draft) link.Link {
	t.Helper()
	if draft.Destination == "" {
		draft.Destination = "https://example.com/books"
	}
	created, err := s.store.Create(t.Context(), workspaceID, draft)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// visit returns the event of an estimated human click on a link: a
// redirect to somebody on a desktop Chrome that named no referrer.
func visit(target link.Link, at time.Time, changes ...func(*ingest.Event)) ingest.Event {
	event := ingest.Event{
		ID: uuid.NewV7().String(), LinkID: target.ID, WorkspaceID: target.WorkspaceID, OccurredAt: at,
		Outcome: ingest.OutcomeRedirected, Method: "GET",
		Classification: classify.Classification{
			Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
			Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
		},
	}
	for _, change := range changes {
		change(&event)
	}
	return event
}

func referred(host string) func(*ingest.Event) {
	return func(e *ingest.Event) { e.ReferrerHost = host }
}

func crawled(e *ingest.Event) { e.Classification.Class = classify.ClassCrawler }

func unserved(e *ingest.Event) { e.Outcome = ingest.OutcomeInactive }

// times returns as many events as asked, each made anew.
func times(n int, event func() ingest.Event) []ingest.Event {
	events := make([]ingest.Event, n)
	for i := range events {
		events[i] = event()
	}
	return events
}

// count writes events to the inbox, as the redirect does, and adds them to
// the aggregates, as the worker does.
func (s *stage) count(t *testing.T, events ...ingest.Event) {
	t.Helper()
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if settled, err := ingest.NewStore(s.pool, quiet).Accept(t.Context(), events); err != nil || settled != len(events) {
		t.Fatalf("Accept settled %d of %d events with the error %v", settled, len(events), err)
	}
	consumer := rollup.NewConsumer(s.pool, quiet)
	for {
		processed, err := consumer.Batch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if processed == 0 {
			return
		}
	}
}

// ask sends the panel a request, changed as a test says, and returns the
// answer.
func (s *stage) ask(t *testing.T, method, path string, changes ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for _, change := range changes {
		change(request)
	}
	answer := httptest.NewRecorder()
	s.handler.ServeHTTP(answer, request)
	return answer
}

// carried returns a cookie as a browser sends it back: a name and a value,
// which is all a request carries of one.
func carried(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value} //nolint:gosec // The attributes of a cookie stay in the browser: a request has none.
}

// with gives a request a cookie.
func with(cookie *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(cookie) }
}

// saying gives a request a header.
func saying(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// to names the host a request is sent to.
func to(host string) func(*http.Request) {
	return func(r *http.Request) { r.Host = host }
}

// from names the address a request comes from.
func from(address string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = address }
}

// filled gives a request the form a page of the panel sends.
func filled(fields url.Values) func(*http.Request) {
	return func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
}

// naming is the form of the demo entry: the workspace to enter.
func naming(workspaceID string) func(*http.Request) {
	return filled(url.Values{"workspace": {workspaceID}})
}

// sessionOf returns the cookie of a new session in a workspace. A session
// opens in a demo workspace alone: for any other, one is opened in a demo
// workspace made for it and its row is moved by hand.
func (s *stage) sessionOf(t *testing.T, workspaceID string) *http.Cookie {
	t.Helper()
	entered, err := s.store.Workspace(t.Context(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	door := workspaceID
	if !entered.Demo {
		door = s.workspace(t, true)
	}
	session, token, err := s.sessions.Open(t.Context(), door)
	if err != nil {
		t.Fatal(err)
	}
	if door != workspaceID {
		if _, err := s.pool.Exec(t.Context(), `UPDATE sessions SET workspace_id = $1::uuid WHERE id = $2::uuid`, workspaceID, session.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `DELETE FROM workspaces WHERE id = $1::uuid`, door); err != nil {
			t.Fatal(err)
		}
	}
	return carried(s.served.cookieName(), token)
}

// get asks the panel for a page with a session in a workspace, or with
// none when the workspace is empty.
func (s *stage) get(t *testing.T, workspaceID, path string) *httptest.ResponseRecorder {
	t.Helper()
	if workspaceID == "" {
		return s.ask(t, http.MethodGet, path)
	}
	return s.ask(t, http.MethodGet, path, with(s.sessionOf(t, workspaceID)))
}

// held returns how many sessions the database holds.
func (s *stage) held(t *testing.T) int {
	t.Helper()
	var rows int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// text returns a text of a catalogue, as a page escapes it.
func (s *stage) text(t *testing.T, lang language, key string, pairs ...any) string {
	t.Helper()
	found, err := s.texts.text(lang, key, pairs...)
	if err != nil {
		t.Fatal(err)
	}
	return escaped(found)
}

// escaped returns a text as html/template writes it inside an element.
func escaped(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(text)
}

// The page of a link, with what a workspace would see of a link that was
// asked for on three of its last thirty days.
func TestThePageOfALinkShowsItsFiguresInBothLanguages(t *testing.T) {
	s := newStage(t)
	workspace := s.workspace(t, true)
	target := s.link(t, workspace, link.Draft{Title: "Spring reading list", Destination: "https://example.com/books/spring"})
	var events []ingest.Event
	// Two days ago: 1,200 estimated human clicks from one site, and 300
	// requests of a crawler.
	events = append(events, times(1200, func() ingest.Event { return visit(target, daysAgo(2), referred("news.example.org")) })...)
	events = append(events, times(300, func() ingest.Event { return visit(target, daysAgo(2), crawled) })...)
	// Today: one click that named no referrer, and one request while the
	// link did not redirect.
	events = append(events, visit(target, daysAgo(0)), visit(target, daysAgo(0), unserved))
	// Thirty days ago, the day before the period: it is in no figure.
	events = append(events, visit(target, daysAgo(30)))
	s.count(t, events...)

	// A day as the language of a page writes it.
	date := func(lang language, at time.Time) string {
		t.Helper()
		written, err := s.texts.date(lang, at)
		if err != nil {
			t.Fatal(err)
		}
		return written
	}
	for _, c := range []struct {
		lang                   language
		requests, clicks, most string
		other                  string
	}{
		{lang: spanish, requests: "1.501", clicks: "1.201", most: "1.500", other: "en"},
		{lang: english, requests: "1,501", clicks: "1,201", most: "1,500", other: "es"},
	} {
		firstDay, lastDay, peak := date(c.lang, daysAgo(29)), date(c.lang, daysAgo(0)), date(c.lang, daysAgo(2))
		share, err := s.texts.percent(c.lang, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		answer := s.get(t, workspace, "/"+string(c.lang)+"/links/"+target.ID)
		page := answer.Body.String()
		if answer.Code != http.StatusOK {
			t.Fatalf("the page in %q = %d: %s", c.lang, answer.Code, page)
		}
		for what, want := range map[string]string{
			"its language":                  `<html lang="` + string(c.lang) + `">`,
			"its title":                     "<title>" + s.text(t, c.lang, "link.page_title", "name", "Spring reading list", "days", 30) + " · Corinto</title>",
			"the name of the link":          "<h1>Spring reading list</h1>",
			"the short link":                redirectDomain + "/" + target.Identifier,
			"the destination":               "https://example.com/books/spring",
			"the state":                     "<dd>" + s.text(t, c.lang, "link.state.active") + "</dd>",
			"that it is synthetic":          s.text(t, c.lang, "link.synthetic"),
			"the period":                    s.text(t, c.lang, "link.period", "days", 30, "first", firstDay, "last", lastDay),
			"the rules":                     s.text(t, c.lang, "link.rules", "version", 1),
			"the requests":                  `<dd class="value">` + c.requests + "</dd>",
			"the estimated clicks":          `<dd class="value">` + c.clicks + "</dd>",
			"the inactive requests":         `<dd class="value">1</dd>`,
			"the limit of the estimate":     s.text(t, c.lang, "metric.estimated_human_clicks.definition"),
			"the question of the series":    `<h2 id="series-title">` + s.text(t, c.lang, "series.title") + "</h2>",
			"the day with the most":         peak + ".",
			"a row of the series":           `<time datetime="2026-11-17">` + peak + `</time></th><td class="count">` + c.most + `</td>`,
			"the site with the most":        `<th scope="row" class="address">news.example.org</th><td class="count">`,
			"the share of that site":        share,
			"the clicks without a referrer": `<th scope="row" class="address">` + s.text(t, c.lang, "referrers.none") + "</th>",
			"the other language":            `<a href="/` + c.other + "/links/" + target.ID + `" hreflang="` + c.other + `" lang="` + c.other + `">`,
			"the stylesheet":                `<link rel="stylesheet" href="/assets/panel-`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the page in %q does not show %s: no %q in it", c.lang, what, want)
			}
		}
		if rows := strings.Count(page, "<time datetime="); rows != 30 {
			t.Errorf("the series of the page in %q has %d days, want 30", c.lang, rows)
		}
		// The day before the period is in no figure, and the series has a
		// line for the requests and another for the estimated clicks.
		if strings.Contains(page, "2026-10-20") || strings.Count(page, "<polyline ") != 2 {
			t.Errorf("the page in %q shows a day outside its period, or not two lines", c.lang)
		}
	}
}

// A link nobody asked for still has its page: the series is flat, and the
// page says in words that there is nothing.
func TestThePageOfALinkWithNoTrafficSaysSo(t *testing.T) {
	s := newStage(t)
	workspace := s.workspace(t, false)
	target := s.link(t, workspace, link.Draft{})
	for _, lang := range languages {
		answer := s.get(t, workspace, "/"+string(lang)+"/links/"+target.ID)
		page := answer.Body.String()
		if answer.Code != http.StatusOK {
			t.Fatalf("the page in %q = %d: %s", lang, answer.Code, page)
		}
		for what, want := range map[string]string{
			"the identifier as its name": "<h1>" + target.Identifier + "</h1>",
			"that it was not requested":  s.text(t, lang, "series.empty", "days", 30),
			"that it had no click":       s.text(t, lang, "referrers.empty", "days", 30),
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the page in %q does not say %s: no %q in it", lang, what, want)
			}
		}
		// It is not a demo workspace, and no event was ever classified.
		if strings.Contains(page, s.text(t, lang, "link.synthetic")) || strings.Contains(page, `class="mark"`) {
			t.Errorf("the page in %q says that the traffic is synthetic, or marks a change of rules", lang)
		}
	}
}

// What a workspace wrote reaches the page as text, whatever it holds.
func TestThePageEscapesWhatAWorkspaceWrote(t *testing.T) {
	s := newStage(t)
	workspace := s.workspace(t, false)
	target := s.link(t, workspace, link.Draft{Title: `<script>alert("x")</script> & co`})
	page := s.get(t, workspace, "/en/links/"+target.ID).Body.String()
	if strings.Contains(page, "<script") || !strings.Contains(page, "<h1>&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; co</h1>") {
		t.Errorf("the title of the link reached the page as markup, or not at all")
	}
}

// One point of authorization (PLAN.md section 9): the link of another
// workspace is answered like one that does not exist.
func TestALinkOfAnotherWorkspaceIsNotFound(t *testing.T) {
	s := newStage(t)
	owner, other := s.workspace(t, false), s.workspace(t, false)
	target := s.link(t, owner, link.Draft{Title: "Spring reading list"})
	path := "/es/links/" + target.ID

	if answer := s.get(t, owner, path); answer.Code != http.StatusOK {
		t.Fatalf("the page of the link for its own workspace = %d", answer.Code)
	}
	// The page of what is not found names its own path, in the link to the
	// other language: with that taken out, two of them are the same page.
	without := func(answer *httptest.ResponseRecorder, path string) string {
		return strings.ReplaceAll(answer.Body.String(), strings.TrimPrefix(path, "/es"), "")
	}
	missing := "/es/links/" + uuid.NewV7().String()
	gone := s.get(t, owner, missing)
	if gone.Code != http.StatusNotFound || !strings.Contains(gone.Body.String(), "<h1>"+s.text(t, spanish, "notfound.title")+"</h1>") {
		t.Fatalf("a link that does not exist = %d, without the page that says so", gone.Code)
	}
	for _, c := range []struct{ who, workspace, path string }{
		{who: "another workspace", workspace: other, path: path},
		{who: "its workspace, by an identifier that is not one", workspace: owner, path: "/es/links/spring"},
	} {
		answer := s.get(t, c.workspace, c.path)
		if got := without(answer, c.path); answer.Code != http.StatusNotFound || got != without(gone, missing) || strings.Contains(got, "Spring") {
			t.Errorf("the page of a link asked for by %s = %d, and it is not the page of a link that does not exist", c.who, answer.Code)
		}
	}
}

// Every response of the panel carries its security headers, the page of a
// failure and the stylesheet included (the engineering rules).
func TestEveryResponseCarriesItsSecurityHeaders(t *testing.T) {
	s := newStage(t)
	workspace := s.workspace(t, false)
	target := s.link(t, workspace, link.Draft{})
	page := s.get(t, workspace, "/en/links/"+target.ID)
	start := strings.Index(page.Body.String(), `href="/assets/`) + len(`href="`)
	stylesheetPath := page.Body.String()[start : start+strings.Index(page.Body.String()[start:], `"`)]

	answers := map[string]*httptest.ResponseRecorder{
		"the page of a link":        page,
		"the list of the links":     s.get(t, workspace, "/en/links"),
		"the door":                  s.get(t, "", "/en/"),
		"a link that is not there":  s.get(t, workspace, "/en/links/"+uuid.NewV7().String()),
		"a path that is not a page": s.get(t, workspace, "/nothing/here"),
		"a path with a dot segment": s.get(t, "", "/en/../es/"),
		"a form from another site":  s.ask(t, http.MethodPost, "/en/logout", saying("Sec-Fetch-Site", "cross-site")),
		"the stylesheet":            s.get(t, "", stylesheetPath),
	}
	// What sends a request elsewhere has no body, and so no type.
	redirects := map[string]*httptest.ResponseRecorder{
		"the address with no language":   s.get(t, "", "/"),
		"a page asked for by nobody":     s.get(t, "", "/en/links"),
		"the door without its slash":     s.get(t, "", "/en"),
		"leaving":                        s.ask(t, http.MethodPost, "/en/logout"),
		"entering":                       s.ask(t, http.MethodPost, "/en/session", naming(s.workspace(t, true))),
		"the door for whoever is inside": s.get(t, workspace, "/en/"),
	}
	for what, answer := range redirects {
		if answer.Code < 300 || answer.Code > 399 || answer.Body.Len() != 0 || answer.Header().Get("Location") == "" ||
			answer.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d with %d bytes, to %q, Cache-Control %q; want a redirect with no body that no cache keeps",
				what, answer.Code, answer.Body.Len(), answer.Header().Get("Location"), answer.Header().Get("Cache-Control"))
		}
		answers[what] = answer
	}
	// With the database gone, a page answers as a failure.
	inside := s.sessionOf(t, workspace)
	s.pool.Close()
	failed := s.ask(t, http.MethodGet, "/es/links/"+target.ID, with(inside))
	if failed.Code != http.StatusInternalServerError || !strings.Contains(failed.Body.String(), "<h1>"+s.text(t, spanish, "failed.title")+"</h1>") {
		t.Errorf("a page without its database = %d, without the page that says so", failed.Code)
	}
	answers["a page that failed"] = failed

	for what, answer := range answers {
		header := answer.Header()
		if got := header.Get("Content-Security-Policy"); got != policy || strings.Contains(got, "unsafe") || !strings.Contains(got, "default-src 'none'") {
			t.Errorf("%s: Content-Security-Policy = %q", what, got)
		}
		if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: X-Content-Type-Options = %q, Referrer-Policy = %q", what, header.Get("X-Content-Type-Options"), header.Get("Referrer-Policy"))
		}
		if header.Get("X-Request-Id") == "" {
			t.Errorf("%s: no X-Request-Id", what)
		}
		kind, cache := "text/html; charset=utf-8", "no-store"
		if what == "the stylesheet" {
			kind, cache = "text/css; charset=utf-8", "public, max-age=31536000, immutable"
		}
		if _, sent := redirects[what]; sent {
			kind = ""
		}
		if header.Get("Content-Type") != kind || header.Get("Cache-Control") != cache {
			t.Errorf("%s: Content-Type = %q, Cache-Control = %q", what, header.Get("Content-Type"), header.Get("Cache-Control"))
		}
		for name := range header {
			if strings.HasPrefix(name, "Access-Control-") {
				t.Errorf("%s carries %s: no response shares itself with another origin", what, name)
			}
		}
	}
	if answers["the stylesheet"].Code != http.StatusOK || answers["the stylesheet"].Body.Len() == 0 {
		t.Errorf("the stylesheet the page names = %d", answers["the stylesheet"].Code)
	}
	// A path of the panel that is not a page is said so in the language
	// its first segment names, and in Spanish when it names none.
	for path, lang := range map[string]language{"/en/nothing": english, "/en/links/": english, "/es/nothing": spanish, "/nothing": spanish, "/english": spanish} {
		answer := s.get(t, "", path)
		if answer.Code != http.StatusNotFound || !strings.Contains(answer.Body.String(), `<html lang="`+string(lang)+`">`) {
			t.Errorf("GET %s = %d, want the page of what is not found in %q", path, answer.Code, lang)
		}
	}
}
