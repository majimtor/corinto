# 0002. One image, three processes

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The redirect is the critical path. It must keep answering when the
management API, the event consumer or the database are down, and the
failure tests have to be able to stop each of them separately. The system
is small: one maintainer, one schema, one release.

## Decision

One binary, `corinto`, with one command per role: `redirect` serves short
links, `api` serves the management API and the panel, `worker` consumes
events and runs retention. The binary's remaining commands are operational
(PLAN.md section 11). One container image holds the binary; the compose
file runs the three roles as separate containers.

The three processes never call each other. The only thing they share is
PostgreSQL: `api` writes links, `redirect` reads them into memory and
writes events, `worker` reads events and writes aggregates.

## Alternatives considered

- A single process with the three roles as goroutines. Two containers
  fewer. A crash or a long pause in the API would take the redirect with it,
  and the requirement that the hot path does not depend on the rest could
  not be shown by stopping a container.
- Separate services with their own repositories and calls between them.
  Independent releases that nobody needs, and a network protocol between
  parts that today agree through one schema.

## Consequences

- Positive: independence is demonstrated literally, by stopping a
  container; one build, one version, one image.
- Negative: three containers and a database to start, and three sets of
  logs to read.
- Strongest argument against: three processes are more moving parts than
  the load of this system demands.

## Reopen when

The failure tests can be written just as clearly against one process, or a
part needs a release cadence of its own.

## Links

ADR 0004, ADR 0005, ADR 0006. PLAN.md sections 3, 4, 11 and 12.
