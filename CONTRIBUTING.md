# Contributing

Corinto is a one-person portfolio project built to a plan and a set of
rules; issues and pull requests are welcome within them.

- Read `PLAN.md` for what is built, what is postponed and why, and
  `docs/engineering-rules.md` for the rules every change follows.
- Set up with the README: one command starts the whole system, and
  `make verify` runs every check CI runs but code scanning and the browser
  tests, which `make e2e` runs.
- Open a pull request from a branch named `type/short-description`. The
  template asks five questions, and the answers are the review. Commits
  follow Conventional Commits; the merge is a squash.
- Every gate of CI blocks: the build and the static analysers, the tests,
  the comparison of the contract with the last release, the secret scan,
  code scanning, the Markdown lint, the image, the load smoke and the
  browser tests. A change to the API starts in `contract/openapi.yaml`,
  with an entry in `contract/CHANGELOG.md`.
- Code, comments and documents are in English.
- No secrets and no real personal data in any file, test or seed.
- The code of conduct applies to every thread.
- Anything on the plan's list of omissions (section 22) needs its trigger
  first: open an issue to discuss it before coding.
