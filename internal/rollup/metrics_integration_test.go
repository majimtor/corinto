//go:build integration

package rollup

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
)

// between returns a period, which must be one.
func between(t *testing.T, from, to time.Time, by Granularity) Period {
	t.Helper()
	period, err := NewPeriod(from, to, by)
	if err != nil {
		t.Fatal(err)
	}
	return period
}

// behind returns a reader whose sessions in the database are in New York,
// which is behind UTC and set its clocks back on 1 November 2026: a day of
// 25 hours there. What a reader answers is of UTC wherever its database
// believes it is.
func (s *stage) behind(t *testing.T) *Reader {
	t.Helper()
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["timezone"] = "America/New_York"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var zone string
	if err := pool.QueryRow(t.Context(), `SHOW timezone`).Scan(&zone); err != nil || zone != "America/New_York" {
		t.Fatalf("the sessions of the reader are in %q (%v)", zone, err)
	}
	return NewReader(pool)
}

// as returns the change that gives an event a traffic class.
func as(class classify.Class) func(*ingest.Event) {
	return func(e *ingest.Event) { e.Classification.Class = class }
}

func inactive(e *ingest.Event) { e.Outcome = ingest.OutcomeInactive }

func TestTheSummaryOfALinkAddsUpTheHoursOfThePeriod(t *testing.T) {
	west(t)
	s := newStage(t)
	s.accept(t,
		// The first second of the period, and the last.
		click(s.spring, s.acme, at(10, 0, 0)),
		click(s.spring, s.acme, at(12, 59, 59)),
		click(s.spring, s.acme, at(11, 30, 0)),
		// Redirects that the rules took for automated: requests, and not
		// clicks.
		with(click(s.spring, s.acme, at(11, 0, 0)), as(classify.ClassCrawler)),
		with(click(s.spring, s.acme, at(12, 0, 0)), as(classify.ClassPreview)),
		// The link did not redirect, whoever asked.
		with(click(s.spring, s.acme, at(11, 0, 0)), inactive),
		with(with(click(s.spring, s.acme, at(11, 0, 0)), inactive), as(classify.ClassAutomation)),
		// The second before the period, and the first after it.
		click(s.spring, s.acme, at(9, 59, 59)),
		click(s.spring, s.acme, at(13, 0, 0)),
		// Another link of the workspace, and a link of another.
		click(s.summer, s.acme, at(11, 0, 0)),
		click(s.autumn, s.globex, at(11, 0, 0)),
	)
	s.drain(t, 1)

	for _, reader := range map[string]*Reader{"in UTC": NewReader(s.pool), "behind UTC": s.behind(t)} {
		got, err := reader.Summary(t.Context(), s.spring, between(t, at(10, 0, 0), at(13, 0, 0), ByHour))
		if want := (Figures{Requests: 5, InactiveRequests: 2, EstimatedHumanClicks: 3}); err != nil || got != want {
			t.Errorf("Summary = %+v, %v; want %+v", got, err, want)
		}
		// Hours in which nothing happened, and a link nobody has.
		if got, err := reader.Summary(t.Context(), s.spring, between(t, at(14, 0, 0), at(20, 0, 0), ByHour)); err != nil || got != (Figures{}) {
			t.Errorf("Summary of hours with nothing = %+v, %v; want zeros", got, err)
		}
		if got, err := reader.Summary(t.Context(), "00000000-0000-7000-8000-000000000000", between(t, at(10, 0, 0), at(13, 0, 0), ByHour)); err != nil || got != (Figures{}) {
			t.Errorf("Summary of a link nobody has = %+v, %v; want zeros", got, err)
		}
	}
}

// same reports whether two series are equal, point by point.
func same(got, want []Point) bool {
	return slices.EqualFunc(got, want, func(a, b Point) bool {
		return a.Start.Equal(b.Start) && a.Start.Location() == time.UTC && a.Figures == b.Figures
	})
}

func TestTheSeriesOfALinkHasEveryHourOfThePeriod(t *testing.T) {
	west(t)
	s := newStage(t)
	s.accept(t,
		click(s.spring, s.acme, at(10, 0, 0)),
		with(click(s.spring, s.acme, at(10, 59, 59)), as(classify.ClassCrawler)),
		click(s.spring, s.acme, at(12, 30, 0)),
		with(click(s.spring, s.acme, at(12, 31, 0)), inactive),
		// Outside the period, and of another link.
		click(s.spring, s.acme, at(9, 59, 59)),
		click(s.spring, s.acme, at(14, 0, 0)),
		click(s.summer, s.acme, at(11, 0, 0)),
	)
	s.drain(t, 1)

	for name, reader := range map[string]*Reader{"in UTC": NewReader(s.pool), "behind UTC": s.behind(t)} {
		got, err := reader.Series(t.Context(), s.spring, between(t, at(10, 0, 0), at(14, 0, 0), ByHour))
		want := []Point{
			{at(10, 0, 0), Figures{Requests: 2, EstimatedHumanClicks: 1}},
			{at(11, 0, 0), Figures{}},
			{at(12, 0, 0), Figures{Requests: 1, InactiveRequests: 1, EstimatedHumanClicks: 1}},
			{at(13, 0, 0), Figures{}},
		}
		if err != nil || !same(got, want) {
			t.Errorf("%s: Series = %+v, %v; want %+v", name, got, err, want)
		}
	}

	// A series by the hour has 744 points at the most, the hours of 31
	// days; one more is refused before anything is read.
	reader := NewReader(s.pool)
	longest, err := reader.Series(t.Context(), s.spring, between(t, day, day.Add(MaxPoints*time.Hour), ByHour))
	if err != nil || len(longest) != MaxPoints || !longest[10].Start.Equal(at(10, 0, 0)) || longest[10].Requests != 2 {
		t.Errorf("the longest series by the hour has %d points (%v), want %d", len(longest), err, MaxPoints)
	}
	if _, err := reader.Series(t.Context(), s.spring, between(t, day, day.Add((MaxPoints+1)*time.Hour), ByHour)); !errors.Is(err, ErrPeriodTooLong) {
		t.Errorf("a series of %d hours: err = %v, want %v", MaxPoints+1, err, ErrPeriodTooLong)
	}
}

func TestTheDaysOfASeriesAreTheOnesOfUTC(t *testing.T) {
	west(t)
	s := newStage(t)
	on := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	s.accept(t,
		// The first and the last minute of a day of UTC.
		click(s.spring, s.acme, on(time.October, 31, 0, 0)),
		click(s.spring, s.acme, on(time.October, 31, 23, 59)),
		// The day New York set its clocks back, hour by hour around it.
		click(s.spring, s.acme, on(time.November, 1, 0, 30)),
		click(s.spring, s.acme, on(time.November, 1, 5, 30)),
		with(click(s.spring, s.acme, on(time.November, 1, 6, 30)), as(classify.ClassCrawler)),
		with(click(s.spring, s.acme, on(time.November, 1, 23, 30)), inactive),
		// A day with nothing, and then one more.
		click(s.spring, s.acme, on(time.November, 3, 12, 0)),
		// The minute before the period, and the first after it.
		click(s.spring, s.acme, on(time.October, 30, 23, 59)),
		click(s.spring, s.acme, on(time.November, 4, 0, 0)),
	)
	s.drain(t, 1)

	want := []Point{
		{on(time.October, 31, 0, 0), Figures{Requests: 2, EstimatedHumanClicks: 2}},
		{on(time.November, 1, 0, 0), Figures{Requests: 3, InactiveRequests: 1, EstimatedHumanClicks: 2}},
		{on(time.November, 2, 0, 0), Figures{}},
		{on(time.November, 3, 0, 0), Figures{Requests: 1, EstimatedHumanClicks: 1}},
	}
	for name, reader := range map[string]*Reader{"in UTC": NewReader(s.pool), "behind UTC": s.behind(t)} {
		got, err := reader.Series(t.Context(), s.spring, between(t, on(time.October, 31, 0, 0), on(time.November, 4, 0, 0), ByDay))
		if err != nil || !same(got, want) {
			t.Errorf("%s: Series = %+v, %v; want %+v", name, got, err, want)
		}
	}
	// By the day, the longest period there is has fewer points than a
	// series may have.
	longest, err := NewReader(s.pool).Series(t.Context(), s.spring, between(t, on(time.October, 31, 0, 0), on(time.October, 31, 0, 0).Add(MaxPeriod), ByDay))
	if err != nil || len(longest) != 397 || longest[1].Requests != 3 {
		t.Errorf("the longest series by the day has %d points (%v), want 397", len(longest), err)
	}
}

func TestABreakdownNamesTheValuesWithTheMostClicksAndAddsUpTheRest(t *testing.T) {
	west(t)
	s := newStage(t)
	from := func(host string, on time.Time, n int) []ingest.Event {
		events := make([]ingest.Event, n)
		for i := range events {
			events[i] = with(click(s.spring, s.acme, on), func(e *ingest.Event) { e.ReferrerHost = host })
		}
		return events
	}
	// Two days. The first minute of the first, and the last of the second.
	var events []ingest.Event
	events = append(events, from("news.example.com", at(0, 0, 0), 3)...)
	events = append(events, from("news.example.com", at(47, 59, 0), 2)...)
	events = append(events, from("blog.example.org", at(10, 0, 0), 3)...)
	events = append(events, from("forum.example.org", at(30, 0, 0), 3)...)
	events = append(events, from("", at(10, 0, 0), 2)...)
	// On a tablet with Firefox, the two of them.
	events = append(events, with(click(s.spring, s.acme, at(12, 0, 0)), func(e *ingest.Event) {
		e.ReferrerHost, e.Classification.Device, e.Classification.Browser = "mail.example.net", classify.DeviceTablet, classify.BrowserFirefox
	}), with(click(s.spring, s.acme, at(36, 0, 0)), func(e *ingest.Event) {
		e.ReferrerHost, e.Classification.Device, e.Classification.Browser = "mail.example.net", classify.DeviceTablet, classify.BrowserFirefox
	}))
	// Not estimated human clicks: in no breakdown.
	events = append(events, with(from("search.example.net", at(10, 0, 0), 1)[0], as(classify.ClassCrawler)),
		with(from("search.example.net", at(10, 0, 0), 1)[0], inactive))
	// The minute before the period, the first after it, and another link.
	events = append(events, from("before.example.com", at(-1, 59, 0), 9)...)
	events = append(events, from("after.example.com", at(48, 0, 0), 9)...)
	for i := range 9 {
		events = append(events, with(click(s.summer, s.acme, at(10, i, 0)), func(e *ingest.Event) { e.ReferrerHost = "summer.example.com" }))
	}
	s.accept(t, events...)
	s.drain(t, 1)
	// The hosts past the cap of the first day, as a consumer leaves them.
	if _, err := s.pool.Exec(t.Context(),
		`INSERT INTO link_daily_dimension (link_id, workspace_id, day, dimension, value, events)
		 VALUES ($1::uuid, $2::uuid, '2026-11-19', 'referrer_host', $3, 7)`, s.spring, s.acme, OtherReferrers); err != nil {
		t.Fatal(err)
	}

	both := between(t, day, day.Add(48*time.Hour), ByDay)
	type want struct {
		shares []Share
		other  int64
	}
	for name, reader := range map[string]*Reader{"in UTC": NewReader(s.pool), "behind UTC": s.behind(t)} {
		for _, c := range []struct {
			name      string
			period    Period
			dimension Dimension
			limit     int
			want
		}{
			// Past the cap come the most clicks of all, and they are never a
			// value. Two hosts are level, and go by their names.
			{"the two hosts with the most clicks", both, DimensionReferrerHost, 2,
				want{[]Share{{"news.example.com", 5}, {"blog.example.org", 3}}, 3 + 2 + 2 + 7}},
			{"three hosts", both, DimensionReferrerHost, 3,
				want{[]Share{{"news.example.com", 5}, {"blog.example.org", 3}, {"forum.example.org", 3}}, 2 + 2 + 7}},
			{"as many hosts as there are", both, DimensionReferrerHost, 5,
				want{[]Share{{"news.example.com", 5}, {"blog.example.org", 3}, {"forum.example.org", 3}, {NoReferrer, 2}, {"mail.example.net", 2}}, 7}},
			{"more hosts than there are", both, DimensionReferrerHost, MaxShares,
				want{[]Share{{"news.example.com", 5}, {"blog.example.org", 3}, {"forum.example.org", 3}, {NoReferrer, 2}, {"mail.example.net", 2}}, 7}},
			{"the first day", between(t, day, nextDay, ByDay), DimensionReferrerHost, MaxShares,
				want{[]Share{{"blog.example.org", 3}, {"news.example.com", 3}, {NoReferrer, 2}, {"mail.example.net", 1}}, 7}},
			{"the second day", between(t, nextDay, nextDay.Add(24*time.Hour), ByDay), DimensionReferrerHost, MaxShares,
				want{[]Share{{"forum.example.org", 3}, {"news.example.com", 2}, {"mail.example.net", 1}}, 0}},
			{"by device", both, DimensionDevice, MaxShares, want{[]Share{{"desktop", 13}, {"tablet", 2}}, 0}},
			{"by device, one value", both, DimensionDevice, 1, want{[]Share{{"desktop", 13}}, 2}},
			{"by browser", both, DimensionBrowser, DefaultShares, want{[]Share{{"chrome", 13}, {"firefox", 2}}, 0}},
			{"a day with nothing", between(t, day.Add(72*time.Hour), day.Add(96*time.Hour), ByDay), DimensionReferrerHost, 5, want{[]Share{}, 0}},
		} {
			shares, other, err := reader.Breakdown(t.Context(), s.spring, c.period, c.dimension, c.limit)
			if err != nil || shares == nil || !slices.Equal(shares, c.shares) || other != c.other {
				t.Errorf("%s, %s: Breakdown = %v and %d others, %v; want %v and %d", name, c.name, shares, other, err, c.shares, c.other)
			}
		}
	}

	// What only a mistake of the caller asks for.
	reader := NewReader(s.pool)
	if _, _, err := reader.Breakdown(t.Context(), s.spring, between(t, day, nextDay, ByHour), DimensionDevice, 5); !errors.Is(err, ErrNotWholeDays) {
		t.Errorf("a breakdown of a period by the hour: err = %v, want %v", err, ErrNotWholeDays)
	}
	for _, limit := range []int{-1, 0, MaxShares + 1} {
		if _, _, err := reader.Breakdown(t.Context(), s.spring, both, DimensionDevice, limit); !errors.Is(err, ErrShares) {
			t.Errorf("a breakdown of %d values: err = %v, want %v", limit, err, ErrShares)
		}
	}
}

func TestTheVersionsOfAPeriodAreTheOnesWhoseHoursMeetIt(t *testing.T) {
	west(t)
	s := newStage(t)
	hour := func(n int) time.Time { return day.Add(time.Duration(n) * time.Hour) }
	// The second version began while the first was in use; the third began
	// later, and has the smallest number: a version that was put back.
	for _, version := range []struct{ version, first, last int }{{2, 0, 10}, {3, 8, 20}, {1, 30, 40}} {
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO rule_versions (version, first_hour, last_hour) VALUES ($1, $2, $3)`,
			version.version, hour(version.first), hour(version.last)); err != nil {
			t.Fatal(err)
		}
	}
	for name, reader := range map[string]*Reader{"in UTC": NewReader(s.pool), "behind UTC": s.behind(t)} {
		for _, c := range []struct {
			name     string
			from, to int
			by       Granularity
			want     []int
		}{
			{"the last hour of a version is the first of the period", 10, 11, ByHour, []int{2, 3}},
			{"the hour after the last of a version", 11, 12, ByHour, []int{3}},
			{"a period that ends where a version begins", 0, 8, ByHour, []int{2}},
			{"a period that ends an hour into a version", 0, 9, ByHour, []int{2, 3}},
			{"hours between two versions", 21, 30, ByHour, []int{}},
			{"from the last hour of one to the first of the next", 20, 31, ByHour, []int{3, 1}},
			{"before any version", -24, 0, ByDay, []int{}},
			{"the first day", 0, 24, ByDay, []int{2, 3}},
			{"the second day", 24, 48, ByDay, []int{1}},
			{"the third day", 48, 72, ByDay, []int{}},
			{"all of it, in the order in which they began", -24, 72, ByDay, []int{2, 3, 1}},
		} {
			versions, err := reader.RuleVersions(t.Context(), between(t, hour(c.from), hour(c.to), c.by))
			if err != nil || versions == nil {
				t.Errorf("%s, %s: RuleVersions = %v, %v", name, c.name, versions, err)
				continue
			}
			got := make([]int, len(versions))
			for i, version := range versions {
				got[i] = version.Version
				if version.FirstHour.Location() != time.UTC || version.LastHour.Location() != time.UTC {
					t.Errorf("%s, %s: the hours of version %d are not in UTC", name, c.name, version.Version)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("%s, %s: the versions are %v, want %v", name, c.name, got, c.want)
			}
		}
		all, err := reader.RuleVersions(t.Context(), between(t, hour(-24), hour(72), ByDay))
		if err != nil || len(all) != 3 || !all[1].FirstHour.Equal(hour(8)) || !all[1].LastHour.Equal(hour(20)) {
			t.Errorf("%s: the versions are %+v (%v), want the second from %s to %s", name, all, err, hour(8), hour(20))
		}
	}
}
