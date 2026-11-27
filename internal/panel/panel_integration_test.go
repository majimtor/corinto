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
	"regexp"
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
	keys     *auth.Keys
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
		keys: auth.NewKeys(pool, rand.Reader),
		log:  &lockedLog{}, clock: &clock{at: today.Add(16 * time.Hour)},
	}
	built.served, err = newPanel(t.Context(), Settings{
		Logger: slog.New(slog.NewJSONHandler(built.log, nil)),
		Links:  built.store, Inbox: ingest.NewStore(pool, quiet), Aggregates: rollup.NewReader(pool),
		Sessions: built.sessions, Keys: built.keys, Cookie: form, Now: built.clock.now,
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

// on says what the rules made of whoever asked: a kind of device and a
// family of browsers.
func on(device classify.Device, browser classify.Browser) func(*ingest.Event) {
	return func(e *ingest.Event) { e.Classification.Device, e.Classification.Browser = device, browser }
}

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
	// What html/template writes where a template gave it something that
	// cannot go there, such as a list where an address goes.
	if strings.Contains(answer.Body.String(), "ZgotmplZ") {
		t.Errorf("%s %s: a template wrote something where it cannot go", method, path)
	}
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

// The sentence of a page that says how far the events are processed: it
// has the minute it was asked in, by the clock of the database.
var throughSentence = regexp.MustCompile(`<p>[^<]*UTC\.</p>`)

// settled returns a page without the sentences that say a moment, so that
// two answers a minute apart can be compared.
func settled(page string) string {
	return throughSentence.ReplaceAllString(page, "")
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
		most, err := s.texts.plural(c.lang, "count.clicks", 1200)
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
			"what that site adds up to":     "<p>" + s.text(t, c.lang, "breakdown.summary", "name", "news.example.org", "clicks", most, "total", 1201) + "</p>",
			"the share of that site":        share,
			"the clicks without a referrer": `<th scope="row" class="address">` + s.text(t, c.lang, "referrers.none") + "</th>",
			"the kind of device":            `<th scope="row">` + s.text(t, c.lang, "device.desktop") + `</th><td class="count">` + c.clicks + "</td>",
			"the browser":                   `<th scope="row">` + s.text(t, c.lang, "browser.chrome") + `</th><td class="count">` + c.clicks + "</td>",
			"what opens the table":          "<details>\n<summary>" + s.text(t, c.lang, "series.table") + "</summary>",
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
			"that it had no click":       s.text(t, lang, "breakdown.empty", "days", 30),
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the page in %q does not say %s: no %q in it", lang, what, want)
			}
		}
		// Each of the three breakdowns says so, and none has a table.
		if said := strings.Count(page, s.text(t, lang, "breakdown.empty", "days", 30)); said != 3 || strings.Count(page, "<table>") != 1 {
			t.Errorf("the page in %q says %d times that it had no click and has %d tables; want three times, and the table of the series alone",
				lang, said, strings.Count(page, "<table>"))
		}
		// It is not a demo workspace, and no event was ever classified.
		if strings.Contains(page, s.text(t, lang, "link.synthetic")) || strings.Contains(page, `class="mark"`) {
			t.Errorf("the page in %q says that the traffic is synthetic, or marks a change of rules", lang)
		}
	}
}

// between returns what a page holds from one mark to the next one.
func between(t *testing.T, page, from, to string) string {
	t.Helper()
	start := strings.Index(page, from)
	if start < 0 {
		t.Fatalf("the page has no %q", from)
	}
	end := strings.Index(page[start:], to)
	if end < 0 {
		t.Fatalf("the page has no %q after %q", to, from)
	}
	return page[start : start+end]
}

// The page of a link covers the period its address names: seven, thirty or
// ninety days of UTC that end today, with the figures, the series and the
// breakdowns of those days and of no other.
func TestThePageOfALinkCoversThePeriodItsAddressNames(t *testing.T) {
	s := newStage(t)
	workspace := s.workspace(t, false)
	target := s.link(t, workspace, link.Draft{Title: "Spring reading list"})
	// The first and the last day of each period have clicks, and so has
	// the day before each. Each day has a number of them that no sum of
	// the others gives, 1, 2, 4 and so on to 64, so a total says which
	// days are in it. The devices and the browsers of a day are not one
	// for one: in no period does the device with the most have as many
	// clicks as the browser with the most.
	var events []ingest.Event
	for _, some := range []struct {
		ago, clicks int
		device      classify.Device
		browser     classify.Browser
	}{
		{ago: 0, clicks: 1, device: classify.DeviceMobile, browser: classify.BrowserSafari},
		{ago: 6, clicks: 1, device: classify.DeviceDesktop, browser: classify.BrowserChrome},
		{ago: 6, clicks: 1, device: classify.DeviceDesktop, browser: classify.BrowserFirefox},
		{ago: 7, clicks: 4, device: classify.DeviceDesktop, browser: classify.BrowserFirefox},
		{ago: 29, clicks: 5, device: classify.DeviceTablet, browser: classify.BrowserOther},
		{ago: 29, clicks: 3, device: classify.DeviceTablet, browser: classify.BrowserEdge},
		{ago: 30, clicks: 16, device: classify.DeviceDesktop, browser: classify.BrowserEdge},
		{ago: 89, clicks: 20, device: classify.DeviceOther, browser: classify.BrowserOpera},
		{ago: 89, clicks: 12, device: classify.DeviceOther, browser: classify.BrowserChrome},
		{ago: 90, clicks: 64, device: classify.DeviceMobile, browser: classify.BrowserSamsungInternet},
	} {
		events = append(events, times(some.clicks, func() ingest.Event { return visit(target, daysAgo(some.ago), on(some.device, some.browser)) })...)
	}
	s.count(t, events...)

	// A row of a breakdown: the key of its name, its clicks and how long
	// its bar is, as a part of the longest.
	type row struct {
		key    string
		clicks int64
		bar    string
	}
	for _, c := range []struct {
		days              int
		query             string
		total             int64
		devices, browsers []row
	}{
		// Three browsers with a click each: they go by their names, and the
		// first is the one the page names.
		{
			days: 7, query: "?days=7", total: 3,
			devices:  []row{{"device.desktop", 2, "100"}, {"device.mobile", 1, "50"}},
			browsers: []row{{"browser.chrome", 1, "100"}, {"browser.firefox", 1, "100"}, {"browser.safari", 1, "100"}},
		},
		// The row of what the rules do not tell apart is the last one, with
		// as many clicks as the first: the first is the one the page names.
		{
			days: 30, query: "", total: 15,
			devices: []row{{"device.tablet", 8, "100"}, {"device.desktop", 6, "75"}, {"device.mobile", 1, "12.5"}},
			browsers: []row{
				{"browser.firefox", 5, "100"}, {"browser.edge", 3, "60"}, {"browser.chrome", 1, "20"},
				{"browser.safari", 1, "20"}, {"browser.other", 5, "100"},
			},
		},
		// And it is the last one when it has the most, which the page says.
		{
			days: 90, query: "?days=90", total: 63,
			devices: []row{{"device.desktop", 22, "68.7"}, {"device.tablet", 8, "25"}, {"device.mobile", 1, "3.1"}, {"device.other", 32, "100"}},
			browsers: []row{
				{"browser.opera", 20, "100"}, {"browser.edge", 19, "95"}, {"browser.chrome", 13, "65"},
				{"browser.firefox", 5, "25"}, {"browser.safari", 1, "5"}, {"browser.other", 5, "25"},
			},
		},
	} {
		for _, lang := range languages {
			other := spanish
			if lang == spanish {
				other = english
			}
			write := func(text string, err error) string {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				return escaped(text)
			}
			here := "/" + string(lang) + "/links/" + target.ID
			answer := s.get(t, workspace, here+c.query)
			page := answer.Body.String()
			if answer.Code != http.StatusOK {
				t.Fatalf("the page of %d days in %q = %d: %s", c.days, lang, answer.Code, page)
			}
			first, last := write(s.texts.date(lang, daysAgo(c.days-1))), write(s.texts.date(lang, daysAgo(0)))
			for what, want := range map[string]string{
				"its title":          "<title>" + s.text(t, lang, "link.page_title", "name", "Spring reading list", "days", c.days) + " · Corinto</title>",
				"the period":         `<h2 id="period">` + s.text(t, lang, "link.period", "days", c.days, "first", first, "last", last) + "</h2>",
				"the summary":        s.text(t, lang, "series.summary", "days", c.days, "requests", write(s.texts.plural(lang, "count.requests", c.total)), "clicks", write(s.texts.plural(lang, "count.clicks", c.total)), "date", write(s.texts.date(lang, daysAgo(c.days-1)))),
				"the other language": `<a href="/` + string(other) + "/links/" + target.ID + c.query + `" hreflang="` + string(other) + `"`,
			} {
				if !strings.Contains(page, want) {
					t.Errorf("the page of %d days in %q does not show %s: no %q in it", c.days, lang, what, want)
				}
			}
			// The requests and the estimated clicks are the clicks of the
			// days of the period, and the series has a row for each day.
			if figures := strings.Count(page, `<dd class="value">`+s.texts.number(lang, c.total)+"</dd>"); figures != 2 {
				t.Errorf("the page of %d days in %q shows the total %d in %d of its figures, want in two", c.days, lang, c.total, figures)
			}
			if rows := strings.Count(page, "<time datetime="); rows != c.days {
				t.Errorf("the series of the page of %d days in %q has %d days", c.days, lang, rows)
			}
			// The three periods are offered, each at its address, and the
			// one of the page is marked as the one that is shown.
			var choices []string
			for _, offered := range []struct {
				days  int64
				query string
			}{{days: 7, query: "?days=7"}, {days: 30, query: ""}, {days: 90, query: "?days=90"}} {
				current := ""
				if int(offered.days) == c.days {
					current = ` aria-current="page"`
				}
				choices = append(choices, `<li><a href="`+here+offered.query+`"`+current+">"+write(s.texts.plural(lang, "period.days", offered.days))+"</a></li>")
			}
			if want := `<nav class="periods" aria-label="` + s.text(t, lang, "period.label") + "\">\n<ul>\n" + strings.Join(choices, "\n") + "\n</ul>\n</nav>"; !strings.Contains(page, want) || strings.Count(page, "aria-current=") != 1 {
				t.Errorf("the page of %d days in %q does not offer its three periods with its own marked: no %q in it, or more than one mark", c.days, lang, want)
			}
			// Each breakdown has the rows of the period and no other, in
			// their order, and names the first of the rows with the most.
			for _, breakdown := range []struct {
				what, from, to string
				rows           []row
			}{
				{what: "devices", from: `<h2 id="devices-title">`, to: `<h2 id="browsers-title">`, rows: c.devices},
				{what: "browsers", from: `<h2 id="browsers-title">`, to: "</main>", rows: c.browsers},
			} {
				section := between(t, page, breakdown.from, breakdown.to)
				var rows []string
				top := breakdown.rows[0]
				for _, row := range breakdown.rows {
					if row.clicks > top.clicks {
						top = row
					}
					rows = append(rows, `<tr><th scope="row">`+s.text(t, lang, row.key)+`</th><td class="count">`+s.texts.number(lang, row.clicks)+
						`</td><td class="share"><svg class="bar" width="120" height="10" aria-hidden="true" focusable="false"><rect class="bar-fill" width="`+
						row.bar+`%" height="100%" rx="2"/></svg> `+write(s.texts.percent(lang, row.clicks, c.total))+"</td></tr>")
				}
				if want := "<tbody>\n" + strings.Join(rows, "\n") + "\n</tbody>"; !strings.Contains(section, want) {
					t.Errorf("the %s of %d days in %q are not these rows in this order: no %q in %q", breakdown.what, c.days, lang, want, section)
				}
				summary := s.text(t, lang, "breakdown.summary", "name", s.texts.plain(lang, top.key),
					"clicks", write(s.texts.plural(lang, "count.clicks", top.clicks)), "total", c.total)
				if !strings.Contains(section, "<p>"+summary+"</p>") {
					t.Errorf("the %s of %d days in %q do not say which has the most: no %q in it", breakdown.what, c.days, lang, summary)
				}
			}
		}
	}

	// The default period has one more address, the one that names it.
	plain, named := s.get(t, workspace, "/en/links/"+target.ID), s.get(t, workspace, "/en/links/"+target.ID+"?days=30")
	if named.Code != http.StatusOK || settled(named.Body.String()) != settled(plain.Body.String()) || settled(plain.Body.String()) == plain.Body.String() {
		t.Errorf("the page asked for thirty days = %d, and it is not the page asked for no period", named.Code)
	}
	// Anything else is a page the panel does not have.
	for _, query := range []string{"?days=14", "?days=07", "?days=", "?days", "?days=7&days=7", "?days=90&days=7", "?days=7%20", "?days=all"} {
		answer := s.get(t, workspace, "/en/links/"+target.ID+query)
		if page := answer.Body.String(); answer.Code != http.StatusNotFound || !strings.Contains(page, "<h1>"+s.text(t, english, "notfound.title")+"</h1>") || strings.Contains(page, "Spring") {
			t.Errorf("GET the page of the link with %s = %d, want the page of what is not found and nothing of the link", query, answer.Code)
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
