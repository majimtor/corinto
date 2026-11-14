# 0006. In-memory replica for link resolution

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Resolving a short identifier must not depend on the database being
available, and a disabled link must stop redirecting within a published
time. An invented identifier must be answered without giving a stranger a
way to load the database.

In a sizing probe on 2026-11-13, a Go process holding one map entry per
link used 34 MB with 100,000 links and 265 MB with a million (conditions in
ADR 0020).

## Decision

The `redirect` process holds a full copy of every link that is not deleted.
It loads the copy at start and polls for changes.

Polling by "changed after my last mark" loses changes: a transaction that
started earlier can commit after a poll that already saw later marks. Three
measures cover it: each poll re-reads an overlap window, changes are
applied idempotently by link identifier and version, and a full
reconciliation runs periodically. The stale-replica failure test holds a
transaction open across a poll and checks that the change arrives.

Every lookup is served from memory, so there is no cache hit rate to
publish; the lag of the replica and the memory it uses are published
instead.

PLAN.md section 3 holds the intervals and what the process does when the
database is down or absent; section 15 holds the deactivation objective.

## Alternatives considered

- A read-through cache with expiry. It has a hit rate and is what a reader
  expects. Every new or invented identifier reaches the database, and
  deactivation takes as long as an entry lives.
- The replica fed by `LISTEN/NOTIFY`. Changes arrive in under a second. It
  needs a permanent connection with its reconnection logic, and polling
  remains necessary as the fallback.
- A shared Redis cache. Another service and another failure mode, with no
  requirement behind it.

## Consequences

- Positive: lookups never touch the database; an outage of the database
  does not stop redirects.
- Negative: memory grows with the number of links; a change normally takes
  up to the poll interval to apply, and one committed late waits for a
  later poll or, at worst, for the reconciliation.
- Strongest argument against: the design does not scale with the number of
  links, and it replaces a familiar metric with one that needs explaining.

## Reopen when

The measured memory of the replica exceeds the limit of the process at the
expected number of links, or deactivation is required in under a second.

## Links

ADR 0002, ADR 0005, ADR 0007, ADR 0020. PLAN.md sections 3 and 15.
