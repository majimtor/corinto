package redirect_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/redirect"
)

// counted returns how many requests each bucket of a histogram has, and
// how many are past the last.
func counted(snapshot redirect.ServiceTimeSnapshot) ([]uint64, uint64) {
	requests := make([]uint64, len(snapshot.Buckets))
	for i, bucket := range snapshot.Buckets {
		requests[i] = bucket.Requests
	}
	return requests, snapshot.Over
}

func TestARequestIsCountedInTheFirstBucketItFits(t *testing.T) {
	measured := redirect.NewServiceTime()
	// The buckets are the same in every process: a figure is the bound of
	// one of them, and ten milliseconds, the objective, is a bound.
	bounds := []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 1000}
	empty := measured.Snapshot()
	if got := len(empty.Buckets); got != len(bounds) || empty.Requests != 0 || empty.Over != 0 {
		t.Fatalf("a new histogram has %d buckets and %d requests, want %d and none", got, empty.Requests, len(bounds))
	}
	for i, bucket := range empty.Buckets {
		if bucket.UpToMS != bounds[i] || bucket.Requests != 0 {
			t.Errorf("bucket %d ends at %v ms with %d requests, want %v and none", i, bucket.UpToMS, bucket.Requests, bounds[i])
		}
	}

	for _, took := range []time.Duration{
		// A request that took what a bucket holds at the most is in it,
		// and one a nanosecond longer is in the next.
		0, 50 * time.Microsecond, 50*time.Microsecond + 1,
		10 * time.Millisecond, 10 * time.Millisecond, 10*time.Millisecond + 1,
		time.Second, time.Second + 1, time.Hour,
		// A clock that went back measures less than nothing.
		-time.Millisecond,
	} {
		measured.Observe(took)
	}
	got := measured.Snapshot()
	requests, over := counted(got)
	if want := []uint64{3, 1, 0, 0, 0, 0, 0, 2, 1, 0, 0, 0, 1}; !slices.Equal(requests, want) || over != 2 || got.Requests != 10 {
		t.Errorf("the buckets hold %v and %d are past the last, of %d requests; want %v, 2 and 10", requests, over, got.Requests, want)
	}
	// As it is written where the probes ask: each request in one bucket,
	// and the buckets not added up.
	written, err := json.Marshal(redirect.ServiceTimeSnapshot{
		Requests: 3, Buckets: []redirect.ServiceTimeBucket{{UpToMS: 0.05, Requests: 2}, {UpToMS: 10, Requests: 0}}, Over: 1,
	})
	if want := `{"requests":3,"buckets":[{"up_to_ms":0.05,"requests":2},{"up_to_ms":10,"requests":0}],"over":1}`; err != nil || string(written) != want {
		t.Errorf("a histogram is written as %s (%v), want %s", written, err, want)
	}

	// A host that was given no histogram measures nothing, and does not
	// fail for it.
	var none *redirect.ServiceTime
	none.Observe(time.Millisecond)
}

func TestRequestsMeasuredAtOnceAreAllCounted(t *testing.T) {
	measured := redirect.NewServiceTime()
	const goroutines, each = 16, 5000
	var done sync.WaitGroup
	for n := range goroutines {
		done.Go(func() {
			for i := range each {
				measured.Observe(time.Duration(n*i) * time.Microsecond)
			}
		})
	}
	done.Wait()
	got := measured.Snapshot()
	requests, over := counted(got)
	var sum uint64
	for _, bucket := range requests {
		sum += bucket
	}
	if got.Requests != goroutines*each || sum+over != got.Requests {
		t.Errorf("%d requests are counted, %d in the buckets and %d past them; want %d", got.Requests, sum, over, goroutines*each)
	}
}

// slow is a recorder that takes its time over what a response leaves
// behind.
type slow struct{ takes time.Duration }

func (s slow) Record(ingest.Event)             { time.Sleep(s.takes) }
func (s slow) Count(ingest.Counter, time.Time) {}

// Every request is measured once, whatever the host answers, and for the
// whole time the host had it: the response leaves when the host is done,
// so what it does after writing the response is time the visitor waits.
func TestEveryRequestIsMeasuredOnceForAllTheTimeTheHostHadIt(t *testing.T) {
	measured := redirect.NewServiceTime()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	copied := resolveOver(t, catalogue()...)
	handler, err := redirect.New(t.Context(), redirect.Settings{
		Links: copied, Logger: logger, Now: func() time.Time { return now }, Rules: rules(t),
		Events: slow{takes: 30 * time.Millisecond}, ServiceTime: measured,
	})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, path string) int {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), method, path, nil))
		return response.Code
	}
	// A redirect, which leaves an event behind, and five answers that
	// leave none or only a count.
	if status := serve(http.MethodGet, "/k7m2xq9d"); status != http.StatusFound {
		t.Fatalf("GET of a link = %d, want 302", status)
	}
	after := measured.Snapshot()
	quick := 0
	for _, bucket := range after.Buckets {
		if bucket.UpToMS <= 25 {
			quick += int(bucket.Requests) //nolint:gosec // one request
		}
	}
	if after.Requests != 1 || quick != 0 {
		t.Errorf("a redirect whose event took 30 ms to hand over is measured as %+v, want one request past 25 ms", after)
	}
	for path, want := range map[string]int{"/": http.StatusFound, "/.well-known/security.txt": http.StatusOK, "/nobody-has-this": http.StatusNotFound} {
		if status := serve(http.MethodGet, path); status != want {
			t.Fatalf("GET %s = %d, want %d", path, status, want)
		}
	}
	if status := serve(http.MethodPost, "/k7m2xq9d"); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", status)
	}
	if got := measured.Snapshot().Requests; got != 5 {
		t.Errorf("%d requests are measured after five answers, want 5", got)
	}
	// The address has forty requests at once on a clock that stands still:
	// the rest are told to wait, and are measured like any other.
	refused := 0
	for range 40 {
		if serve(http.MethodGet, "/") == http.StatusTooManyRequests {
			refused++
		}
	}
	if got := measured.Snapshot().Requests; got != 45 || refused != 5 {
		t.Errorf("%d requests are measured and %d were told to wait, want 45 and 5", got, refused)
	}

	// A request that ends in a panic is measured too.
	broken, err := redirect.New(t.Context(), redirect.Settings{Logger: logger, Now: func() time.Time { return now }, ServiceTime: measured})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	broken.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/k7m2xq9d", nil))
	if got := measured.Snapshot().Requests; response.Code != http.StatusInternalServerError || got != 46 {
		t.Errorf("a request that panicked = %d and %d requests are measured, want 500 and 46", response.Code, got)
	}
}
