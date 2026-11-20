package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// metrics serves the operations on the metrics of a link. Like the ones on
// the link itself, each acts in the workspace of the key of the request.
type metrics struct {
	links *link.Store
	// inbox says how far the events are processed.
	inbox      *ingest.Store
	aggregates *rollup.Reader
}

// shownMetrics is what every answer about the metrics of a link carries
// (PLAN.md section 7): the period, how far the figures reach, whether they
// are of generated traffic and the versions of the rules of the period.
type shownMetrics struct {
	LinkID           string             `json:"link_id"`
	From             time.Time          `json:"from"`
	To               time.Time          `json:"to"`
	ProcessedThrough time.Time          `json:"processed_through"`
	Synthetic        bool               `json:"synthetic"`
	RuleVersions     []shownRuleVersion `json:"rule_versions"`
}

type shownRuleVersion struct {
	Version   int       `json:"version"`
	FirstHour time.Time `json:"first_hour"`
	LastHour  time.Time `json:"last_hour"`
}

// shownFigures is the metrics of a link, by the names of PLAN.md section 5.
type shownFigures struct {
	Requests             int64 `json:"requests"`
	InactiveRequests     int64 `json:"inactive_requests"`
	EstimatedHumanClicks int64 `json:"estimated_human_clicks"`
}

func showFigures(figures rollup.Figures) shownFigures {
	return shownFigures{
		Requests: figures.Requests, InactiveRequests: figures.InactiveRequests,
		EstimatedHumanClicks: figures.EstimatedHumanClicks,
	}
}

// period reads the period of a request, counted by hours or by days. It
// answers the request itself when the period is not one, and says so.
func period(w http.ResponseWriter, r *http.Request, by rollup.Granularity) (rollup.Period, bool) {
	query := r.URL.Query()
	// The contract has checked that both are there and look like moments.
	// One that looks like a moment and is not, such as the thirtieth of
	// February, is found here.
	var (
		moments  [2]time.Time
		rejected []rejection
	)
	for i, name := range []string{"from", "to"} {
		moment, err := time.Parse(time.RFC3339, query.Get(name))
		if err != nil {
			rejected = append(rejected, rejection{In: inQuery, Field: name, Code: ReasonInvalid})
		}
		moments[i] = moment
	}
	if len(rejected) > 0 {
		writeRejected(w, rejected...)
		return rollup.Period{}, false
	}
	asked, err := rollup.NewPeriod(moments[0], moments[1], by)
	if err != nil {
		refuse(w, r, err)
		return rollup.Period{}, false
	}
	return asked, true
}

// about reads what every answer carries, for the link of the request, and
// answers the request itself when it cannot: the workspace has no such
// link, or the database did not answer.
//
// How far the events are processed is read before any figure, here and
// never after: the figures read later hold at least what that moment says
// (docs/delivery-semantics.md, statement 7).
func (m metrics) about(w http.ResponseWriter, r *http.Request, asked rollup.Period) (shownMetrics, bool) {
	ctx := r.Context()
	id, demo, err := m.links.InWorkspace(ctx, principalOf(ctx).WorkspaceID, r.PathValue("id"))
	if err != nil {
		refuse(w, r, err)
		return shownMetrics{}, false
	}
	through, err := m.inbox.ProcessedThrough(ctx)
	if err != nil {
		fail(w, r, err)
		return shownMetrics{}, false
	}
	versions, err := m.aggregates.RuleVersions(ctx, asked)
	if err != nil {
		fail(w, r, err)
		return shownMetrics{}, false
	}
	shown := shownMetrics{
		LinkID: id, From: asked.From(), To: asked.To(), ProcessedThrough: through, Synthetic: demo,
		RuleVersions: make([]shownRuleVersion, len(versions)),
	}
	for i, version := range versions {
		shown.RuleVersions[i] = shownRuleVersion{Version: version.Version, FirstHour: version.FirstHour, LastHour: version.LastHour}
	}
	return shown, true
}

func (m metrics) summary(w http.ResponseWriter, r *http.Request) {
	asked, ok := period(w, r, rollup.ByHour)
	if !ok {
		return
	}
	about, ok := m.about(w, r, asked)
	if !ok {
		return
	}
	figures, err := m.aggregates.Summary(r.Context(), about.LinkID, asked)
	if err != nil {
		refuse(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		shownMetrics
		shownFigures
	}{about, showFigures(figures)})
}

func (m metrics) series(w http.ResponseWriter, r *http.Request) {
	// The contract has checked that the granularity is one of the two.
	by := rollup.Granularity(r.URL.Query().Get("granularity"))
	asked, ok := period(w, r, by)
	if !ok {
		return
	}
	about, ok := m.about(w, r, asked)
	if !ok {
		return
	}
	series, err := m.aggregates.Series(r.Context(), about.LinkID, asked)
	if err != nil {
		refuse(w, r, err)
		return
	}
	type shownPoint struct {
		Start time.Time `json:"start"`
		shownFigures
	}
	points := make([]shownPoint, len(series))
	for i, point := range series {
		points[i] = shownPoint{point.Start, showFigures(point.Figures)}
	}
	writeJSON(w, http.StatusOK, struct {
		shownMetrics
		Granularity rollup.Granularity `json:"granularity"`
		Points      []shownPoint       `json:"points"`
	}{about, by, points})
}

func (m metrics) breakdown(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// The validation of the contract has checked the limit and written the
	// default in its place when none was sent.
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil {
		writeRejected(w, rejection{In: inQuery, Field: "limit", Code: ReasonInvalid})
		return
	}
	asked, ok := period(w, r, rollup.ByDay)
	if !ok {
		return
	}
	about, ok := m.about(w, r, asked)
	if !ok {
		return
	}
	dimension := rollup.Dimension(query.Get("dimension"))
	shares, other, err := m.aggregates.Breakdown(r.Context(), about.LinkID, asked, dimension, limit)
	if err != nil {
		refuse(w, r, err)
		return
	}
	type shownShare struct {
		Value  string `json:"value"`
		Clicks int64  `json:"estimated_human_clicks"`
	}
	items := make([]shownShare, len(shares))
	for i, share := range shares {
		items[i] = shownShare{share.Value, share.Clicks}
	}
	writeJSON(w, http.StatusOK, struct {
		shownMetrics
		Dimension rollup.Dimension `json:"dimension"`
		Items     []shownShare     `json:"items"`
		Other     int64            `json:"other"`
	}{about, dimension, items, other})
}
