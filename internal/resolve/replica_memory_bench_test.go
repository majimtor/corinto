package resolve

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// whole is a source that reads every link anew each time it is asked, as
// the database does: every reading brings rows with strings of their own.
type whole struct{ links int }

// Served returns the rows of the links, of the sizes real rows have: an
// identifier of 8 characters, the two identifiers of a row with 36 each,
// and a destination with its campaign parameters, 113 characters.
func (w whole) Served(context.Context) ([]link.Served, time.Time, error) {
	rows := make([]link.Served, w.links)
	for i := range rows {
		rows[i] = link.Served{
			ID:          fmt.Sprintf("01a1f954-%04x-7c3c-b9fa-%012x", i%65536, i),
			WorkspaceID: fmt.Sprintf("01a1f954-6e10-7a90-aa76-%012x", i%50),
			Identifier:  fmt.Sprintf("%08x", i),
			State:       link.StateActive,
			Resolved:    fmt.Sprintf("https://www.example.com/books/spring-reading-%07d?utm_source=newsletter&utm_medium=email&utm_campaign=spring-reading", i),
			Version:     1,
		}
	}
	return rows, databaseTime, nil
}

func (whole) ServedSince(context.Context, time.Time) ([]link.Served, time.Time, error) {
	return nil, databaseTime, nil
}

// The two things the runtime says of its memory that the figures are made
// of: what the objects of the heap take, and what the process holds from
// the system, which is everything it has asked for less what it has given
// back.
const (
	heapObjects = "/memory/classes/heap/objects:bytes"
	total       = "/memory/classes/total:bytes"
	released    = "/memory/classes/heap/released:bytes"
)

func read() (objects, fromTheSystem float64) {
	samples := []metrics.Sample{{Name: heapObjects}, {Name: total}, {Name: released}}
	metrics.Read(samples)
	return float64(samples[0].Value.Uint64()), float64(samples[1].Value.Uint64() - samples[2].Value.Uint64())
}

// settle collects what nothing holds and gives the memory back to the
// system, so that a reading after it is of what is alive.
func settle() {
	runtime.GC()
	debug.FreeOSMemory()
}

// BenchmarkMemory measures what the copy of the links costs in memory, at
// four sizes, in megabytes: what the copy holds once it is loaded, and the
// most the process holds from the system while the whole copy is read
// again, which is every five minutes. Then the rows that were just read
// and the copy that is being built are alive next to the copy that is
// still answering, and that moment is the one a memory limit has to fit.
// Both are over what the process held before it had any link. It is what
// ADR 0006 says is published: run it with
//
//	make bench-replica
func BenchmarkMemory(b *testing.B) {
	for _, links := range []int{100_000, 200_000, 250_000, 1_000_000} {
		b.Run(strconv.Itoa(links)+" links", func(b *testing.B) {
			var held, most float64
			for range b.N {
				held, most = memory(b, links)
			}
			b.ReportMetric(0, "ns/op")
			b.ReportMetric(held/float64(links), "held-B/link")
			b.ReportMetric(held/1e6, "held-MB")
			b.ReportMetric(most/1e6, "most-MB")
		})
	}
}

// memory loads a copy of so many links and reads it whole once more.
func memory(b *testing.B, links int) (held, most float64) {
	b.Helper()
	settle()
	objectsBefore, systemBefore := read()

	replica := New(whole{links: links}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := replica.Load(b.Context()); err != nil {
		b.Fatal(err)
	}
	settle()
	objects, _ := read()
	held = objects - objectsBefore

	// The most the process holds is read while the second reading runs:
	// once it has ended, the collector may already have given some back.
	var (
		highest  float64
		watching sync.WaitGroup
		done     = make(chan struct{})
	)
	watching.Go(func() {
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		for {
			if _, now := read(); now > highest {
				highest = now
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	})
	if err := replica.Load(b.Context()); err != nil {
		b.Fatal(err)
	}
	close(done)
	watching.Wait()
	if replica.Len() != links {
		b.Fatalf("the copy holds %d links, want %d", replica.Len(), links)
	}
	return held, highest - systemBefore
}
