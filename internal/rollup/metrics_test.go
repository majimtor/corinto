package rollup

import (
	"errors"
	"testing"
	"time"
)

func TestAPeriodIsAlignedToWhatItIsCountedByAndBounded(t *testing.T) {
	utc := func(day, hour, minute, second int) time.Time {
		return time.Date(2026, time.November, day, hour, minute, second, 0, time.UTC)
	}
	// Five and a half hours ahead of UTC: on the hour there is the half
	// hour of UTC, and the other way round.
	kolkata := time.FixedZone("UTC+5:30", 5*60*60+30*60)
	for _, c := range []struct {
		name     string
		from, to time.Time
		by       Granularity
		want     error
		points   int
	}{
		{"one hour", utc(19, 10, 0, 0), utc(19, 11, 0, 0), ByHour, nil, 1},
		{"a day, by the hour", utc(19, 0, 0, 0), utc(20, 0, 0, 0), ByHour, nil, 24},
		{"a day, by the day", utc(19, 0, 0, 0), utc(20, 0, 0, 0), ByDay, nil, 1},
		{"hours of UTC said from another zone", time.Date(2026, time.November, 19, 15, 30, 0, 0, kolkata), time.Date(2026, time.November, 19, 17, 30, 0, 0, kolkata), ByHour, nil, 2},
		{"midnights of UTC said from another zone", time.Date(2026, time.November, 19, 5, 30, 0, 0, kolkata), time.Date(2026, time.November, 21, 5, 30, 0, 0, kolkata), ByDay, nil, 2},
		{"the longest period, by the hour", utc(1, 0, 0, 0), utc(1, 0, 0, 0).Add(MaxPeriod), ByHour, nil, 397 * 24},
		{"the longest period, by the day", utc(1, 0, 0, 0), utc(1, 0, 0, 0).Add(MaxPeriod), ByDay, nil, 397},

		{"a beginning a second past the hour", utc(19, 10, 0, 1), utc(19, 11, 0, 0), ByHour, ErrFromNotAligned, 0},
		{"a beginning a nanosecond past the hour", utc(19, 10, 0, 0).Add(time.Nanosecond), utc(19, 11, 0, 0), ByHour, ErrFromNotAligned, 0},
		{"a beginning on the hour somewhere else", time.Date(2026, time.November, 19, 10, 0, 0, 0, kolkata), utc(19, 11, 0, 0), ByHour, ErrFromNotAligned, 0},
		{"an end a minute before the hour", utc(19, 10, 0, 0), utc(19, 10, 59, 0), ByHour, ErrToNotAligned, 0},
		{"neither on the hour", utc(19, 10, 30, 0), utc(19, 11, 30, 0), ByHour, ErrFromNotAligned, 0},
		{"a beginning at one o'clock, by the day", utc(19, 1, 0, 0), utc(20, 0, 0, 0), ByDay, ErrFromNotAligned, 0},
		{"an end at eleven at night, by the day", utc(19, 0, 0, 0), utc(19, 23, 0, 0), ByDay, ErrToNotAligned, 0},
		{"midnights of somewhere else, by the day", time.Date(2026, time.November, 19, 0, 0, 0, 0, kolkata), time.Date(2026, time.November, 20, 0, 0, 0, 0, kolkata), ByDay, ErrFromNotAligned, 0},
		{"an end that is the beginning", utc(19, 10, 0, 0), utc(19, 10, 0, 0), ByHour, ErrPeriodEmpty, 0},
		{"an end before the beginning", utc(19, 10, 0, 0), utc(19, 9, 0, 0), ByHour, ErrPeriodEmpty, 0},
		{"an hour more than the longest", utc(1, 0, 0, 0), utc(1, 1, 0, 0).Add(MaxPeriod), ByHour, ErrPeriodTooLong, 0},
		{"a day more than the longest", utc(1, 0, 0, 0), utc(2, 0, 0, 0).Add(MaxPeriod), ByDay, ErrPeriodTooLong, 0},
	} {
		got, err := NewPeriod(c.from, c.to, c.by)
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			continue
		}
		if c.want != nil {
			if got != (Period{}) {
				t.Errorf("%s: a period that was refused is %+v", c.name, got)
			}
			continue
		}
		if !got.From().Equal(c.from) || !got.To().Equal(c.to) || got.From().Location() != time.UTC || got.To().Location() != time.UTC {
			t.Errorf("%s: the period is from %s to %s, want the same moments in UTC", c.name, got.From(), got.To())
		}
		if got.By() != c.by || got.points() != c.points {
			t.Errorf("%s: the period has %d points by the %s, want %d by the %s", c.name, got.points(), got.By(), c.points, c.by)
		}
	}
}

// The bounds are the ones PLAN.md section 7 states.
func TestTheBoundsOfMetricsAreTheOnesThePlanStates(t *testing.T) {
	if MaxPeriod != 397*24*time.Hour || MaxPoints != 744 || DefaultShares != 20 || MaxShares != 100 {
		t.Errorf("a period has at most %s, a series %d points, and a breakdown %d values of %d at the most",
			MaxPeriod, MaxPoints, DefaultShares, MaxShares)
	}
}
