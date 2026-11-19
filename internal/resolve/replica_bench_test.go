package resolve

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// changing is a source of 100,000 links of which a hundred change on every
// poll, which is far more than a workspace changes in two seconds.
type changing struct {
	links   []link.Served
	version atomic.Int64
}

func (c *changing) Served(context.Context) ([]link.Served, time.Time, error) {
	return c.links, databaseTime, nil
}

func (c *changing) ServedSince(context.Context, time.Time) ([]link.Served, time.Time, error) {
	version := c.version.Add(1)
	changed := make([]link.Served, 100)
	for i := range changed {
		changed[i] = c.links[(int(version)*100+i)%len(c.links)]
		changed[i].Version = version + 1
	}
	return changed, databaseTime, nil
}

// BenchmarkLookup measures one lookup in a copy of 100,000 links, from as
// many goroutines as the -cpu flag says: with the copy at rest, and with a
// poll applying a hundred changes every millisecond, which is two thousand
// times as often as the replica polls. It is what the choice of a map under
// a read-write lock rests on: run it with
//
//	go test -run '^$' -bench Lookup -cpu 1,4 ./internal/resolve/
func BenchmarkLookup(b *testing.B) {
	read := &changing{links: make([]link.Served, 100_000)}
	identifiers := make([]string, len(read.links))
	for i := range read.links {
		identifiers[i] = "link-" + strconv.Itoa(i)
		read.links[i] = link.Served{
			ID: "id-" + strconv.Itoa(i), WorkspaceID: "workspace", Identifier: identifiers[i],
			State: link.StateActive, Resolved: "https://example.com/" + strconv.Itoa(i), Version: 1,
		}
	}
	replica := New(read, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := replica.Load(b.Context()); err != nil {
		b.Fatal(err)
	}
	lookups := func(b *testing.B) {
		b.RunParallel(func(next *testing.PB) {
			i := 0
			for next.Next() {
				if _, held := replica.Lookup(identifiers[i%len(identifiers)]); !held {
					b.Error("a link of the copy was not found")
					return
				}
				i += 7919
			}
		})
	}

	b.Run("at rest", lookups)
	b.Run("with a poll every millisecond", func(b *testing.B) {
		ctx, stop := context.WithCancel(b.Context())
		polling := make(chan struct{})
		go func() {
			defer close(polling)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := replica.Poll(ctx); err != nil && ctx.Err() == nil {
						b.Error(err)
						return
					}
				}
			}
		}()
		b.ResetTimer()
		lookups(b)
		b.StopTimer()
		stop()
		<-polling
	})
}
