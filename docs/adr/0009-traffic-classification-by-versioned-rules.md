# 0009. Traffic classification by versioned rules

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

A large share of the requests to a public short link come from crawlers,
link previewers and scripts. A count that mixes them with people is not a
click count. The classification has to be explainable to someone looking at
a number in the panel, and it can only use what the request carries.

Facts checked on 2026-11-12. The list `monperrus/crawler-user-agents` is
MIT-licensed, held 1,505 patterns that day, 1,460 of them with real
user-agent examples, and tags each entry by kind. Alternatives with
user-agent data carry heavier licences: LGPL-3.0 for
`matomo-org/device-detector`, AGPL-3.0 for `faisalman/ua-parser-js`. MDN
documents `Sec-Purpose: prefetch` as the header browsers send when
prefetching.

In a sizing probe on 2026-11-13 (conditions in ADR 0020), testing one
browser user-agent against every pattern took 0.4 ms in Go on one CPU, and
a cache by exact string made it negligible.

## Decision

Each event gets one class from deterministic rules evaluated in a fixed
order, where the first match wins. There are no scores and no thresholds:
the classification is a table, and what is an estimate is the metric built
on it.

The rules are a versioned copy of the list above plus a short file of the
project's own. Their test cases are the list's real examples and a set of
ordinary browsers that must not be flagged; updating the list is a reviewed
change that runs the cases and raises the version. Every classification
can be traced to the rule that produced it, and history is never
reclassified.

PLAN.md section 5 holds the classes, their order and the definition of the
metric.

## Alternatives considered

- A score or a trained model. It could catch what headers do not show. It
  cannot be explained next to a number, and it needs data this system
  refuses to store.
- An external detection service. It would see every visitor's request.
- A general user-agent parsing library. More detail than the two closed
  enumerations need, under licences heavier than this project's.

## Consequences

- Positive: any count can be traced to a rule; the rule set is data,
  reviewed like code.
- Negative: the error is known and one-sided, towards counting automation
  as people; the list ages.
- Strongest argument against: header rules cannot see a scanner that
  presents itself as a browser, and that traffic exists.

## Reopen when

A kind of traffic is shown to be miscounted systematically and no rule over
the request can express it.

## Links

ADR 0008, ADR 0010, ADR 0020. PLAN.md section 5. Licence files of the
repositories named above; MDN, `Sec-Purpose`; consulted 2026-11-12.
