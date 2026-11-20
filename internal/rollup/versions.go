package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/ingest"
)

// span is the hours, counted from 1970, of the first and of the last event
// that a version of the rules classified.
type span struct{ first, last int64 }

// recordVersions records the versions of the rules that classified events,
// and the hours between which each did. The aggregates do not say which
// version classified what they count; this is where a response with
// metrics finds the versions of its period (PLAN.md section 5).
//
// The row of a version is one for every consumer, and whoever writes it
// holds it until its commit. So it is written only when the events reach
// an hour that the row does not cover, which is the first batch of each
// hour, and it is the last thing a transaction writes.
func recordVersions(ctx context.Context, tx pgx.Tx, events []ingest.Event) error {
	seen := make(map[int]span)
	for _, event := range events {
		hour := event.OccurredAt.Unix() / secondsInAnHour
		of, known := seen[event.Classification.Version]
		if !known {
			of = span{first: hour, last: hour}
		}
		seen[event.Classification.Version] = span{first: min(of.first, hour), last: max(of.last, hour)}
	}
	var (
		versions    = make([]int64, 0, len(seen))
		first, last = make([]time.Time, 0, len(seen)), make([]time.Time, 0, len(seen))
	)
	for version, hours := range seen {
		versions = append(versions, int64(version))
		first = append(first, time.Unix(hours.first*secondsInAnHour, 0).UTC())
		last = append(last, time.Unix(hours.last*secondsInAnHour, 0).UTC())
	}
	// The row that is there may be of a moment ago: a consumer that finds
	// it changed, or held, takes the smaller and the greater of what it
	// has and what it brings.
	_, err := tx.Exec(ctx,
		`INSERT INTO rule_versions (version, first_hour, last_hour)
		 SELECT s.version, s.first_hour, s.last_hour
		   FROM unnest($1::bigint[], $2::timestamptz[], $3::timestamptz[]) AS s (version, first_hour, last_hour)
		  WHERE NOT EXISTS (
		        SELECT FROM rule_versions AS r
		         WHERE r.version = s.version AND r.first_hour <= s.first_hour AND r.last_hour >= s.last_hour)
		  ORDER BY s.version
		 ON CONFLICT (version) DO UPDATE
		    SET first_hour = least(rule_versions.first_hour, EXCLUDED.first_hour),
		        last_hour = greatest(rule_versions.last_hour, EXCLUDED.last_hour)`,
		versions, first, last)
	if err != nil {
		return fmt.Errorf("record the versions of the rules: %w", err)
	}
	return nil
}
