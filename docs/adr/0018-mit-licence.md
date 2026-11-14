# 0018. MIT licence

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The repository is public from its first commit, and a public repository
without a licence grants nobody the right to use it. The project exists to
be read, run and learned from; it has no revenue and is not a hosted
service. It includes two third-party data files under permissive
licences: the crawler list of ADR 0009 and, as a test fixture, the URL test
data of the web platform tests, under the 3-Clause BSD licence. Among comparable
link projects, Shlink, YOURLS and Kutt are MIT and Dub is AGPL-3.0 with a
commercial directory (licence files consulted 2026-11-12).

## Decision

The MIT licence, in a `LICENSE` file at the root, without per-file headers.
Each third-party file keeps its own notice next to it. No code is taken
from projects whose licence is not compatible with MIT; reading how they
solved a problem is not copying it.

## Alternatives considered

- Apache-2.0. An explicit patent grant and a defined contribution process.
  Longer, with a `NOTICE` file to maintain, for a project with one author
  and no patents.
- AGPL-3.0. Anyone hosting a modified version would have to publish it. It
  protects a business this project does not have, and it keeps people in
  companies from studying or reusing the code.
- No licence until the code is finished. Nothing to decide yet. Everything
  published in the meantime would be all rights reserved, in a repository
  whose history is never rewritten.

## Consequences

- Positive: anyone can run, copy and adapt the code with one condition
  they already know; the same licence as the sister project.
- Negative: nothing obliges a fork to stay open.
- Strongest argument against: a permissive licence lets a hosted service
  be built on this work without giving anything back.

## Reopen when

The project accepts outside contributions that raise patent questions, or
it becomes a hosted service. A change of licence applies to new versions
only.

## Links

ADR 0009. PLAN.md section 11. Licence files of the projects named above,
consulted 2026-11-12.
