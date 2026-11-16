# Security policy

## Reporting a vulnerability

Report it in private, through the "Report a vulnerability" button of the
[Security tab](https://github.com/majimtor/corinto/security/advisories/new)
of this repository. Please do not open a public issue or a pull request
for it.

Say which commit or release you looked at, what you did and what happened.
A request, an input or a failing test is worth more than a description.

## What to expect

While the project is active, a report is answered within seven days, and
that answer says whether the vulnerability is confirmed and when its fix
is due. The README says when the project is paused; no time frame runs
then, and the report is still read.

A fix reaches `main` through a pull request like any other change. The
advisory is published with it and credits whoever reported it, unless they
prefer otherwise. There is no bounty.

## Scope

- The code of this repository, on `main` and, once there is one, in the
  latest release. Older releases get no fixes.
- The maintainer runs no public deployment of Corinto (ADR 0001). A report
  about an installation run by someone else goes to whoever runs it.
- A vulnerability in a dependency is in scope when it can be reached
  through Corinto. `docs/engineering-rules.md` says how dependencies are
  updated and how fast a critical one is fixed.
