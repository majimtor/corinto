package redirect

import (
	"sync/atomic"
	"time"
)

// serviceTimeBounds are where the buckets of the service time end, the
// same in every process and every run, so that two figures can be
// compared. Ten milliseconds, the objective of PLAN.md section 15, is one
// of them: whether the objective is met is read from the histogram itself.
var serviceTimeBounds = [...]time.Duration{
	50 * time.Microsecond, 100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond,
	25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
	time.Second,
}

// ServiceTime is how long the host of the short links took to answer each
// request since the process started: a histogram of fixed buckets, with
// nothing but counters in it (ADR 0020). The time is the one of the server
// alone, from the moment it takes a request to the moment it has nothing
// left to do for it, without the network on either side.
//
// A count is kept for ever and nothing is kept of a request. Whoever wants
// the figure of a stretch of time reads the histogram at its two ends and
// takes the difference.
type ServiceTime struct {
	buckets [len(serviceTimeBounds)]atomic.Uint64
	// over counts the requests that took longer than the last bound.
	over atomic.Uint64
}

// NewServiceTime returns a histogram with nothing counted.
func NewServiceTime() *ServiceTime {
	return &ServiceTime{}
}

// Observe counts a request that took so long, in the first bucket whose
// bound it does not pass. A host that was given no histogram measures
// nothing.
func (s *ServiceTime) Observe(took time.Duration) {
	if s == nil {
		return
	}
	for i, bound := range serviceTimeBounds {
		if took <= bound {
			s.buckets[i].Add(1)
			return
		}
	}
	s.over.Add(1)
}

// ServiceTimeBucket is how many requests took up to a time and longer than
// the bucket before it.
type ServiceTimeBucket struct {
	UpToMS   float64 `json:"up_to_ms"`
	Requests uint64  `json:"requests"`
}

// ServiceTimeSnapshot is the histogram at one moment. The buckets are not
// cumulative: each request is in one of them, or in Over, and Requests is
// what they add up to.
type ServiceTimeSnapshot struct {
	Requests uint64              `json:"requests"`
	Buckets  []ServiceTimeBucket `json:"buckets"`
	// Over is how many requests took longer than the last bucket holds.
	Over uint64 `json:"over"`
}

// Snapshot returns what has been counted so far.
func (s *ServiceTime) Snapshot() ServiceTimeSnapshot {
	snapshot := ServiceTimeSnapshot{Buckets: make([]ServiceTimeBucket, len(serviceTimeBounds))}
	for i, bound := range serviceTimeBounds {
		counted := s.buckets[i].Load()
		snapshot.Buckets[i] = ServiceTimeBucket{UpToMS: float64(bound.Microseconds()) / 1000, Requests: counted}
		snapshot.Requests += counted
	}
	snapshot.Over = s.over.Load()
	snapshot.Requests += snapshot.Over
	return snapshot
}
