# 0016. Dependencies and pinned toolchains

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The project must be picked up after six months on a clean machine and still
build, run and reproduce its figures. Every dependency is something that
can go stale in that time.

Facts checked on 2026-11-12 and 2026-11-13. Go supports each release until
two newer ones exist, about a year, under a compatibility promise.
Dependabot covers `gomod`, `npm`, `docker` and `github-actions`, groups
updates, and updates actions pinned by commit SHA together with their
version comment; it does not issue security updates for Docker. GitHub's
hardening guide recommends pinning actions to a full commit SHA. Secret
scanning on a free public repository detects provider-format secrets, not
generic passwords or connection strings. The `gitleaks` binary is MIT and
in maintenance mode; its GitHub Action carries a separate end-user
licence.

## Decision

- The standard library first. A dependency enters with its reason written
  where it is added. The ones planned are few: the PostgreSQL driver of
  ADR 0003, one library for JSON Schema, one for validating against the
  OpenAPI document, a migration runner if the open decision of PLAN.md
  section 21 picks a library, and Testcontainers in tests only.
- The browser tests bring the only dependencies from another ecosystem,
  Playwright and axe, and none of them ships.
- Every version is pinned, one place per ecosystem, and updates arrive
  grouped. The engineering rules say where and how.
- Secrets are scanned over the whole history with the `gitleaks` binary,
  before each commit and in CI, in addition to GitHub's push protection.
- No infrastructure component enters without a measured requirement
  (PLAN.md section 22 lists the ones left out and what would admit each).

## Alternatives considered

- Floating versions with a lockfile only. Less to maintain while active.
  After a pause, the first build pulls whatever is current and fails in
  ways unrelated to the change being made.
- The `gitleaks` GitHub Action. One line of configuration. A licence of
  its own for something the binary does under MIT.

## Consequences

- Positive: a clean machine builds what was last built; updates arrive as a
  few reviewable pull requests.
- Negative: pinned versions age, and base images collect known
  vulnerabilities until rebuilt.
- Strongest argument against: pinning moves the cost of updating from
  every week to the day of return, when it arrives all at once.

## Reopen when

The secret scanner stops receiving fixes, or the quarterly clean-machine
drill takes more than an afternoon of updates.

## Links

ADR 0003, ADR 0014, ADR 0017. PLAN.md sections 13, 20, 21 and 22. Dependabot
options reference and GitHub Actions secure use reference
(docs.github.com), go.dev/doc/devel/release, consulted 2026-11-12 and
2026-11-13.
