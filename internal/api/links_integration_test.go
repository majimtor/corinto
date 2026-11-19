//go:build integration

package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// The host that serves the short links of these tests.
const redirectDomain = "go.example.dev"

// stage is the API on a database of its own, with the pieces a test needs
// to prepare what it calls.
type stage struct {
	handler http.Handler
	pool    *pgxpool.Pool
	store   *link.Store
	keys    *auth.Keys
}

func newStage(t *testing.T) *stage {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	built := &stage{
		pool:  pool,
		store: link.NewStore(pool, redirectDomain, rand.Reader),
		keys:  auth.NewKeys(pool, rand.Reader),
	}
	built.handler, err = New(t.Context(), Settings{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Keys: built.keys, Links: built.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// workspace creates a workspace and returns its identifier.
func (s *stage) workspace(t *testing.T, draft link.WorkspaceDraft) string {
	t.Helper()
	if draft.Name == "" {
		draft.Name = "Acme"
	}
	if draft.MaxLinks == 0 {
		draft.MaxLinks = 1000
	}
	draft.MaxKeys = 10
	created, err := link.CreateWorkspace(t.Context(), s.pool, draft)
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// key issues a key of a workspace and returns its credential.
func (s *stage) key(t *testing.T, workspaceID string, scopes ...auth.Scope) string {
	t.Helper()
	_, credential, err := s.keys.Issue(t.Context(), workspaceID, auth.Draft{Name: "Deploy pipeline", Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

// do calls the API with a key, through the client that checks every
// response against the contract. A body that is a string is sent as it is;
// anything else is written as JSON.
func (s *stage) do(t *testing.T, credential, method, path string, body any, headers ...string) (received, []byte) {
	t.Helper()
	var sent io.Reader
	switch value := body.(type) {
	case nil:
	case string:
		sent = strings.NewReader(value)
	case io.Reader:
		sent = value
	default:
		written, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sent = bytes.NewReader(written)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, "http://api.example"+path, sent)
	if err != nil {
		t.Fatal(err)
	}
	if sent != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := contracttest.Over(t, from{s.handler, "203.0.113.7:40000"}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return received{response.StatusCode, response.Header}, read
}

// links returns how many links the database holds, tombstones included.
func (s *stage) links(t *testing.T) int {
	t.Helper()
	var stored int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM links`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// read parses a response body.
func read[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("the body is not what was expected: %v: %s", err, body)
	}
	return value
}

func TestCreateLinkStoresWhatItIsGivenAndShowsIt(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{MayChooseAliases: true})
	key := s.key(t, owner, auth.ScopeLinksRead, auth.ScopeLinksWrite)
	expiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	response, body := s.do(t, key, http.MethodPost, "/v1/links", map[string]any{
		"destination": "HTTPS://Example.com/landing?ref=home#top",
		"campaign":    map[string]string{"source": "newsletter", "name": "launch"},
		"title":       "Launch",
		"tags":        []string{"q4", "email"},
		"expires_at":  expiry.In(time.FixedZone("", 3600)).Format(time.RFC3339),
	})
	if response.status != http.StatusCreated {
		t.Fatalf("POST /v1/links = %d %s", response.status, body)
	}
	created := read[shownLink](t, body)
	newsletter, launch, title := "newsletter", "launch", "Launch"
	want := shownLink{
		ID: created.ID, Domain: redirectDomain, Identifier: created.Identifier, State: link.StateActive,
		Destination: "https://example.com/landing?ref=home#top",
		Resolved:    "https://example.com/landing?ref=home&utm_source=newsletter&utm_campaign=launch#top",
		Campaign:    shownCampaign{Source: &newsletter, Name: &launch},
		Title:       &title, Tags: []string{"q4", "email"}, ExpiresAt: &expiry,
		CreatedAt: created.CreatedAt, UpdatedAt: created.CreatedAt,
	}
	if got, expected := string(mustJSON(t, created)), string(mustJSON(t, want)); got != expected {
		t.Errorf("the link shown is\n%s\nwant\n%s", got, expected)
	}
	if !link.WellFormedIdentifier(created.Identifier) || len(created.Identifier) != 8 || created.ID == "" {
		t.Errorf("the link has the identifier %q and the id %q", created.Identifier, created.ID)
	}
	// Times are given in UTC, whatever zone they were sent or stored in.
	if !strings.Contains(string(body), `"expires_at":"`+expiry.Format(time.RFC3339)+`"`) || created.CreatedAt.Location() != time.UTC {
		t.Errorf("the times are not in UTC: %s", body)
	}
	if got := response.header.Get("Location"); got != "/v1/links/"+created.ID {
		t.Errorf("Location = %q, want the path of the link", got)
	}

	// What is read is what was created, byte for byte.
	again, shown := s.do(t, key, http.MethodGet, response.header.Get("Location"), nil)
	if again.status != http.StatusOK || !bytes.Equal(shown, body) {
		t.Errorf("GET of the link = %d\n%s\nwant\n%s", again.status, shown, body)
	}

	// The least a link needs: what it does not set is null, never left out.
	_, body = s.do(t, key, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`)
	for _, part := range []string{
		`"campaign":{"source":null,"medium":null,"name":null,"term":null,"content":null}`,
		`"title":null`, `"tags":[]`, `"expires_at":null`, `"state":"active"`,
	} {
		if !strings.Contains(string(body), part) {
			t.Errorf("a link with only a destination does not show %s: %s", part, body)
		}
	}

	// An alias is the identifier of the link.
	response, body = s.do(t, key, http.MethodPost, "/v1/links", `{"destination":"https://example.com/","alias":"spring-sale"}`)
	if aliased := read[shownLink](t, body); response.status != http.StatusCreated || aliased.Identifier != "spring-sale" {
		t.Errorf("with an alias = %d and the identifier %q", response.status, aliased.Identifier)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	written, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return written
}

// rejected reads the entries of an error.
func rejected(t *testing.T, body []byte) []rejection {
	t.Helper()
	return read[problem](t, body).Errors
}

// Everything that is wrong with what was sent is told at once, each value
// with its place and its reason, and nothing of it is stored.
func TestCreateLinkTellsEveryValueItRejects(t *testing.T) {
	s := newStage(t)
	plain := s.key(t, s.workspace(t, link.WorkspaceDraft{}), auth.ScopeLinksWrite)
	trusted := s.key(t, s.workspace(t, link.WorkspaceDraft{MayChooseAliases: true}), auth.ScopeLinksWrite)
	narrow := s.key(t, s.workspace(t, link.WorkspaceDraft{Allowed: []string{"https://docs.example.com/guides"}}), auth.ScopeLinksWrite)
	bell := "Launch" + string(rune(7))
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	body := func(field string, reason Reason) []rejection {
		return []rejection{{In: inBody, Field: field, Code: reason}}
	}
	with := func(fields map[string]any) map[string]any {
		draft := map[string]any{"destination": "https://example.com/"}
		for name, value := range fields {
			draft[name] = value
		}
		return draft
	}
	cases := []struct {
		name string
		key  string
		sent any
		want []rejection
	}{
		{"no destination", plain, `{}`, body("/destination", ReasonRequired)},
		{"a field the operation does not take", plain, with(map[string]any{"colour": "red"}), body("/colour", ReasonUnknown)},
		{"a field the campaign does not take", plain, with(map[string]any{"campaign": map[string]any{"channel": "tv"}}), body("/campaign/channel", ReasonUnknown)},
		{"a destination that is not text", plain, `{"destination":7}`, body("/destination", ReasonInvalid)},
		{"tags that are not a list", plain, with(map[string]any{"tags": "q4"}), body("/tags", ReasonInvalid)},
		{"a body that is not an object", plain, `[]`, body("", ReasonInvalid)},
		{"a body that is not JSON", plain, `{"destination":`, body("", ReasonInvalid)},
		{"no body", plain, ``, body("", ReasonRequired)},
		{"several values at once", plain,
			with(map[string]any{"title": "", "tags": []string{"a", "a"}, "expires_at": "tomorrow", "campaign": map[string]any{"source": ""}}),
			[]rejection{
				{inBody, "/campaign/source", ReasonTooShort}, {inBody, "/expires_at", ReasonInvalid},
				{inBody, "/tags", ReasonDuplicate}, {inBody, "/title", ReasonTooShort},
			}},
		{"a destination of 2,049 characters", plain, with(map[string]any{"destination": "https://example.com/" + strings.Repeat("a", 2029)}), body("/destination", ReasonTooLong)},
		{"a title of 201 characters", plain, with(map[string]any{"title": strings.Repeat("a", 201)}), body("/title", ReasonTooLong)},
		{"21 tags", plain, with(map[string]any{"tags": strings.Fields(strings.Repeat("tag ", 21))}),
			[]rejection{{inBody, "/tags", ReasonDuplicate}, {inBody, "/tags", ReasonTooMany}}},
		{"a tag of 41 characters", plain, with(map[string]any{"tags": []string{strings.Repeat("a", 41)}}), body("/tags/0", ReasonTooLong)},
		{"a destination with a space", plain, with(map[string]any{"destination": "https://example.com/a b"}), body("/destination", ReasonCharacters)},
		{"another scheme", plain, with(map[string]any{"destination": "ftp://example.com/"}), body("/destination", ReasonScheme)},
		{"credentials in the destination", plain, with(map[string]any{"destination": "https://user@example.com/"}), body("/destination", ReasonCredentials)},
		{"an address as the host", plain, with(map[string]any{"destination": "http://192.0.2.1/"}), body("/destination", ReasonHost)},
		{"a port that is none", plain, with(map[string]any{"destination": "https://example.com:0/"}), body("/destination", ReasonPort)},
		{"a dot segment", plain, with(map[string]any{"destination": "https://example.com/docs/../admin"}), body("/destination", ReasonDotSegment)},
		{"the redirect domain as the destination", plain, with(map[string]any{"destination": "https://" + redirectDomain + "/k7m2xq9d"}), body("/destination", ReasonRedirectDomain)},
		{"a destination outside the allowlist", narrow, with(map[string]any{"destination": "https://docs.example.com/pricing"}), body("/destination", ReasonNotAllowed)},
		{"an alias too short and badly formed", trusted, with(map[string]any{"alias": "A"}),
			[]rejection{{inBody, "/alias", ReasonInvalid}, {inBody, "/alias", ReasonTooShort}}},
		{"a reserved alias", trusted, with(map[string]any{"alias": "api"}), body("/alias", ReasonReserved)},
		{"an alias in a workspace that cannot choose one", plain, with(map[string]any{"alias": "spring-sale"}), body("/alias", ReasonNotPermitted)},
		{"a campaign parameter the destination already has", plain,
			map[string]any{"destination": "https://example.com/?utm_source=x", "campaign": map[string]any{"source": "newsletter"}}, body("/campaign", ReasonConflict)},
		{"a campaign value with a control character", plain, with(map[string]any{"campaign": map[string]any{"name": bell}}), body("/campaign", ReasonCharacters)},
		{"a title with a control character", plain, with(map[string]any{"title": bell}), body("/title", ReasonCharacters)},
		{"a tag with a control character", plain, with(map[string]any{"tags": []string{bell}}), body("/tags", ReasonCharacters)},
		{"an expiry that has passed", plain, with(map[string]any{"expires_at": past}), body("/expires_at", ReasonInThePast)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, answered := s.do(t, c.key, http.MethodPost, "/v1/links", c.sent)
			if response.status != http.StatusBadRequest || code(t, answered) != CodeInvalidRequest {
				t.Fatalf("POST /v1/links = %d %s, want 400 %s", response.status, answered, CodeInvalidRequest)
			}
			if got := rejected(t, answered); !slices.Equal(got, c.want) {
				t.Errorf("the values rejected are %v, want %v", got, c.want)
			}
		})
	}
	if stored := s.links(t); stored != 0 {
		t.Errorf("the refused requests stored %d links", stored)
	}
}

func TestCreateLinkSaysWhatItConflictsWith(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{MayChooseAliases: true, MaxLinks: 2})
	key := s.key(t, owner, auth.ScopeLinksWrite)
	const aliased = `{"destination":"https://example.com/","alias":"spring-sale"}`

	if response, body := s.do(t, key, http.MethodPost, "/v1/links", aliased); response.status != http.StatusCreated {
		t.Fatalf("the first link = %d %s", response.status, body)
	}
	response, body := s.do(t, key, http.MethodPost, "/v1/links", aliased)
	if response.status != http.StatusConflict || code(t, body) != CodeAliasTaken {
		t.Errorf("the same alias again = %d %s, want 409 %s", response.status, body, CodeAliasTaken)
	}
	if response, body := s.do(t, key, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`); response.status != http.StatusCreated {
		t.Fatalf("the second link = %d %s", response.status, body)
	}
	response, body = s.do(t, key, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`)
	if response.status != http.StatusConflict || code(t, body) != CodeQuotaExceeded {
		t.Errorf("a link over the quota = %d %s, want 409 %s", response.status, body, CodeQuotaExceeded)
	}
	if stored := s.links(t); stored != 2 {
		t.Errorf("%d links stored, want 2", stored)
	}
}

func TestTheBodyOfCreateLinkHasAMediaTypeAndASize(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{}), auth.ScopeLinksWrite)
	const draft = `{"destination":"https://example.com/"}`

	for _, mediaType := range []string{"text/plain", "application/x-www-form-urlencoded", "application/jsonx", ""} {
		response, body := s.do(t, key, http.MethodPost, "/v1/links", draft, "Content-Type", mediaType)
		if response.status != http.StatusUnsupportedMediaType || code(t, body) != CodeUnsupportedMedia {
			t.Errorf("a body sent as %q = %d %s, want 415 %s", mediaType, response.status, body, CodeUnsupportedMedia)
		}
	}
	if response, body := s.do(t, key, http.MethodPost, "/v1/links", draft, "Content-Type", "application/json; charset=utf-8"); response.status != http.StatusCreated {
		t.Errorf("JSON with its character set = %d %s, want 201", response.status, body)
	}

	// A body that declares no length is cut off while it is read.
	endless := struct{ io.Reader }{strings.NewReader(`{"destination":"https://example.com/` + strings.Repeat("a", 20_000) + `"}`)}
	response, body := s.do(t, key, http.MethodPost, "/v1/links", endless)
	if response.status != http.StatusRequestEntityTooLarge || code(t, body) != CodePayloadTooLarge {
		t.Errorf("a body of unknown length over the limit = %d %s, want 413 %s", response.status, body, CodePayloadTooLarge)
	}
	if stored := s.links(t); stored != 1 {
		t.Errorf("%d links stored, want only the one sent as JSON", stored)
	}
}

func TestTheOperationsOnLinksAskForTheirScope(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	onlyReads, onlyWrites := s.key(t, owner, auth.ScopeLinksRead), s.key(t, owner, auth.ScopeLinksWrite)
	other := s.key(t, owner, auth.ScopeMetricsRead)
	_, body := s.do(t, onlyWrites, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`)
	path := "/v1/links/" + read[shownLink](t, body).ID

	cases := []struct {
		name   string
		key    string
		method string
		path   string
		sent   any
		want   int
	}{
		{"creating with links:write", onlyWrites, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`, http.StatusCreated},
		{"creating with links:read", onlyReads, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`, http.StatusForbidden},
		{"creating with another scope", other, http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`, http.StatusForbidden},
		{"creating without a key", "", http.MethodPost, "/v1/links", `{"destination":"https://example.com/"}`, http.StatusUnauthorized},
		{"reading with links:read", onlyReads, http.MethodGet, path, nil, http.StatusOK},
		{"reading with links:write", onlyWrites, http.MethodGet, path, nil, http.StatusForbidden},
		{"reading without a key", "", http.MethodGet, path, nil, http.StatusUnauthorized},
		{"listing with links:read", onlyReads, http.MethodGet, "/v1/links", nil, http.StatusOK},
		{"listing with links:write", onlyWrites, http.MethodGet, "/v1/links", nil, http.StatusForbidden},
		{"listing with another scope", other, http.MethodGet, "/v1/links", nil, http.StatusForbidden},
		// The key comes before the content: a request without the scope
		// learns nothing about what the operation takes.
		{"an invalid body without the scope", onlyReads, http.MethodPost, "/v1/links", `{}`, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if response, body := s.do(t, c.key, c.method, c.path, c.sent); response.status != c.want {
				t.Errorf("%s %s = %d %s, want %d", c.method, c.path, response.status, body, c.want)
			}
		})
	}
}

// A link of another workspace, a deleted one and one that never existed
// cannot be told apart.
func TestGetLinkAnswersOnlyForALinkOfTheWorkspace(t *testing.T) {
	s := newStage(t)
	owner, stranger := s.workspace(t, link.WorkspaceDraft{}), s.workspace(t, link.WorkspaceDraft{Name: "Globex"})
	key := s.key(t, owner, auth.ScopeLinksRead)
	create := func(workspaceID string) link.Link {
		t.Helper()
		created, err := s.store.Create(t.Context(), workspaceID, link.Draft{Destination: "https://example.com/"})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	own, foreign, deleted := create(owner), create(stranger), create(owner)
	if err := s.store.Delete(t.Context(), owner, deleted.ID); err != nil {
		t.Fatal(err)
	}

	if response, _ := s.do(t, key, http.MethodGet, "/v1/links/"+own.ID, nil); response.status != http.StatusOK {
		t.Errorf("a link of the workspace = %d, want 200", response.status)
	}
	var first []byte
	for name, id := range map[string]string{
		"a link of another workspace":    foreign.ID,
		"a deleted link":                 deleted.ID,
		"an identifier nobody has":       "00000000-0000-7000-8000-000000000000",
		"something that is not one":      "spring-sale",
		"the short identifier of a link": own.Identifier,
	} {
		response, body := s.do(t, key, http.MethodGet, "/v1/links/"+id, nil)
		if response.status != http.StatusNotFound || code(t, body) != CodeNotFound {
			t.Errorf("%s = %d %s, want 404 %s", name, response.status, body, CodeNotFound)
		}
		if first == nil {
			first = body
		}
		if !bytes.Equal(body, first) {
			t.Errorf("%s answers differently from the others: %s", name, body)
		}
	}
}

func TestListLinksPagesThroughTheLinksOfTheWorkspace(t *testing.T) {
	s := newStage(t)
	owner, stranger := s.workspace(t, link.WorkspaceDraft{}), s.workspace(t, link.WorkspaceDraft{Name: "Globex"})
	key := s.key(t, owner, auth.ScopeLinksRead)
	type page struct {
		Items      []shownLink `json:"items"`
		NextCursor *string     `json:"next_cursor"`
	}
	create := func(workspaceID string) string {
		t.Helper()
		created, err := s.store.Create(t.Context(), workspaceID, link.Draft{Destination: "https://example.com/"})
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	var want []string
	for range 5 {
		want = append(want, create(owner))
		create(stranger)
	}
	foreign := create(stranger)

	// Pages of two: three pages, the last one says so.
	var (
		got   []string
		sizes []int
	)
	path := "/v1/links?limit=2"
	for {
		response, body := s.do(t, key, http.MethodGet, path, nil)
		if response.status != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, response.status, body)
		}
		listed := read[page](t, body)
		for _, item := range listed.Items {
			got = append(got, item.ID)
		}
		sizes = append(sizes, len(listed.Items))
		if listed.NextCursor == nil {
			break
		}
		if strings.Contains(*listed.NextCursor, got[len(got)-1]) {
			t.Errorf("the cursor shows what is inside it: %s", *listed.NextCursor)
		}
		path = "/v1/links?limit=2&cursor=" + *listed.NextCursor
	}
	if !slices.Equal(got, want) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("listed %v in pages of %v\nwant   %v in pages of 2, 2 and 1", got, sizes, want)
	}

	// Without a limit a page has up to fifty, which here is all of them.
	_, body := s.do(t, key, http.MethodGet, "/v1/links", nil)
	if all := read[page](t, body); len(all.Items) != 5 || all.NextCursor != nil {
		t.Errorf("without a limit: %d links and the cursor %v, want 5 and none", len(all.Items), all.NextCursor)
	}
	// A workspace without links has an empty list, not a null.
	empty := s.key(t, s.workspace(t, link.WorkspaceDraft{Name: "Initech"}), auth.ScopeLinksRead)
	if _, body := s.do(t, empty, http.MethodGet, "/v1/links", nil); strings.TrimSpace(string(body)) != `{"items":[],"next_cursor":null}` {
		t.Errorf("a workspace without links lists %s", body)
	}
	// The position of a link of another workspace is only a position.
	_, body = s.do(t, key, http.MethodGet, "/v1/links?cursor="+cursor(foreign), nil)
	if after := read[page](t, body); len(after.Items) != 0 {
		t.Errorf("after the last link of another workspace: %d links, want none", len(after.Items))
	}

	invalid := []struct {
		query string
		want  []rejection
	}{
		{"limit=0", []rejection{{inQuery, "limit", ReasonInvalid}}},
		{"limit=101", []rejection{{inQuery, "limit", ReasonInvalid}}},
		{"limit=many", []rejection{{inQuery, "limit", ReasonInvalid}}},
		{"cursor=", []rejection{{inQuery, "cursor", ReasonTooShort}}},
		{"cursor=!!!", []rejection{{inQuery, "cursor", ReasonInvalid}}},
		{"cursor=" + cursor("spring-sale"), []rejection{{inQuery, "cursor", ReasonInvalid}}},
		{"cursor=" + strings.Repeat("a", 201), []rejection{{inQuery, "cursor", ReasonTooLong}}},
		{"limit=0&cursor=", []rejection{{inQuery, "cursor", ReasonTooShort}, {inQuery, "limit", ReasonInvalid}}},
	}
	for _, c := range invalid {
		response, body := s.do(t, key, http.MethodGet, "/v1/links?"+c.query, nil)
		if response.status != http.StatusBadRequest || !slices.Equal(rejected(t, body), c.want) {
			t.Errorf("GET /v1/links?%s = %d %s, want 400 with %v", c.query, response.status, body, c.want)
		}
	}
}
