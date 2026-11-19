package rollup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The two periods of PLAN.md section 5, written here a second time on
// purpose: what is deleted and when is a promise, not a setting.
func TestThePeriodsAreTheOnesThePlanStates(t *testing.T) {
	if RawEventsFor != 30*24*time.Hour || AggregatesForMonths != 13 {
		t.Errorf("raw events are kept %s and aggregates %d months", RawEventsFor, AggregatesForMonths)
	}
}

// A process asks for the run of the day when it starts and every hour. A
// run that failed is in the log, and is asked for again like any other.
func TestTheRetentionRunIsAskedForAtTheStartAndEveryHour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		away := errors.New("the database is away")
		// What each asking finds: a run done, nothing to do, a failure.
		finds := []error{nil, nil, away, nil}
		var asked []time.Duration
		began := time.Now()
		log := &bytes.Buffer{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			sweep(ctx, func(context.Context) (bool, error) {
				asked = append(asked, time.Since(began))
				if len(asked) > len(finds) {
					return false, nil
				}
				return true, finds[len(asked)-1]
			}, slog.New(slog.NewJSONHandler(log, nil)))
		}()
		time.Sleep(4*retentionCheckEvery + retentionCheckEvery/2)
		synctest.Wait()
		cancel()
		<-done

		want := []time.Duration{0, time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour}
		if !slices.Equal(asked, want) {
			t.Errorf("the run was asked for at %v, want %v", asked, want)
		}
		if failures := strings.Count(log.String(), "the retention run failed and will be tried again"); failures != 1 || !strings.Contains(log.String(), away.Error()) {
			t.Errorf("the log has %d failures, want the one with its cause: %s", failures, log.String())
		}
	})
}

// A run cut off by the stop of the process is not a failure, and the job
// stops with its context.
func TestTheRetentionJobStopsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &bytes.Buffer{}
		ctx, cancel := context.WithCancel(t.Context())
		asked := 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			sweep(ctx, func(ctx context.Context) (bool, error) {
				asked++
				// A run that lasts until the process is told to stop.
				<-ctx.Done()
				return true, ctx.Err()
			}, slog.New(slog.NewJSONHandler(log, nil)))
		}()
		time.Sleep(time.Minute)
		cancel()
		<-done
		if asked != 1 || log.Len() != 0 {
			t.Errorf("the run was asked for %d times and the log says %q; want once, and nothing", asked, log.String())
		}
	})
}
