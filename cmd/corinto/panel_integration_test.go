//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// The api process serves the management API and the panel on one port, and
// the two share nothing else. The session cookie is read only by the pages
// of the panel (the engineering rules): an operation of the API called
// with a valid cookie and no key answers 401. And a key opens no page.
func TestTheAPIProcessServesThePanelAndNeitherReadsWhatTheOtherIsGiven(t *testing.T) {
	env, _ := migrated(t)
	seeded, _ := seedOnce(t, env)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, env, io.Discard, stderr) }()
	base := "http://" + announced(t, stderr, "api listening")

	// A client that follows nothing, so that each answer is the one of the
	// request the test made.
	plain := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// answer is what a test looks at of a response, read whole and closed.
	type answer struct {
		status  int
		header  http.Header
		cookies []*http.Cookie
		body    string
	}
	ask := func(client *http.Client, method, path string, form url.Values, headers map[string]string) answer {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequestWithContext(t.Context(), method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		read, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return answer{status: response.StatusCode, header: response.Header, cookies: response.Cookies(), body: string(read)}
	}
	page := func(what string, got answer, status int) {
		t.Helper()
		if got.status != status || got.header.Get("Content-Type") != "text/html; charset=utf-8" ||
			got.header.Get("Content-Security-Policy") == "" || !strings.Contains(got.body, "<!doctype html>") {
			t.Errorf("%s = %d %s, want %d and a page of the panel", what, got.status, got.header.Get("Content-Type"), status)
		}
	}

	// The door of the panel, with the two workspaces of the seed, and the
	// address with no language, which leads to it.
	door := ask(plain, http.MethodGet, "/en/", nil, nil)
	page("the door", door, http.StatusOK)
	if !strings.Contains(door.body, seeded[0].ID) || !strings.Contains(door.body, seeded[1].ID) || !strings.Contains(door.body, "Lighthouse Books") {
		t.Error("the door does not offer the two demo workspaces of the seed")
	}
	if root := ask(plain, http.MethodGet, "/", nil, map[string]string{"Accept-Language": "en"}); root.status != http.StatusSeeOther || root.header.Get("Location") != "/en/" {
		t.Errorf("GET / = %d to %q, want %d to /en/", root.status, root.header.Get("Location"), http.StatusSeeOther)
	}

	// Entering, as a browser on a page of the panel does. Nothing set the
	// form of the cookie, so it is the one for HTTPS.
	entered := ask(plain, http.MethodPost, "/en/session", url.Values{"workspace": {seeded[0].ID}}, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if entered.status != http.StatusSeeOther || entered.header.Get("Location") != "/en/links" || len(entered.cookies) != 1 {
		t.Fatalf("entering = %d to %q with %d cookies, want %d to /en/links with one", entered.status, entered.header.Get("Location"), len(entered.cookies), http.StatusSeeOther)
	}
	cookie := entered.cookies[0]
	if cookie.Name != "__Host-session" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("the cookie is %s, Secure %t, HttpOnly %t; want __Host-session with both and SameSite=Lax", cookie.Name, cookie.Secure, cookie.HttpOnly)
	}
	session := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
	links := ask(plain, http.MethodGet, "/en/links", nil, session)
	page("the links with the session", links, http.StatusOK)
	if !strings.Contains(links.body, "Lighthouse Books") || strings.Count(links.body, `<th scope="row">`) != len(seeded[0].Links) {
		t.Errorf("the list does not show the %d links of the workspace entered", len(seeded[0].Links))
	}

	// The page of the first link shows the history the seed wrote for it:
	// thirty days of the series, and requests in the thousands.
	address := regexp.MustCompile(`<a href="(/en/links/[0-9a-f-]+)">`).FindStringSubmatch(links.body)
	if address == nil {
		t.Fatal("the list names no link")
	}
	shown := ask(plain, http.MethodGet, address[1], nil, session)
	page("the page of the first link", shown, http.StatusOK)
	if strings.Count(shown.body, "<time datetime=") != 30 || !regexp.MustCompile(`<dd class="value">\d,\d{3}</dd>`).MatchString(shown.body) {
		t.Error("the page of the first link does not show thirty days with thousands of requests")
	}

	// The API, whose answers the client checks against the contract: the
	// cookie is not a credential there, and the key is.
	contract := contracttest.Client(t)
	if got := ask(contract, http.MethodGet, "/v1/links", nil, session); got.status != http.StatusUnauthorized {
		t.Errorf("GET /v1/links with a valid session cookie and no key = %d, want %d", got.status, http.StatusUnauthorized)
	}
	key := map[string]string{"Authorization": "Bearer " + seeded[0].Key}
	if got := ask(contract, http.MethodGet, "/v1/links", nil, key); got.status != http.StatusOK {
		t.Errorf("GET /v1/links with the key = %d, want %d", got.status, http.StatusOK)
	}
	// And the key is not a session: the page sends it to the door.
	if got := ask(plain, http.MethodGet, "/en/links", nil, key); got.status != http.StatusSeeOther || got.header.Get("Location") != "/en/" {
		t.Errorf("GET /en/links with a key and no session = %d to %q, want %d to the door", got.status, got.header.Get("Location"), http.StatusSeeOther)
	}

	// Everything under /v1 is of the API, which answers what matches no
	// operation as the contract says. Everything else is of the panel.
	for _, path := range []string{"/v1", "/v1/", "/v1/nothing", "/v1/en/links", "/v1//links"} {
		if got := ask(contract, http.MethodGet, path, nil, session); got.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d as the API answers it", path, got.status, http.StatusNotFound)
		}
	}
	for _, path := range []string{"/nothing", "/readyz", "/v1x", "/v10/links", "/en/v1/links", "/V1/links"} {
		page("GET "+path, ask(plain, http.MethodGet, path, nil, key), http.StatusNotFound)
	}

	// Leaving ends the session for the panel.
	if left := ask(plain, http.MethodPost, "/en/logout", url.Values{}, session); left.status != http.StatusSeeOther || left.header.Get("Clear-Site-Data") == "" {
		t.Errorf("leaving = %d, Clear-Site-Data %q", left.status, left.header.Get("Clear-Site-Data"))
	}
	if got := ask(plain, http.MethodGet, "/en/links", nil, session); got.status != http.StatusSeeOther {
		t.Errorf("the links after leaving = %d, want %d", got.status, http.StatusSeeOther)
	}

	// The log of the process never carries the token nor the key.
	_, secret, _ := strings.Cut(strings.TrimPrefix(seeded[0].Key, "cor_"), "_")
	if log := stderr.String(); strings.Contains(log, cookie.Value) || strings.Contains(log, secret) || !strings.Contains(log, `"route":"GET /en/links"`) {
		t.Error("the log of the api process carries the token of a session or the secret of a key, or no line of the panel")
	}

	cancel()
	select {
	case code := <-exited:
		if code != exitOK {
			t.Errorf("exit code = %d after a clean stop, want %d; log: %s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not stop")
	}
}

// The form of the cookie comes from the environment: the local stack asks
// for the one without Secure, and the process issues it on its loopback
// address.
func TestTheProcessOfTheLocalRunIssuesTheCookieWithoutSecure(t *testing.T) {
	databaseURL := postgrestest.NewDatabase(t)
	env := environmentOf(map[string]string{
		"CORINTO_DATABASE_URL":    databaseURL,
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": redirectDomain,
		"CORINTO_SESSION_COOKIE":  "loopback",
	})
	var log bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, &log); code != exitOK {
		t.Fatalf("migrate exit code = %d; log: %s", code, log.String())
	}
	// A demo workspace, without the history that the seed writes for its
	// own: the operator creates the workspace and the test marks it.
	entered, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, entered); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, env, io.Discard, stderr) }()
	base := "http://" + announced(t, stderr, "api listening")

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/es/session",
		strings.NewReader(url.Values{"workspace": {entered}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	cookies := response.Cookies()
	if response.StatusCode != http.StatusSeeOther || len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Secure ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Errorf("entering the local run = %d with the cookies %v; want %d and session without Secure, with HttpOnly and SameSite=Lax",
			response.StatusCode, cookies, http.StatusSeeOther)
	}

	cancel()
	select {
	case code := <-exited:
		if code != exitOK {
			t.Errorf("exit code = %d after a clean stop, want %d; log: %s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not stop")
	}
}
