package ingest

import (
	"context"
	"fmt"
	"log/slog"
)

// deliverAttempts is how many times Deliver asks the inbox to take what it
// did not take, before it gives up. A write that ran out of its time on a
// busy machine goes through at the next one; a database that is away does
// not, however often it is asked.
const deliverAttempts = 5

// Deliver writes the events of responses that were served, and returns
// when the inbox has them all. It hands them to a buffer and empties it,
// so each event gets its identifier, each response is counted as served
// and the counts are written before the events, by the code that does so
// for the redirect. What differs is who waits: the redirect never waits
// for the inbox, and whoever calls this has nothing else to do. It is the
// seed that calls it, with a history it made up.
func Deliver(ctx context.Context, inbox Inbox, logger *slog.Logger, events []Event) error {
	buffer := NewBuffer(inbox, logger)
	for len(events) > 0 {
		// No more at once than the buffer holds: past that it drops them.
		size := min(len(events), Capacity)
		for _, event := range events[:size] {
			buffer.Record(event)
		}
		events = events[size:]
		for attempt := 1; ; attempt++ {
			err := buffer.empty(ctx)
			if err == nil {
				break
			}
			if attempt == deliverAttempts || ctx.Err() != nil {
				return fmt.Errorf("deliver events to the inbox: %w", err)
			}
		}
	}
	return nil
}
