package api

import (
	"context"
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

// optional is one field of a merge patch, which says three things where a
// field of a new link says two: it was left out, it is null, or it has a
// value.
type optional[T any] struct {
	sent  bool
	null  bool
	value T
}

// UnmarshalJSON is called only for a field that was sent, null included.
func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.sent = true
	if string(data) == "null" {
		o.null = true
		return nil
	}
	return json.Unmarshal(data, &o.value)
}

// changed returns what a store change takes for a field of text: nil when
// the field was left out, and an empty value, which removes it, when it is
// null.
func changed(field optional[string]) *string {
	if !field.sent {
		return nil
	}
	// The zero value of a null field is the empty text.
	return &field.value
}

func (l links) update(w http.ResponseWriter, r *http.Request) {
	var sent struct {
		Destination optional[string] `json:"destination"`
		Campaign    optional[struct {
			Source  optional[string] `json:"source"`
			Medium  optional[string] `json:"medium"`
			Name    optional[string] `json:"name"`
			Term    optional[string] `json:"term"`
			Content optional[string] `json:"content"`
		}] `json:"campaign"`
		Title     optional[string]    `json:"title"`
		Tags      optional[[]string]  `json:"tags"`
		ExpiresAt optional[time.Time] `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		// As in a new link: the contract let it through and it cannot be
		// read.
		writeRejected(w, rejection{In: inBody, Field: "", Code: ReasonInvalid})
		return
	}
	change := link.Change{Destination: changed(sent.Destination), Title: changed(sent.Title)}
	switch campaign := sent.Campaign; {
	case campaign.null:
		// No campaign at all: every parameter is removed.
		removed := ""
		change.Campaign = link.CampaignChange{
			Source: &removed, Medium: &removed, Name: &removed, Term: &removed, Content: &removed,
		}
	case campaign.sent:
		change.Campaign = link.CampaignChange{
			Source: changed(campaign.value.Source), Medium: changed(campaign.value.Medium),
			Name: changed(campaign.value.Name), Term: changed(campaign.value.Term),
			Content: changed(campaign.value.Content),
		}
	}
	if sent.Tags.sent {
		// A null leaves no tags, like an empty list.
		tags := sent.Tags.value
		if tags == nil {
			tags = []string{}
		}
		change.Tags = &tags
	}
	switch expiry := sent.ExpiresAt; {
	case expiry.null:
		change.NoExpiry = true
	case expiry.sent:
		// The rule of a new link: a client does not make a link expire in
		// the past. To stop a link it disables it.
		if !expiry.value.After(l.now()) {
			writeRejected(w, rejection{In: inBody, Field: "/expires_at", Code: ReasonInThePast})
			return
		}
		change.ExpiresAt = &expiry.value
	}

	updated, err := l.store.Update(r.Context(), principalOf(r.Context()).WorkspaceID, r.PathValue("id"), change)
	if err != nil {
		refuse(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, show(updated))
}

func (l links) disable(w http.ResponseWriter, r *http.Request) {
	l.switched(w, r, l.store.Disable)
}

func (l links) enable(w http.ResponseWriter, r *http.Request) {
	l.switched(w, r, l.store.Enable)
}

// switched answers the two operations that move a link between the states
// its workspace controls.
func (l links) switched(
	w http.ResponseWriter, r *http.Request,
	move func(ctx context.Context, workspaceID, id string) (link.Link, error),
) {
	moved, err := move(r.Context(), principalOf(r.Context()).WorkspaceID, r.PathValue("id"))
	if err != nil {
		refuse(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, show(moved))
}

func (l links) delete(w http.ResponseWriter, r *http.Request) {
	if err := l.store.Delete(r.Context(), principalOf(r.Context()).WorkspaceID, r.PathValue("id")); err != nil {
		refuse(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
