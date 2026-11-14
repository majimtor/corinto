# 0003. Backend in Go

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The backend is a small hot-path service, a JSON API and a consumer. The
maintainer works daily in TypeScript, built the sister project Delfos in
Kotlin with Spring Boot, and has not shipped Go before.

Facts gathered on 2026-11-12 and 2026-11-13:

- Sizing probes on 2026-11-13 with two equivalent throwaway servers (Go
  1.27.4 and Node 26.13.1, one CPU and 256 MB each; other conditions in
  ADR 0020): 14 MB against 70 MB resident with no links loaded; a 10 MB
  image against 354 MB; 257 against 290 bytes per link held in memory. In
  latency there was no winner. With 100,000 links, under k6 2.4.0 at 300
  requests per second for 60 seconds, the 99th percentile ran from 1.3 to
  9.1 ms in seven runs of Go and from 1.3 to 2.2 ms in three of Node.
- Go published four releases in 24 months under its compatibility promise.
  The TypeScript toolchain the maintainer uses (TypeScript, Vitest and
  the Vite it runs on) had eight majors in the same period, and
  TypeScript 7 shipped without a compiler API.
- Go's `regexp` runs in time linear in its input, which matters for the
  third-party patterns of ADR 0009, applied to a header a stranger writes.
- OpenAPI 3.1 tooling in Go is young (ADR 0011).
- In job-posting samples, Go and Node.js appear at similar rates in
  startup and remote listings (14 to 17 against 16 of the 232 posts in the
  November 2026 "Who is hiring" thread), and Go is rare in Spanish
  listings.

Learning Go is a goal of the maintainer. It is recorded as a preference,
not as evidence.

## Decision

Go 1.27, standard library first: `net/http` with its router, `log/slog`,
and `pgx` for PostgreSQL. The same binary renders the panel (ADR 0014).

## Alternatives considered

- TypeScript on Node 26. The language the maintainer uses daily and the
  cheapest option in hours. Weighing the facts above left it level with Go;
  its toolchain moves faster than a project that must be picked up after
  six months can absorb comfortably.
- Kotlin with Spring Boot. It repeats the sister project and adds nothing a
  reader has not already seen there.
- Rust. No measured requirement that Go fails, and a longer learning curve.

## Consequences

- Positive: small footprint, a stable toolchain, bounded regular
  expressions, and bounded channels as the natural form of back-pressure.
- Negative: young contract tooling, which is why S0 includes a bounded
  trial of it; the additional hours estimated in PLAN.md section 18.
- Strongest argument against: the language is new to the maintainer, and
  the extra hours come early, in S0 and S1, before anything redirects,
  where the risk of abandonment is highest.

## Reopen when

S0 lands at the pessimistic end of its estimate, in which case the language
is reviewed before S1 starts; or the contract
trial fails without a reasonable fallback.

## Links

ADR 0009, ADR 0011, ADR 0014, ADR 0016, ADR 0020. PLAN.md sections 10 and
18. Go compatibility promise (go.dev/doc/go1compat), pkg.go.dev/regexp,
TypeScript 7.0 announcement (devblogs.microsoft.com/typescript) and npm
registry release dates, consulted 2026-11-12.
