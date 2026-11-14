# Corinto

[![CI](https://github.com/majimtor/corinto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/majimtor/corinto/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

Corinto is a link distribution and analytics platform with a webhook core:
short links that redirect, access events recorded without making the
redirect wait and processed with declared delivery semantics, metrics named
for what they measure, and signed webhooks in both directions.

## Status

In development. The plan and the architecture decisions are written; there
is no application code yet. This section is replaced by the command that
starts the system when the first slice ships.

## Where to look

- [PLAN.md](PLAN.md): what gets built, in which order, with which stack and
  why, how long each step takes, and how the result stays reproducible. Its
  section 11 shows the layout of the repository.
- [docs/adr/](docs/adr/README.md): the decisions that are expensive to
  reverse, each with its alternatives and the condition that reopens it.
- [docs/engineering-rules.md](docs/engineering-rules.md): the rules every
  change in this repository follows.

## Working on the repository

After cloning, point Git at the versioned hooks and install
[gitleaks](https://github.com/gitleaks/gitleaks/releases) so that a commit
containing a secret is blocked before it exists:

```sh
git config core.hooksPath .githooks
```

`make verify` runs every check CI runs. It needs Docker and nothing else:
the tools run from images pinned in the `Makefile`. Markdown is checked
with `markdownlint-cli2`; the configuration lives in
`.markdownlint-cli2.yaml`.

## Licence

MIT. See [LICENSE](LICENSE).
