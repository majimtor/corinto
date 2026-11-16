# Corinto

[![CI](https://github.com/majimtor/corinto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/majimtor/corinto/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

Corinto is a link distribution and analytics platform with a webhook core:
short links that redirect, access events recorded without making the
redirect wait and processed with declared delivery semantics, metrics named
for what they measure, and signed webhooks in both directions.

## Status

In development. The plan and the architecture decisions are written, and
the foundations are being laid: so far, one binary whose `api` command
answers its health endpoints, and the local stack that runs it next to its
database. This section goes away when the first slice ships.

## Where to look

- [PLAN.md](PLAN.md): what gets built, in which order, with which stack and
  why, how long each step takes, and how the result stays reproducible. Its
  section 11 shows the layout of the repository.
- [docs/adr/](docs/adr/README.md): the decisions that are expensive to
  reverse, each with its alternatives and the condition that reopens it.
- [docs/engineering-rules.md](docs/engineering-rules.md): the rules every
  change in this repository follows.
- [docs/delivery-semantics.md](docs/delivery-semantics.md): what happens to
  an access event between the response and the count, what gives way when
  a part fails, and the test that checks each statement.

## Run it

One command starts PostgreSQL 18 and the `api` process, which so far only
answers its health endpoints, on port 8081:

```sh
make up
```

Then `http://127.0.0.1:8081/readyz` answers `ready`. The command needs
Docker, `make` and `curl`. It generates the database password of the stack
in `deploy/.env`, which is never versioned; `.env.example` lists the
variables the binary reads. `make down` stops the stack and deletes its
data.

Without installing anything,
[a codespace](https://codespaces.new/majimtor/corinto) runs the same command
when it opens. Its hours count against the free allowance of whoever opens
it.

## Working on the repository

After cloning, point Git at the versioned hooks and install
[gitleaks](https://github.com/gitleaks/gitleaks/releases) so that a commit
containing a secret is blocked before it exists:

```sh
git config core.hooksPath .githooks
```

`make verify` runs every check CI runs, except code scanning, which runs on
GitHub. It needs Go and Docker: the `go` command downloads the toolchain
that `go.mod` names and builds the analysers at the versions pinned in the
`Makefile`, and the other tools run from images pinned there too.
`make test` runs the unit tests alone and `make lint` the compilation and
the static analysers. Markdown is checked with `markdownlint-cli2`; the
configuration lives in `.markdownlint-cli2.yaml`.

## Licence

MIT. See [LICENSE](LICENSE).
