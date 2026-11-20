package rollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
)

// Granularity is what a period is counted by.
type Granularity string

// The two granularities, which are the ones of the aggregates: the time
// series is kept by the hour, and the breakdowns by the day of UTC.
const (
	ByHour Granularity = "hour"
	ByDay  Granularity = "day"
)

const (
	// MaxPeriod is the longest period metrics are asked for: 397 days, the
	// most that the 13 months aggregates are kept for can be.
	MaxPeriod = 397 * 24 * time.Hour
	// MaxPoints is how many points a series has at the most: the hours of
	// 31 days.
	MaxPoints = 31 * 24
	// DefaultShares is how many values a breakdown names when it is not
	// told how many.
	DefaultShares = 20
	// MaxShares is how many values a breakdown names at the most. The
	// rest are added up.
	MaxShares = 100
)

// What a period, or what is asked of one, is refused for.
var (
	ErrFromNotAligned = errors.New("the beginning of a period is on the hour, or at midnight when it is counted by days, in UTC")
	ErrToNotAligned   = errors.New("the end of a period is on the hour, or at midnight when it is counted by days, in UTC")
	ErrPeriodEmpty    = errors.New("a period ends after it begins")
	ErrPeriodTooLong  = errors.New("a period has at most 397 days, and a series at most 744 points")
	// The two below are mistakes of the caller, not of a request.
	ErrNotWholeDays = errors.New("a breakdown is of a period counted by days")
	ErrShares       = errors.New("a breakdown names from 1 to 100 values")
)

// Period is the time metrics are asked for: from its beginning, which is
// part of it, to its end, which is not. Both are in UTC and aligned to
// what it is counted by.
type Period struct {
	from, to time.Time
	by       Granularity
}

// NewPeriod returns the period between two moments, counted by hours or by
// days of UTC.
func NewPeriod(from, to time.Time, by Granularity) (Period, error) {
	from, to = from.UTC(), to.UTC()
	step := time.Hour
	if by == ByDay {
		step = 24 * time.Hour
	}
	// UTC has no hour that is not sixty minutes long, and no day that is
	// not twenty-four hours long.
	switch {
	case !from.Truncate(step).Equal(from):
		return Period{}, ErrFromNotAligned
	case !to.Truncate(step).Equal(to):
		return Period{}, ErrToNotAligned
	case !to.After(from):
		return Period{}, ErrPeriodEmpty
	case to.Sub(from) > MaxPeriod:
		return Period{}, ErrPeriodTooLong
	}
	return Period{from: from, to: to, by: by}, nil
}

// From returns the beginning of the period, which is part of it.
func (p Period) From() time.Time { return p.from }

// To returns the end of the period, which is not part of it.
func (p Period) To() time.Time { return p.to }

// By returns what the period is counted by.
func (p Period) By() Granularity { return p.by }

// points returns how many hours, or days, the period has.
func (p Period) points() int {
	return int(p.to.Sub(p.from) / p.step())
}

// step is the length of an hour or of a day of UTC.
func (p Period) step() time.Duration {
	if p.by == ByDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// Figures are the metrics of a link over some time, as PLAN.md section 5
// names them.
type Figures struct {
	// Requests is how many times the link redirected, whoever asked.
	Requests int64
	// InactiveRequests is how many times the link was asked for while it
	// did not redirect.
	InactiveRequests int64
	// EstimatedHumanClicks is the redirects that the rules did not take
	// for automated: an estimate, by excess.
	EstimatedHumanClicks int64
}

// Point is the figures of one hour, or of one day of UTC, of a series.
type Point struct {
	Start time.Time
	Figures
}

// Share is one value of a breakdown, with the estimated human clicks that
// had it.
type Share struct {
	Value  string
	Clicks int64
}

// RuleVersion is a version of the classification rules, with the hours of
// the first and of the last event it classified, in any workspace.
type RuleVersion struct {
	Version             int
	FirstHour, LastHour time.Time
}

// Reader reads the aggregates. Whoever calls it has checked that the link
// is of the workspace that asks: the aggregates are read by the link
// alone.
type Reader struct {
	pool *pgxpool.Pool
}

// NewReader returns a reader of the aggregates over a pool.
func NewReader(pool *pgxpool.Pool) *Reader {
	return &Reader{pool: pool}
}

// The three figures, from rows of the time series. The enumerations are
// sent with each query, from the code that owns them.
const figures = `coalesce(sum(events) FILTER (WHERE outcome = $4), 0)::bigint,
       coalesce(sum(events) FILTER (WHERE outcome = $5), 0)::bigint,
       coalesce(sum(events) FILTER (WHERE outcome = $4 AND class = $6), 0)::bigint`

// Summary returns the figures of a link over a period.
func (r *Reader) Summary(ctx context.Context, linkID string, period Period) (Figures, error) {
	var total Figures
	err := r.pool.QueryRow(ctx,
		`SELECT `+figures+` FROM link_hourly WHERE link_id = $1::uuid AND hour >= $2 AND hour < $3`,
		linkID, period.from, period.to,
		string(ingest.OutcomeRedirected), string(ingest.OutcomeInactive), string(classify.ClassPresumedHuman),
	).Scan(&total.Requests, &total.InactiveRequests, &total.EstimatedHumanClicks)
	if err != nil {
		return Figures{}, fmt.Errorf("read the figures of a link: %w", err)
	}
	return total, nil
}

// Series returns the figures of a link for every hour, or every day of
// UTC, of a period, in order. An hour in which nothing happened is there,
// with zeros: whoever draws the series does not have to guess which points
// are missing.
func (r *Reader) Series(ctx context.Context, linkID string, period Period) ([]Point, error) {
	if period.points() > MaxPoints {
		return nil, ErrPeriodTooLong
	}
	// The day of an hour is the one of UTC, whatever zone the session of
	// the database is in.
	rows, err := r.pool.Query(ctx,
		`SELECT date_trunc($7, hour, 'UTC'), `+figures+`
		   FROM link_hourly WHERE link_id = $1::uuid AND hour >= $2 AND hour < $3
		  GROUP BY 1`,
		linkID, period.from, period.to,
		string(ingest.OutcomeRedirected), string(ingest.OutcomeInactive), string(classify.ClassPresumedHuman),
		string(period.by))
	if err != nil {
		return nil, fmt.Errorf("read the series of a link: %w", err)
	}
	found := make(map[int64]Figures)
	var (
		start time.Time
		of    Figures
	)
	_, err = pgx.ForEachRow(rows, []any{&start, &of.Requests, &of.InactiveRequests, &of.EstimatedHumanClicks}, func() error {
		found[start.Unix()] = of
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the series of a link: %w", err)
	}
	series := make([]Point, 0, period.points())
	for start := period.from; start.Before(period.to); start = start.Add(period.step()) {
		series = append(series, Point{Start: start, Figures: found[start.Unix()]})
	}
	return series, nil
}

// Breakdown returns the values of one dimension that had the most
// estimated human clicks of a link over a period of whole days, the most
// clicked first and at most limit of them, and how many clicks the values
// it does not name add up to. Those are the values past limit and, for the
// referrer host, the hosts past the ones a link keeps for a day.
func (r *Reader) Breakdown(ctx context.Context, linkID string, period Period, dimension Dimension, limit int) ([]Share, int64, error) {
	if period.by != ByDay {
		return nil, 0, ErrNotWholeDays
	}
	if limit < 1 || limit > MaxShares {
		return nil, 0, ErrShares
	}
	// The row of the hosts past the cap comes last: it is not a value to
	// name, and it is in what the others add up to. Values with as many
	// clicks go by their bytes, which is one order in every database
	// whatever language it sorts text in.
	rows, err := r.pool.Query(ctx,
		`SELECT v.value, v.clicks, (sum(v.clicks) OVER ())::bigint
		   FROM (SELECT value, sum(events)::bigint AS clicks
		           FROM link_daily_dimension
		          WHERE link_id = $1::uuid AND dimension = $2 AND day >= $3::date AND day < $4::date
		          GROUP BY value) AS v
		  ORDER BY v.value = $5, v.clicks DESC, v.value COLLATE "C"
		  LIMIT $6`,
		linkID, string(dimension), period.from, period.to, OtherReferrers, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read a breakdown of a link: %w", err)
	}
	var (
		shares       = make([]Share, 0, limit)
		share        Share
		total, named int64
	)
	_, err = pgx.ForEachRow(rows, []any{&share.Value, &share.Clicks, &total}, func() error {
		if share.Value != OtherReferrers {
			shares = append(shares, share)
			named += share.Clicks
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read a breakdown of a link: %w", err)
	}
	return shares, total - named, nil
}

// RuleVersions returns the versions of the classification rules that
// classified events between hours that meet a period, the one that began
// first at the head. More than one means that the rules changed, or that
// two were in use at once.
func (r *Reader) RuleVersions(ctx context.Context, period Period) ([]RuleVersion, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT version, first_hour, last_hour FROM rule_versions
		  WHERE first_hour < $2 AND last_hour >= $1
		  ORDER BY first_hour, version`, period.from, period.to)
	if err != nil {
		return nil, fmt.Errorf("read the versions of the rules: %w", err)
	}
	versions := []RuleVersion{}
	var version RuleVersion
	_, err = pgx.ForEachRow(rows, []any{&version.Version, &version.FirstHour, &version.LastHour}, func() error {
		version.FirstHour, version.LastHour = version.FirstHour.UTC(), version.LastHour.UTC()
		versions = append(versions, version)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the versions of the rules: %w", err)
	}
	return versions, nil
}
