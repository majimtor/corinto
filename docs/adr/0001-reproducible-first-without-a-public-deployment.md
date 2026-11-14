# 0001. Reproducible first, without a public deployment

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Corinto is judged by two proofs: performance figures published with their
method, and delivery semantics demonstrated with provoked failures. The
infrastructure budget is zero, and one maintainer shares a few hours a week
with a sister project.

Facts checked on 2026-11-12. Free container hosting sleeps: Render's free
web services spin down after fifteen minutes and take about a minute to
start, with 750 instance hours a month per workspace; the sister project
Delfos measured wake-ups of 47 to 95 seconds. The only free compute found
that does not sleep is an edge runtime, and its free quotas would shape the
design: on Cloudflare, 100,000 requests a day per account and 10 ms of CPU
per invocation for Workers, 1,000 writes a day for KV, 10,000 operations a
day for Queues, and a KV propagation delay of "up to 60 seconds or more".

## Decision

There is no public deployment in the first stretch. Corinto is demonstrated
by running it: one command brings up the whole system with its seed, a
Codespaces link does the same without installing anything, CI runs the
failure tests on every change, and `docs/benchmarks.md` holds figures that
anyone can reproduce. Screenshots come from the end-to-end test.

A public deployment is a later slice. It starts with whichever comes first:
the integration with Delfos begins, which needs Corinto reachable from
outside, or the analytics of the maintainer's site show that visitors open
the live demo of Delfos.

## Alternatives considered

- Deploy on the edge runtime now. One click for a visitor and figures that
  include a real network. The price is an architecture chosen by quotas: the
  event buffer picked by daily operation limits, a deactivation delay set
  by the provider's store, and a hot path written against one vendor.
- A small paid virtual machine. The architecture would be published as it
  is, for a few euros a month. It breaks the zero budget.
- A free container that sleeps. A redirect that waits a minute on its first
  request is not a redirect.

## Consequences

- Positive: the architecture follows requirements; the evidence is
  reproducible; there is nothing to operate, no account to keep alive and no
  visitor data being processed.
- Negative: there is no live link to click. Behaviour that only the public
  internet exercises (TLS, real crawlers, abuse) is designed and tested but
  not lived, and no published figure includes a real network (ADR 0020).
- Strongest argument against: a service that has never been on the internet
  is a laboratory piece, and real constraints are what make trade-offs
  real.

## Reopen when

Either trigger above occurs, or free container compute that neither sleeps
nor requires a payment card becomes available.

## Links

ADR 0020. PLAN.md sections 14, 15 and 18. Render free instances
(render.com/docs/free), Cloudflare Workers, KV and Queues limits and pricing
(developers.cloudflare.com), all consulted 2026-11-12.
