package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// links serves the operations on links. Every one of them acts in the
// workspace of the key of the request, and in no other.
type links struct {
	store *link.Store
	// now is the clock the expiry of a new link is compared with.
	now func() time.Time
}

// shownLink is a link as the contract shows it. What a link does not set is
// null, never left out: a client reads the same fields in every link.
type shownLink struct {
	ID          string        `json:"id"`
	Domain      string        `json:"domain"`
	Identifier  string        `json:"identifier"`
	State       link.State    `json:"state"`
	Destination string        `json:"destination"`
	Resolved    string        `json:"resolved_destination"`
	Campaign    shownCampaign `json:"campaign"`
	Title       *string       `json:"title"`
	Tags        []string      `json:"tags"`
	ExpiresAt   *time.Time    `json:"expires_at"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type shownCampaign struct {
	Source  *string `json:"source"`
	Medium  *string `json:"medium"`
	Name    *string `json:"name"`
	Term    *string `json:"term"`
	Content *string `json:"content"`
}

// show writes a link as the contract shows it. Times are given in UTC,
// whatever zone the database connection answers in.
func show(stored link.Link) shownLink {
	set := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	shown := shownLink{
		ID: stored.ID, Domain: stored.Domain, Identifier: stored.Identifier, State: stored.State,
		Destination: stored.Destination, Resolved: stored.Resolved,
		Campaign: shownCampaign{
			Source: set(stored.Campaign.Source), Medium: set(stored.Campaign.Medium), Name: set(stored.Campaign.Name),
			Term: set(stored.Campaign.Term), Content: set(stored.Campaign.Content),
		},
		Title:     set(stored.Title),
		Tags:      stored.Tags,
		CreatedAt: stored.CreatedAt.UTC(), UpdatedAt: stored.UpdatedAt.UTC(),
	}
	if stored.ExpiresAt != nil {
		expiry := stored.ExpiresAt.UTC()
		shown.ExpiresAt = &expiry
	}
	return shown
}

func (l links) create(w http.ResponseWriter, r *http.Request) {
	var sent struct {
		Destination string `json:"destination"`
		Alias       string `json:"alias"`
		Campaign    struct {
			Source  string `json:"source"`
			Medium  string `json:"medium"`
			Name    string `json:"name"`
			Term    string `json:"term"`
			Content string `json:"content"`
		} `json:"campaign"`
		Title     string     `json:"title"`
		Tags      []string   `json:"tags"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		// The contract let the body through and it still cannot be read: a
		// value its schema allows and its type here does not, such as a
		// time with a sixtieth second.
		writeRejected(w, rejection{In: inBody, Field: "", Code: ReasonInvalid})
		return
	}
	// A link can exist expired, and the store keeps what it is given. That a
	// client cannot create one that way is a rule of this operation.
	if sent.ExpiresAt != nil && !sent.ExpiresAt.After(l.now()) {
		writeRejected(w, rejection{In: inBody, Field: "/expires_at", Code: ReasonInThePast})
		return
	}

	created, err := l.store.Create(r.Context(), principalOf(r.Context()).WorkspaceID, link.Draft{
		Alias:       sent.Alias,
		Destination: sent.Destination,
		Campaign: link.Campaign{
			Source: sent.Campaign.Source, Medium: sent.Campaign.Medium, Name: sent.Campaign.Name,
			Term: sent.Campaign.Term, Content: sent.Campaign.Content,
		},
		Title:     sent.Title,
		Tags:      sent.Tags,
		ExpiresAt: sent.ExpiresAt,
	})
	if err != nil {
		refuse(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/links/"+created.ID)
	writeJSON(w, http.StatusCreated, show(created))
}

func (l links) get(w http.ResponseWriter, r *http.Request) {
	found, err := l.store.Get(r.Context(), principalOf(r.Context()).WorkspaceID, r.PathValue("id"))
	if err != nil {
		refuse(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, show(found))
}

func (l links) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// The validation of the contract has checked the limit and written the
	// default in its place when none was sent.
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil {
		writeRejected(w, rejection{In: inQuery, Field: "limit", Code: ReasonInvalid})
		return
	}
	after, valid := position(query.Get("cursor"))
	if !valid {
		writeRejected(w, rejection{In: inQuery, Field: "cursor", Code: ReasonInvalid})
		return
	}

	found, more, err := l.store.List(r.Context(), principalOf(r.Context()).WorkspaceID, after, limit)
	if errors.Is(err, link.ErrPage) {
		// A cursor that reads as text and is not a position.
		writeRejected(w, rejection{In: inQuery, Field: "cursor", Code: ReasonInvalid})
		return
	}
	if err != nil {
		refuse(w, r, err)
		return
	}
	page := struct {
		Items      []shownLink `json:"items"`
		NextCursor *string     `json:"next_cursor"`
	}{Items: make([]shownLink, len(found))}
	for i, stored := range found {
		page.Items[i] = show(stored)
	}
	if more {
		next := cursor(found[len(found)-1].ID)
		page.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, page)
}

// cursor writes the position after a link as the opaque text a client sends
// back. It is encoded so that nobody builds on what is inside: the position
// is the identifier of a link today, and may be something else later.
func cursor(after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(after))
}

// position reads a cursor. No cursor is the start.
func position(sent string) (string, bool) {
	if sent == "" {
		return "", true
	}
	decoded, err := base64.RawURLEncoding.DecodeString(sent)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}
