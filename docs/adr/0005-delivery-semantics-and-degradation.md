# 0005. Delivery semantics and degradation

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Lowest latency, no lost event and no synchronous work cannot all hold. When
something fails, one of them gives way, and which one has to be written
before the code.

## Decision

The redirect always answers; analytics give way.

- The response never waits for the event. The first hop, from `redirect`
  to the inbox table, is at most once: a bounded in-memory buffer, flushed
  in batches, drops what does not fit and counts it.
- From acceptance on, delivery is at least once with a consumer idempotent
  by event identifier, so each accepted event adds to the aggregates
  exactly once. An event that keeps failing goes to a dead-letter table
  with its reason; it is never dropped in silence.
- Loss is measured, not assumed, and shown per day on the panel's status
  page.
- One loss is declared and not counted: what an abruptly crashing
  `redirect` held in memory.

The numbered statements, their parameters, the bound of the uncounted loss
and the table of failures are in PLAN.md section 4 until S1 and in
`docs/delivery-semantics.md` from then on, where each statement names the
test that checks it or says that it is a declared limit.

## Alternatives considered

- Write the event before responding. Nothing is lost while the store
  answers. It ties the latency and the availability of the redirect to the
  event store.
- A local append-only file synced to disk before acknowledging. It survives
  a crash of the process. It adds file rotation, replay and corruption
  handling to the hot path, for a loss that is small while the database
  answers.

## Consequences

- Positive: once started, the redirect depends on nothing but its own
  process; every loss the system can know about is shown.
- Negative: analytics are incomplete by design under failure, and
  eventually consistent always.
- Strongest argument against: analytics that admit loss are less
  trustworthy than analytics that do not. The answer is to count the loss
  and show it next to the numbers.

## Reopen when

Measured loss exceeds 1 % of a day's events, or a requirement appears to
lose nothing when the process crashes; the local file then enters.

## Links

ADR 0002, ADR 0004, ADR 0006. PLAN.md sections 4 and 12.
