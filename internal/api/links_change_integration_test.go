//go:build integration

package api

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/majimtor/corinto/contract"
	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
)

// patch is the media type a change to a link is sent as.
const patch = "application/merge-patch+json"

// change sends a merge patch to a link.
func (s *stage) change(t *testing.T, credential, id string, body any) (received, []byte) {
	t.Helper()
	return s.do(t, credential, http.MethodPatch, "/v1/links/"+id, body, "Content-Type", patch)
}

// newLink creates a link through the API and returns it as it was shown.
func (s *stage) newLink(t *testing.T, credential string, draft any) shownLink {
	t.Helper()
	response, body := s.do(t, credential, http.MethodPost, "/v1/links", draft)
	if response.status != http.StatusCreated {
		t.Fatalf("POST /v1/links = %d %s", response.status, body)
	}
	return read[shownLink](t, body)
}

func TestUpdateLinkChangesWhatIsNamedAndNothingElse(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{}), auth.ScopeLinksRead, auth.ScopeLinksWrite)
	expiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	created := s.newLink(t, key, map[string]any{
		"destination": "https://example.com/a",
		"campaign":    map[string]string{"source": "newsletter", "medium": "email"},
		"title":       "First", "tags": []string{"q4"}, "expires_at": expiry.Format(time.RFC3339),
	})
	text := func(value *string) string {
		if value == nil {
			return "<null>"
		}
		return *value
	}

	// One field: the rest stays, and the link says when it changed.
	response, body := s.change(t, key, created.ID, `{"title":"Second"}`)
	renamed := read[shownLink](t, body)
	if response.status != http.StatusOK || text(renamed.Title) != "Second" || !renamed.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("PATCH of the title = %d %s", response.status, body)
	}
	renamed.Title, renamed.UpdatedAt = created.Title, created.UpdatedAt
	if got, want := string(mustJSON(t, renamed)), string(mustJSON(t, created)); got != want {
		t.Errorf("something else changed:\n%s\nwant\n%s", got, want)
	}

	// The parameters of the campaign change one by one: the one left out
	// stays, the null one goes, the new one is set.
	_, body = s.change(t, key, created.ID, `{"campaign":{"medium":null,"term":"spring"}}`)
	retagged := read[shownLink](t, body)
	if text(retagged.Campaign.Source) != "newsletter" || retagged.Campaign.Medium != nil || text(retagged.Campaign.Term) != "spring" ||
		retagged.Resolved != "https://example.com/a?utm_source=newsletter&utm_term=spring" {
		t.Errorf("after the change of two parameters: %s", body)
	}
	_, body = s.change(t, key, created.ID, `{"campaign":null}`)
	if bare := read[shownLink](t, body); bare.Campaign != (shownCampaign{}) || bare.Resolved != "https://example.com/a" {
		t.Errorf("after a null campaign: %s", body)
	}

	// A null removes what the link had.
	_, body = s.change(t, key, created.ID, `{"title":null,"tags":null,"expires_at":null}`)
	if plain := read[shownLink](t, body); plain.Title != nil || len(plain.Tags) != 0 || plain.ExpiresAt != nil ||
		!strings.Contains(string(body), `"tags":[]`) {
		t.Errorf("after three nulls: %s", body)
	}

	// Several fields at once, and a destination is normalised as in a new
	// link.
	later := expiry.Add(24 * time.Hour)
	_, body = s.change(t, key, created.ID, map[string]any{
		"destination": "HTTPS://Example.com/b", "tags": []string{"q1", "push"}, "expires_at": later.Format(time.RFC3339),
		"campaign": map[string]any{"name": "launch"},
	})
	moved := read[shownLink](t, body)
	if moved.Destination != "https://example.com/b" || moved.Resolved != "https://example.com/b?utm_campaign=launch" ||
		!slices.Equal(moved.Tags, []string{"q1", "push"}) || moved.ExpiresAt == nil || !moved.ExpiresAt.Equal(later) ||
		moved.Identifier != created.Identifier || moved.ID != created.ID {
		t.Errorf("after several fields at once: %s", body)
	}

	// A request that leaves the link as it was is not a change, and not an
	// error.
	for _, same := range []string{`{}`, `{"tags":["q1","push"]}`, `{"campaign":{"name":"launch","term":null}}`} {
		response, body := s.change(t, key, created.ID, same)
		if unchanged := read[shownLink](t, body); response.status != http.StatusOK || !unchanged.UpdatedAt.Equal(moved.UpdatedAt) {
			t.Errorf("PATCH %s = %d and the link changed at %v, want 200 and %v", same, response.status, unchanged.UpdatedAt, moved.UpdatedAt)
		}
	}
	if _, body := s.do(t, key, http.MethodGet, "/v1/links/"+created.ID, nil); string(mustJSON(t, read[shownLink](t, body))) != string(mustJSON(t, moved)) {
		t.Errorf("what is read is not the last change: %s", body)
	}
}

func TestUpdateLinkTellsEveryValueItRejects(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{MayChooseAliases: true}), auth.ScopeLinksRead, auth.ScopeLinksWrite)
	created := s.newLink(t, key, `{"destination":"https://example.com/?utm_term=x","title":"First"}`)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	body := func(field string, reason Reason) []rejection {
		return []rejection{{In: inBody, Field: field, Code: reason}}
	}

	cases := []struct {
		name string
		sent any
		want []rejection
	}{
		{"the identifier of the link", `{"identifier":"spring-sale"}`, body("/identifier", ReasonUnknown)},
		{"an alias", `{"alias":"spring-sale"}`, body("/alias", ReasonUnknown)},
		{"the state", `{"state":"disabled"}`, body("/state", ReasonUnknown)},
		{"no destination at all", `{"destination":null}`, body("/destination", ReasonInvalid)},
		{"another scheme", `{"destination":"ftp://example.com/"}`, body("/destination", ReasonScheme)},
		{"a campaign that is text", `{"campaign":"newsletter"}`, body("/campaign", ReasonInvalid)},
		{"a parameter the campaign does not have", `{"campaign":{"channel":"tv"}}`, body("/campaign/channel", ReasonUnknown)},
		{"a parameter the destination already has", `{"campaign":{"term":"spring"}}`, body("/campaign", ReasonConflict)},
		{"an empty title", `{"title":""}`, body("/title", ReasonTooShort)},
		{"a tag twice", `{"tags":["q4","q4"]}`, body("/tags", ReasonDuplicate)},
		{"an expiry that has passed", `{"expires_at":"` + past + `"}`, body("/expires_at", ReasonInThePast)},
		{"an expiry that is not a time", `{"expires_at":"never"}`, body("/expires_at", ReasonInvalid)},
		{"several values at once", `{"title":"","destination":7,"tags":"q4"}`,
			[]rejection{{inBody, "/destination", ReasonInvalid}, {inBody, "/tags", ReasonInvalid}, {inBody, "/title", ReasonTooShort}}},
		{"a body that is not an object", `[]`, body("", ReasonInvalid)},
		{"a body that is not JSON", `{"title":`, body("", ReasonInvalid)},
		{"no body", ``, body("", ReasonRequired)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, answered := s.change(t, key, created.ID, c.sent)
			if response.status != http.StatusBadRequest || code(t, answered) != CodeInvalidRequest {
				t.Fatalf("PATCH = %d %s, want 400 %s", response.status, answered, CodeInvalidRequest)
			}
			if got := rejected(t, answered); !slices.Equal(got, c.want) {
				t.Errorf("the values rejected are %v, want %v", got, c.want)
			}
		})
	}
	// A valid field next to a refused one changes nothing either.
	if response, _ := s.change(t, key, created.ID, `{"title":"Second","destination":"ftp://example.com/"}`); response.status != http.StatusBadRequest {
		t.Errorf("a valid title with a refused destination = %d, want 400", response.status)
	}
	_, shown := s.do(t, key, http.MethodGet, "/v1/links/"+created.ID, nil)
	if string(mustJSON(t, read[shownLink](t, shown))) != string(mustJSON(t, created)) {
		t.Errorf("the refused changes left the link as %s", shown)
	}

	// A change is a merge patch and says so: plain JSON would not say what
	// a field left out means.
	for _, mediaType := range []string{"application/json", "text/plain"} {
		response, answered := s.do(t, key, http.MethodPatch, "/v1/links/"+created.ID, `{"title":"Second"}`, "Content-Type", mediaType)
		if response.status != http.StatusUnsupportedMediaType || code(t, answered) != CodeUnsupportedMedia {
			t.Errorf("a change sent as %s = %d %s, want 415", mediaType, response.status, answered)
		}
	}
	if response, _ := s.do(t, key, http.MethodPatch, "/v1/links/"+created.ID, `{"title":"Second"}`, "Content-Type", patch+"; charset=utf-8"); response.status != http.StatusOK {
		t.Errorf("a merge patch with its character set = %d, want 200", response.status)
	}
}

func TestDisableAndEnableSwitchALinkAndCanBeRepeated(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{}), auth.ScopeLinksRead, auth.ScopeLinksWrite)
	created := s.newLink(t, key, `{"destination":"https://example.com/"}`)
	path := "/v1/links/" + created.ID

	response, body := s.do(t, key, http.MethodPost, path+"/disable", nil)
	disabled := read[shownLink](t, body)
	if response.status != http.StatusOK || disabled.State != link.StateDisabled || !disabled.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("disable = %d %s", response.status, body)
	}
	response, body = s.do(t, key, http.MethodPost, path+"/disable", nil)
	if again := read[shownLink](t, body); response.status != http.StatusOK || again.State != link.StateDisabled || !again.UpdatedAt.Equal(disabled.UpdatedAt) {
		t.Errorf("disable again = %d %s, want 200 and no change", response.status, body)
	}
	response, body = s.do(t, key, http.MethodPost, path+"/enable", nil)
	enabled := read[shownLink](t, body)
	if response.status != http.StatusOK || enabled.State != link.StateActive || !enabled.UpdatedAt.After(disabled.UpdatedAt) {
		t.Errorf("enable = %d %s", response.status, body)
	}
	response, body = s.do(t, key, http.MethodPost, path+"/enable", nil)
	if again := read[shownLink](t, body); response.status != http.StatusOK || !again.UpdatedAt.Equal(enabled.UpdatedAt) {
		t.Errorf("enable again = %d %s, want 200 and no change", response.status, body)
	}

	// Neither takes a body: whoever sends one expects something else.
	response, body = s.do(t, key, http.MethodPost, path+"/disable", `{"reason":"campaign over"}`)
	if response.status != http.StatusBadRequest || !slices.Equal(rejected(t, body), []rejection{{inBody, "", ReasonInvalid}}) {
		t.Errorf("disable with a body = %d %s, want 400", response.status, body)
	}
	if _, body := s.do(t, key, http.MethodGet, path, nil); read[shownLink](t, body).State != link.StateActive {
		t.Errorf("the refused request disabled the link: %s", body)
	}
}

// The workspace of a link the operator blocked can read it and delete it,
// and nothing else.
func TestABlockedLinkIsReadAndDeletedAndNothingElse(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{}), auth.ScopeLinksRead, auth.ScopeLinksWrite)
	created := s.newLink(t, key, `{"destination":"https://example.com/"}`)
	if _, err := s.store.Block(t.Context(), created.Identifier); err != nil {
		t.Fatal(err)
	}
	path := "/v1/links/" + created.ID

	refusals := []struct {
		name   string
		method string
		path   string
		sent   any
		header []string
	}{
		{"a change", http.MethodPatch, path, `{"title":"Second"}`, []string{"Content-Type", patch}},
		{"a change that changes nothing", http.MethodPatch, path, `{}`, []string{"Content-Type", patch}},
		{"disabling it", http.MethodPost, path + "/disable", nil, nil},
		{"enabling it", http.MethodPost, path + "/enable", nil, nil},
	}
	for _, c := range refusals {
		response, body := s.do(t, key, c.method, c.path, c.sent, c.header...)
		if response.status != http.StatusConflict || code(t, body) != CodeLinkBlocked {
			t.Errorf("%s = %d %s, want 409 %s", c.name, response.status, body, CodeLinkBlocked)
		}
	}
	response, body := s.do(t, key, http.MethodGet, path, nil)
	if shown := read[shownLink](t, body); response.status != http.StatusOK || shown.State != link.StateBlocked || shown.Destination != created.Destination {
		t.Errorf("reading it = %d %s, want it blocked and untouched", response.status, body)
	}
	if response, body := s.do(t, key, http.MethodDelete, path, nil); response.status != http.StatusNoContent {
		t.Errorf("deleting it = %d %s, want 204", response.status, body)
	}
}

func TestDeleteLinkErasesItAndKeepsItsIdentifier(t *testing.T) {
	s := newStage(t)
	key := s.key(t, s.workspace(t, link.WorkspaceDraft{MayChooseAliases: true}), auth.ScopeLinksRead, auth.ScopeLinksWrite)
	created := s.newLink(t, key, `{"destination":"https://example.com/","alias":"spring-sale"}`)
	kept := s.newLink(t, key, `{"destination":"https://example.com/"}`)
	path := "/v1/links/" + created.ID

	response, body := s.do(t, key, http.MethodDelete, path, nil)
	if response.status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("DELETE = %d with %d bytes of body, want 204 and none", response.status, len(body))
	}
	for name, call := range map[string]func() (received, []byte){
		"reading it":     func() (received, []byte) { return s.do(t, key, http.MethodGet, path, nil) },
		"deleting again": func() (received, []byte) { return s.do(t, key, http.MethodDelete, path, nil) },
		"changing it":    func() (received, []byte) { return s.change(t, key, created.ID, `{"title":"Second"}`) },
		"enabling it":    func() (received, []byte) { return s.do(t, key, http.MethodPost, path+"/enable", nil) },
	} {
		if response, body := call(); response.status != http.StatusNotFound || code(t, body) != CodeNotFound {
			t.Errorf("%s after the deletion = %d %s, want 404", name, response.status, body)
		}
	}
	_, body = s.do(t, key, http.MethodGet, "/v1/links", nil)
	if strings.Contains(string(body), created.ID) || !strings.Contains(string(body), kept.ID) {
		t.Errorf("the listing after the deletion: %s", body)
	}
	// The identifier of a deleted link is never issued again, not even to
	// the workspace that had it.
	response, body = s.do(t, key, http.MethodPost, "/v1/links", `{"destination":"https://example.com/","alias":"spring-sale"}`)
	if response.status != http.StatusConflict || code(t, body) != CodeAliasTaken {
		t.Errorf("the alias of the deleted link again = %d %s, want 409 %s", response.status, body, CodeAliasTaken)
	}
}

// operationOnALink is one operation of the contract with what a test needs
// to call it.
type operationOnALink struct {
	id, method, path string
	// query is what follows the path, when the operation needs it.
	query  string
	scope  auth.Scope
	sent   any
	header []string
}

// operationsOfTheContract walks the contract and returns every operation
// that asks for a scope. One that takes a body, or that cannot be asked
// without a query, needs an entry here: a new operation fails these tests
// until somebody says what to send it.
func operationsOfTheContract(t *testing.T) []operationOnALink {
	t.Helper()
	bodies := map[string]struct {
		sent      string
		mediaType string
	}{
		"createLink": {`{"destination":"https://example.com/"}`, "application/json"},
		"updateLink": {`{"title":"Changed by the test"}`, patch},
	}
	const aDay = "from=2026-11-19T00:00:00Z&to=2026-11-20T00:00:00Z"
	queries := map[string]string{
		"getLinkMetricsSummary":   aDay,
		"getLinkMetricsSeries":    aDay + "&granularity=hour",
		"getLinkMetricsBreakdown": aDay + "&dimension=device",
	}
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found []operationOnALink
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			asked, err := requirementOf(operation)
			if err != nil {
				t.Fatal(err)
			}
			if asked.scope == "" {
				continue
			}
			call := operationOnALink{id: operation.OperationID, method: method, path: path, scope: asked.scope}
			for _, parameters := range []openapi3.Parameters{item.Parameters, operation.Parameters} {
				for _, parameter := range parameters {
					if parameter.Value.In != openapi3.ParameterInQuery || !parameter.Value.Required || call.query != "" {
						continue
					}
					query, known := queries[operation.OperationID]
					if !known {
						t.Fatalf("the operation %s cannot be asked without a query and this test does not know which", operation.OperationID)
					}
					call.query = "?" + query
				}
			}
			if operation.RequestBody != nil {
				body, known := bodies[operation.OperationID]
				if !known {
					t.Fatalf("the operation %s takes a body and this test does not know what to send it", operation.OperationID)
				}
				call.sent, call.header = body.sent, []string{"Content-Type", body.mediaType}
			}
			found = append(found, call)
		}
	}
	slices.SortFunc(found, func(a, b operationOnALink) int { return strings.Compare(a.id, b.id) })
	return found
}

// Every operation that names a link answers 404 to a key of another
// workspace, exactly as it does for a link nobody has, and leaves the link
// as it was. The operations are read from the contract, so that one added
// later is walked too.
func TestEveryOperationOnALinkIsClosedToAnotherWorkspace(t *testing.T) {
	s := newStage(t)
	owner, stranger := s.workspace(t, link.WorkspaceDraft{}), s.workspace(t, link.WorkspaceDraft{Name: "Globex"})
	intruder := s.key(t, stranger, auth.Scopes()...)
	target, err := s.store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}

	walked := 0
	for _, operation := range operationsOfTheContract(t) {
		if !strings.Contains(operation.path, "{id}") {
			continue
		}
		walked++
		t.Run(operation.id, func(t *testing.T) {
			foreign, body := s.do(t, intruder, operation.method, strings.ReplaceAll(operation.path, "{id}", target.ID)+operation.query, operation.sent, operation.header...)
			if foreign.status != http.StatusNotFound || code(t, body) != CodeNotFound {
				t.Fatalf("%s on a link of another workspace = %d %s, want 404 %s", operation.id, foreign.status, body, CodeNotFound)
			}
			missing, nobody := s.do(t, intruder, operation.method,
				strings.ReplaceAll(operation.path, "{id}", "00000000-0000-7000-8000-000000000000")+operation.query, operation.sent, operation.header...)
			if missing.status != http.StatusNotFound || !bytes.Equal(body, nobody) {
				t.Errorf("a link of another workspace answers %s and one nobody has %s", body, nobody)
			}
		})
	}
	if walked < 8 {
		t.Errorf("only %d operations on a link were walked: the contract has at least eight", walked)
	}
	after, err := s.store.Get(t.Context(), owner, target.ID)
	if err != nil || after.Version != target.Version || after.State != link.StateActive || after.Title != "First" {
		t.Errorf("after the walk the link is %+v, %v; want it as it was", after, err)
	}
}

// Every operation asks for its scope and for no other: a key with every
// scope but that one is refused, and a key with only that one is not.
func TestEveryOperationAsksForItsScopeAndNoOther(t *testing.T) {
	s := newStage(t)
	owner := s.workspace(t, link.WorkspaceDraft{})
	operations := operationsOfTheContract(t)
	if len(operations) < 10 {
		t.Fatalf("only %d operations ask for a scope: the contract has at least ten", len(operations))
	}
	for _, operation := range operations {
		t.Run(operation.id, func(t *testing.T) {
			// A link of its own for each operation: one of them deletes it.
			target, err := s.store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
			if err != nil {
				t.Fatal(err)
			}
			path := strings.ReplaceAll(operation.path, "{id}", target.ID) + operation.query
			others := slices.DeleteFunc(auth.Scopes(), func(scope auth.Scope) bool { return scope == operation.scope })

			without, body := s.do(t, s.key(t, owner, others...), operation.method, path, operation.sent, operation.header...)
			if without.status != http.StatusForbidden || code(t, body) != CodeForbidden {
				t.Errorf("with every scope but %s = %d %s, want 403 %s", operation.scope, without.status, body, CodeForbidden)
			}
			with, body := s.do(t, s.key(t, owner, operation.scope), operation.method, path, operation.sent, operation.header...)
			if with.status < 200 || with.status > 299 {
				t.Errorf("with only %s = %d %s, want it done", operation.scope, with.status, body)
			}
			if nobody, _ := s.do(t, "", operation.method, path, operation.sent, operation.header...); nobody.status != http.StatusUnauthorized {
				t.Errorf("without a key = %d, want 401", nobody.status)
			}
		})
	}
}
