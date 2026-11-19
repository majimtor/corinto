# Delivery semantics

What happens to an access event between the response that owes it and the
aggregate that counts it, and what happens when a part of that path fails.
This document is the only source of these statements (ADR 0005). It is
written before the code of the slice that implements it: each statement
names the test that checks it or says that it is a declared limit, and
until that slice closes a name may point at a test that is not written yet.

The redirect always answers; when something fails, analytics give way.

## Terms

- **Served**: a response that owes an event. A redirect and the 404 of an
  inactive link owe one; no other response does (`contract/redirect.md`).
- **Accepted**: an event inserted in the inbox table. Acceptance is the
  first moment the event is durable.
- **Lost**: served and never accepted.
- **Processed**: an accepted event that has been added to the aggregates.
- **Pending**: accepted and waiting, for its first attempt or a later one.
- **Dead-lettered**: accepted, failed on every attempt, and set aside with
  its reason.

## Statements

1. **The response never waits for the event.** The handler hands the event
   to a buffer in memory and answers; it performs no input or output for
   the event.
   Test: `TestTheResponseDoesNotWaitForTheEvent` in `internal/redirect`,
   which answers while the writer behind the buffer is blocked.

2. **The first hop is at most once.** Events wait in a bounded buffer of
   50,000 and are inserted into the inbox in batches, every second or every
   500 events, whichever comes first. A batch that the database does not
   accept stays in the buffer and is tried again at the next flush. A
   batch that the database refuses for what it holds, not because it is
   away, is inserted event by event, and an event refused on its own is
   dropped: it is lost, and counted as such. The insert ignores conflicts
   on the event identifier, so a batch written twice leaves one row per
   event.
   Tests: `TestTheBufferFlushesByCountAndByInterval` in `internal/ingest`;
   `TestABatchInsertedTwiceLeavesOneRowPerEvent` in `internal/ingest`,
   against PostgreSQL.

3. **A full buffer drops the new event, and the loss is counted.** The
   event that does not fit is dropped; the ones already waiting are kept.
   Loss is not estimated. It is the difference between two counts that do
   not share a path: the responses served, counted by the handler, and the
   events accepted, counted by the insert.
   Tests: `TestAFullBufferDropsTheNewEventAndCountsIt` in
   `internal/ingest`; `TestConcurrentWritersLoseNoEventThatFits` in
   `internal/ingest`, under the race detector.

4. **From acceptance on, delivery is at least once, and each accepted
   event counts exactly once.** The `worker` process takes pending events
   in batches of 500 with `FOR UPDATE SKIP LOCKED`. Adding a batch to the
   aggregates and marking it as processed happen in one transaction, so
   both happen or neither does. A consumer that stops before the commit
   leaves its batch pending for another one.
   Tests: `TestSeveralConsumersCountEachEventOnce` and
   `TestAggregateUpsertsInKeyOrderDoNotDeadlock` in `internal/rollup`,
   against PostgreSQL; the failure tests `TestAConsumerStoppedMidBatch` and
   `TestADuplicatedEvent` in `cmd/corinto`.

5. **An event that keeps failing is dead-lettered, never dropped.** An
   event has five attempts. After a failed attempt it waits 1 second, then
   5 seconds, 30 seconds and 2 minutes; after the fifth failure it moves to
   the dead-letter table with the reason of the last one. The operator can
   send a dead-lettered event back to the inbox, which gives it five
   attempts again. When a batch fails, its events are tried one by one, so
   one bad event does not hold the others back and only its own failures
   count as attempts.
   Tests: `TestAnEventThatExhaustsItsAttemptsIsDeadLettered`,
   `TestOneBadEventDoesNotHoldItsBatch` and
   `TestADeadLetteredEventCanBeReprocessed` in `internal/rollup`, against
   PostgreSQL.

6. **Order between events is not required.** Aggregates are sums, so any
   order of processing gives the same result.
   Test: `TestSeveralConsumersCountEachEventOnce`, which compares the
   aggregates with the events whatever order the consumers took them in.

7. **Metrics are eventually consistent, and every response says how far
   they reach.** A metrics response carries the instant up to which every
   accepted event has been processed or dead-lettered: the acceptance time
   of the oldest pending event, or the time of the response when none is
   pending. The instant refers to acceptance, not to the access. An event
   still in the buffer of `redirect` is not accepted yet, so it can belong
   to an hour the response already covers; while the database accepts
   writes that gap is at most one flush interval.
   Test: `TestMetricsSayThroughWhenTheyAreProcessed` in `internal/api`,
   against PostgreSQL.

## Reconciliation

Two equalities hold for every UTC day, counted by the day of the access,
and the ingestion-status operation of the API returns their terms:

- served = accepted + lost
- accepted = processed + pending + dead-lettered

Served is counted in the memory of `redirect` and written to the database
at each flush, apart from the events, so that a batch that fails does not
take the count with it. Lost is never stored: it is always the difference.
Events still waiting in the buffer are part of that difference until they
are accepted, so the lost count of a period settles once the buffer of
that period has been written.
Test: `TestTheReconciliationAddsUp` in `internal/ingest`, against
PostgreSQL, which drops events on purpose and reads both equalities.

## When something fails

| What fails | What happens | Where it shows | Checked by |
| --- | --- | --- | --- |
| The database is down | Redirects continue from the replica. Events wait in the buffer; once it is full, new ones are dropped and counted when the database returns. The API answers `503` | Age of the replica; lost count | `TestTheDatabaseAbsent` in `cmd/corinto` |
| `worker` is stopped | Accepted events stay pending. Nothing is lost | Pending count; the instant of statement 7 stops advancing | `TestAConsumerStoppedMidBatch` in `cmd/corinto` |
| The buffer is full | The new event is dropped and counted | Lost count | Statement 3 |
| The inbox refuses an event | It is dropped; the rest of its batch is accepted | Lost count; an error in the log | `TestOneRefusedEventDoesNotHoldItsBatch` in `internal/ingest` |
| `redirect` crashes | The events in its memory are lost and not counted, together with the served count not yet written | Nowhere: see the declared limits | None |
| A batch is retried, or an event arrives twice | The second copy is ignored by its identifier | Nothing to show | `TestADuplicatedEvent` in `cmd/corinto` |
| An event cannot be processed | It is retried, then dead-lettered with its reason | Dead-letter count | Statement 5 |

How `redirect` keeps resolving links while the database is down, and what
it does when the database is absent at start, is in PLAN.md section 3.

## Declared limits

- **Loss on a crash is not counted.** A `redirect` process that dies
  without flushing loses what it held in memory: at most one flush
  interval of events while the database was accepting them, and up to the
  whole buffer, 50,000 events, if the database was down. The served count
  of that same period dies with it, so the reconciliation does not show
  the loss. A clean stop flushes first, for at most 3 seconds: what the
  database has not taken by then is lost like the rest. ADR 0005 names
  what would remove this limit.
- **A served count can be written twice.** If the database commits the
  counts of a flush and its answer is lost, they are written again with
  the next one, and that day shows more served, and so more lost, than
  there was. The error is towards declaring loss. The accepted count
  cannot be written twice: the insert that counts it ignores an event that
  is already there.
- **Exactly once ends at the aggregates.** The guarantee of statement 4
  rests on one PostgreSQL transaction. Nothing outside the database is
  told about an event in S1; when something is, that step has its own
  statement here.
- **An outage longer than the buffer loses events.** The buffer holds
  about 69 minutes of the average design volume of PLAN.md section 15, and
  under three minutes of its burst.
- **Time is the server's.** An event carries the time of the process that
  served it. Two processes with different clocks can place events of the
  same instant in different hours.

## Parameters

| Parameter | Value |
| --- | --- |
| Buffer | 50,000 events |
| Flush | Every second, or every 500 events |
| Last flush when the process stops | At most 3 seconds |
| Consumer batch | 500 events |
| Consumer poll | Every second, and at once after a whole batch |
| Attempts per event | 5 |
| Waits between attempts | 1 s, 5 s, 30 s, 2 min |
