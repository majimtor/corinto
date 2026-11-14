# 0017. GitHub Flow with squash merges and automated releases

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

One maintainer, a public repository on a free personal account, and a
history that is public from the first commit and never rewritten. The
history is part of what a reader judges.

Facts checked on 2026-11-13. Rulesets are available for public
repositories on the free plan: require a pull request, required status
checks, linear history, no deletion, no force push; bypass is granted
explicitly, so a ruleset without a bypass list also binds the owner. Merge
queues exist only for organisations. A pull request opened with the
workflow token leaves its workflow runs waiting for approval; running them
without approval needs a personal or application token. `release-please`
has a `simple` release type driven by a version file and a changelog.
GitHub signs the commit of a squash merge.

## Decision

- Short-lived branches, pull requests and squash merges into `main`.
  Conventional Commits; the pull request title is the commit message.
- A ruleset on `main` with every rule named above and no bypass list, so
  that it binds the maintainer too.
- Commits are signed and everything reaches `main` through a pull request.
  The engineering rules hold the two written exceptions: the unsigned
  commit of the release tool on its own branch, and the documentation
  commits that precede the first CI workflow.
- One version for the whole repository, managed by `release-please` with
  the `simple` type and tags of the form `vX.Y.Z`.
- `release-please` runs with the workflow token. CI on a release pull
  request therefore waits for one click of approval. In exchange, the
  repository holds no long-lived token.

## Alternatives considered

- A personal access token for the release tool, as in the sister project.
  CI runs on release pull requests without a click. The token expires, and
  it would be the only secret the repository keeps.
- Classic branch protection. The same rules; by default they do not apply
  to administrators.
- Merge commits or rebase merges. They keep every intermediate commit.
  The history of `main` would stop reading as one change per line, and
  rebase merges drop signature verification.
- A version of its own for the contract, apart from the repository's. The
  API could be versioned without a release of the binary. The binary
  embeds the contract, so they are released together, and tags would need
  a prefix.

## Consequences

- Positive: `main` reads as a sequence of reviewed changes, each signed;
  releases and the changelog come from the commit messages; no secret to
  rotate.
- Negative: the maintainer follows the same rules as anyone, including for
  a one-line fix; release pull requests need a click.
- Strongest argument against: with one person, a required pull request is
  ceremony. The pull request description is the review, and the checks are
  what the rule protects.

## Reopen when

A second maintainer joins, or release pull requests become frequent enough
for the approval click to cost more than keeping a token.

## Links

ADR 0015, ADR 0016. PLAN.md sections 11 and 13. Rulesets, `GITHUB_TOKEN`
and commit signature documentation (docs.github.com); `release-please`
README; consulted 2026-11-13.
