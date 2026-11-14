# 0020. Published figures and how they are measured

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

One of the two proofs this project rests on is performance figures
published with their method. A latency without a method is an opinion.

Two things were learned while planning, on 2026-11-13, with throwaway
servers on a laptop that was not dedicated to the test. Across repeated
runs, the variation between runs was larger than the difference between
the two runtimes being compared; a conclusion drawn from one run was wrong
and had to be withdrawn. And with only the figure reported by the load
generator, a pause could not be attributed to the server or to the
environment.

The sizing probes cited in ADR 0003, ADR 0004, ADR 0006 and ADR 0009 ran on
that laptop (Intel i5-1038NG7) under Docker Desktop, on 2026-11-12 and
2026-11-13. They sized decisions; their code is not in this repository and
they are not published figures.

The load tool k6 implements an open model, in which requests arrive at a
set rate whether or not earlier ones have finished; its documentation ties
the closed model to coordinated omission. Its default summary reports only
the 90th and 95th percentiles. Checked on 2026-11-13.

## Decision

- Objectives are written before measuring, and those a load run can check
  are thresholds of the scenario: when one fails, the run fails. PLAN.md
  section 15 lists them, with the container limits that are part of the
  figures.
- Load uses k6 with a constant arrival rate. Reported percentiles are the
  50th, 95th, 99th and 99.9th.
- A published figure is a median over several runs, with its spread, on a
  named machine with no other load. The engineering rules fix how many
  runs and what each figure carries.
- The server measures its own service time with a histogram of fixed
  buckets from the standard library. A percentile is reported as the upper
  bound of its bucket, and is labelled so.
- The redirect's rate limiter stays in the measured path. The load
  generator is declared as a trusted proxy and spreads its requests over
  simulated client addresses, each under the limit; the figure says how
  many.
- CI runs a short smoke scenario with wide thresholds. It checks that the
  command works and that nothing regressed by an order of magnitude; it
  publishes nothing.
- Figures are redone when the redirect, ingestion, the major version of
  PostgreSQL or of the load tool, or the base image change.

## Alternatives considered

- Publish from the CI runner. Automatic and always current. A shared
  virtual machine that nobody else can reproduce on.
- A closed model with a fixed number of clients. Simpler to reason about.
  When the server slows, the load slows with it and the slow requests are
  under-counted.
- The Prometheus client for the histogram. A standard format with
  well-understood semantics. Nothing reads it without a metrics server,
  and the percentiles shown in the panel would be computed by this
  project's code anyway.

## Consequences

- Positive: every figure can be repeated and questioned; server time and
  environment noise can be told apart.
- Negative: figures exclude the network and TLS and say so; publishing
  takes several runs on a quiet machine, by hand.
- Strongest argument against: figures from one named laptop say little
  about any other machine. They say how the design behaves under a stated
  limit, which is what they are for.

## Reopen when

The system is deployed publicly, at which point figures from an external
client are added; or more than one `redirect` instance runs and their
histograms need to be aggregated, which is what admits a metrics server.

## Links

ADR 0001, ADR 0003, ADR 0004, ADR 0005, ADR 0006, ADR 0009. PLAN.md sections
12 and 15. k6 documentation on open and closed models and on thresholds
(grafana.com/docs/k6), consulted 2026-11-13.
