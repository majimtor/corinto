package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/rollup"
)

// ingestion serves the status of the ingestion: what became of the events
// of the workspace of the key, day by day (docs/delivery-semantics.md,
// reconciliation).
type ingestion struct {
	inbox      *ingest.Store
	aggregates *rollup.Reader
}

// shownDay is the terms of the reconciliation of one day.
type shownDay struct {
	Day          string `json:"day"`
	Served       int64  `json:"served"`
	Accepted     int64  `json:"accepted"`
	Lost         int64  `json:"lost"`
	Processed    int64  `json:"processed"`
	Pending      int64  `json:"pending"`
	DeadLettered int64  `json:"dead_lettered"`
}

func (i ingestion) status(w http.ResponseWriter, r *http.Request) {
	// The validation of the contract has checked the number of days and
	// written the default in its place when none was sent.
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil {
		writeRejected(w, rejection{In: inQuery, Field: "days", Code: ReasonInvalid})
		return
	}
	ctx := r.Context()
	// Of the whole service, like the moment of an answer about metrics.
	through, err := i.inbox.ProcessedThrough(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	terms, err := i.inbox.Reconciliation(ctx, principalOf(ctx).WorkspaceID, days)
	if err != nil {
		fail(w, r, err)
		return
	}
	finished, err := i.aggregates.LastRetention(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	type shownRetention struct {
		LastFinishedAt *time.Time `json:"last_finished_at"`
	}
	shown := struct {
		ProcessedThrough time.Time      `json:"processed_through"`
		Retention        shownRetention `json:"retention"`
		Days             []shownDay     `json:"days"`
	}{through, shownRetention{finished}, make([]shownDay, len(terms))}
	for n, day := range terms {
		shown.Days[n] = shownDay{
			Day: day.Day.Format(time.DateOnly), Served: day.Served, Accepted: day.Accepted, Lost: day.Lost,
			Processed: day.Processed, Pending: day.Pending, DeadLettered: day.DeadLettered,
		}
	}
	writeJSON(w, http.StatusOK, shown)
}
