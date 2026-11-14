# 0010. Aggregation and retention

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The panel answers a few questions per link: how many requests over time,
how many of them are estimated to be people, and where they come from. Raw
events are needed to deduplicate, to reprocess and to test failures, but
not for long. The guide cited in ADR 0008 accepts its statistics aggregated
daily, and a retention of at most twenty-five months.

At the size per event measured in ADR 0004, the design load takes about
7 GB over the life of the raw events.

## Decision

Both raw events and aggregates, with different lifetimes: raw events for
weeks, to deduplicate, reprocess and test failures, and aggregates for
about a year.

The aggregates are a fixed set of rollups: a time series per link and
breakdowns one dimension at a time, never crossed. Campaign parameters and
tags belong to the link, so they group links and are not dimensions of the
event. A value that the requester writes, the referrer host, is capped per
link and day. A scheduled job deletes what is due and shows when it last
ran. Aggregates live in PostgreSQL next to everything else.

The panel shows times in UTC and says so: its pages are rendered by the
server, which does not know the reader's time zone.

PLAN.md section 5 holds the granularity, the dimensions, the cap and the
two periods.

## Alternatives considered

- Raw events only, aggregating on read. No rollup code. Every panel query
  would scan a month of events, and retention would have to be long.
- Aggregates only. The least data. Duplicates could not be detected and a
  bug in the consumer could not be repaired by reprocessing.
- A cube of crossed dimensions. It answers more questions. Its size grows
  with the product of the cardinalities, and it leaves the list the
  authority considers necessary.
- A columnar analytical store. Faster scans at volumes this system does not
  have.

## Consequences

- Positive: panel queries read small tables; storage is bounded and
  predictable; the data kept longest identifies nobody.
- Negative: questions that cross two dimensions cannot be answered; the
  series and the daily breakdowns follow UTC.
- Strongest argument against: a fixed set of rollups decides in advance
  which questions can be asked.

## Reopen when

A panel query over the seed misses its objective (PLAN.md section 15); the
daily deletion takes more than a minute or the table
stops reclaiming space, in which case raw events are partitioned by day; or
an owner needs the series or the breakdowns in a time zone of their own.

## Links

ADR 0004, ADR 0008, ADR 0009, ADR 0014. PLAN.md sections 5, 8 and 15.
