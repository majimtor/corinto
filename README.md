# Corinto

[![CI](https://github.com/majimtor/corinto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/majimtor/corinto/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

Corinto is a link distribution and analytics platform with a webhook core:
short links that redirect, access events recorded without making the
redirect wait and processed with declared delivery semantics, metrics named
for what they measure, and signed webhooks in both directions.

## Status

In development. The plan and the architecture decisions are written, and
the first slice is being built: so far, one binary whose `api` command
serves the links of a workspace, whose `redirect` command follows them and
keeps an event of each access, whose `worker` command adds those events to
the figures of each link, and whose `operator` command creates workspaces
and issues their keys, and the
local stack that runs them next to their database. This section goes away
when the first slice ships.

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
- [docs/benchmarks.md](docs/benchmarks.md): the objectives of the plan that
  a load run can check, measured on a named machine, with the command that
  reproduces each figure.

## Run it

One command starts PostgreSQL 18, applies the migrations and starts three
processes: `api`, which serves the management API on port 8080,
`redirect`, which answers on the host of the short links on port 8082, and
`worker`, which adds what `redirect` serves to the figures of each link.
Their health endpoints are on ports 8081, 8083 and 8084. The first start
also seeds two demo workspaces with their links, and prints an API key for
each, that once:

```sh
make up
```

Then `http://127.0.0.1:8081/readyz` answers `ready`,
`http://127.0.0.1:8083/statusz` says how old the copy of the links that
`redirect` answers from is, and
`http://127.0.0.1:8080/v1/openapi.yaml` returns the contract of the API,
with the operations it has so far. With a key of the seed in the
`Authorization` header, as `Bearer` and the key,
`http://127.0.0.1:8080/v1/links` lists the links of its workspace, and
`http://127.0.0.1:8082/spring-reading` is one of them. `make seed` deletes
the demo workspaces and creates them again, with new keys.

The command needs
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
`make test` runs the unit tests alone, `make integration` adds the tests
that need a real PostgreSQL, which they start in a container, and
`make lint` runs the compilation and the static analysers.
`make contract` compares the contract of the API with the one of the last
release, and fails on a breaking change that `contract/CHANGELOG.md` does
not announce. Markdown is checked with `markdownlint-cli2`; the
configuration lives in `.markdownlint-cli2.yaml`.

`make bench` runs the load scenario of the plan, ten minutes of it, against
a stack of its own with the limits the plan gives each process, and fails
when an objective is missed. `make bench-smoke` runs it for a few seconds
with wide thresholds, which is what CI and `make verify` do.

## Licence

MIT. See [LICENSE](LICENSE).
