# 0004. PostgreSQL as store and queue

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Accepted events have to be aggregated asynchronously, at least once, by an
idempotent consumer. The design load and its bursts are the objectives of
PLAN.md section 15.

Sizing probes on 2026-11-12, PostgreSQL 18 in a container limited to
0.25 CPU and 1 GB, with the load tool inside the same limit (other
conditions in ADR 0020):

- Inserting batches of 500 while two consumers aggregated: about 3,000
  events per second for 30 seconds, no failed transaction, and the sum of
  the aggregates equal to the rows processed.
- With four consumers taking batches of 100 and an aggregate upsert that
  did not order its rows, 15 % of the transactions died in deadlocks.
  Ordering the rows by key removed them.
- 233 bytes per event with its indexes.

## Decision

PostgreSQL 18 holds everything: links, the event inbox, aggregates and
webhook receipts. The inbox is a table consumed in batches with
`FOR UPDATE SKIP LOCKED`; marking a batch as processed and adding it to the
aggregates happen in one transaction. Aggregate upserts order their rows by
key.

## Alternatives considered

- Redis Streams. In a probe, the default image configuration lost all
  50,000 entries after the process was killed; with the append-only file it
  kept them, and its documentation accepts the loss of one second. Dead
  letters are a pattern the application writes. About three times faster
  than PostgreSQL at comparable durability, which the load does not need.
- RabbitMQ 4.4. Quorum queues with confirmations give at-least-once
  delivery; dead-lettering is at-most-once unless configured. The heaviest
  option to operate.
- NATS JetStream 2.15. In a probe it redelivered three times and then
  published an advisory, with no dead-letter queue. By default it
  acknowledges before syncing to disk, every two minutes; an independent
  analysis lost acknowledged messages in a simulated power failure.

None of them solves a requirement the table fails.

## Consequences

- Positive: one service to run and to reason about; the accounting of
  accepted against processed is a query.
- Negative: ingestion depends on the availability of the same database that
  serves management. What the buffer cannot hold during an outage is lost
  and counted (ADR 0005).
- Strongest argument against: a queue inside the database couples two
  concerns that a broker would keep apart.

## Reopen when

The break test falls below the burst objective, a requirement appears to
accept events through long database outages, or the storage objective of
PLAN.md section 15 is missed.

## Links

ADR 0005, ADR 0010, ADR 0020. PLAN.md sections 4, 8 and 15. Redis persistence
(redis.io/docs), RabbitMQ quorum queues (rabbitmq.com/docs), Jepsen analysis
of NATS 2.12.1 (jepsen.io), consulted 2026-11-12 and 2026-11-13.
