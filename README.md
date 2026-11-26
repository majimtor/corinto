# Corinto

[![CI](https://github.com/majimtor/corinto/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/majimtor/corinto/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

Corinto is a link distribution and analytics platform with a webhook core:
short links that redirect, access events recorded without making the
redirect wait and processed with declared delivery semantics, metrics named
for what they measure, and signed webhooks in both directions.

## A tour

The first slice is built: links, their redirect, an event for each access
and the figures the events add up to, all of it through the API. The panel
is the slice in progress: it has a door into the demo workspaces, the list
of their links, the page of each, with the figures of the last seven,
thirty or ninety days of synthetic traffic and three breakdowns of them,
the forms that create, change and delete a link, and the keys of the API,
which a demo workspace issues and revokes there.
The webhooks come after it (PLAN.md section 18), so for now the tour is a
handful of commands.

Without installing anything,
[a codespace](https://codespaces.new/majimtor/corinto) has the system
running about two and a half minutes after it is opened, and `make seed`
there prints an API key for each of its two demo workspaces. Its hours
count against the free allowance of whoever opens it.

On a machine with Docker, `make` and `curl`, the first command below
starts everything and prints the two keys, that once:

```sh
make up
api=http://127.0.0.1:8080
key=cor_...   # one of the two keys that make up printed
```

Create a link. A demo workspace takes destinations under
`https://www.example.com/` and `https://www.example.org/` and no other.
The answer carries the `id` of the link and its `identifier`, which is the
path of the short link:

```sh
curl --silent --request POST $api/v1/links \
  --header "Authorization: Bearer $key" \
  --header 'Content-Type: application/json' \
  --data '{"destination": "https://www.example.com/books/new-arrivals"}'
id=...           # the id of the answer
identifier=...   # its identifier
```

Follow it. `redirect` answers from a copy of the links that it refreshes
every two seconds, so a link made a moment ago is a `404` until then:

```sh
curl --include http://127.0.0.1:8082/$identifier
```

The answer is `302 Found`, with the destination in `Location`. Then read
what was counted for the link, from the start of today to the end of this
hour, in UTC:

```sh
from=$(date -u +%Y-%m-%dT00:00:00Z)
to=$(date -u -d '+1 hour' +%Y-%m-%dT%H:00:00Z 2>/dev/null ||
  date -u -v+1H +%Y-%m-%dT%H:00:00Z)
curl --silent "$api/v1/links/$id/metrics/summary?from=$from&to=$to" \
  --header "Authorization: Bearer $key"
```

Within a few seconds it says `"requests":1` and
`"estimated_human_clicks":0`: the request was counted, and `curl` is not
taken for a person. Followed from a browser, the link adds one to both.
`processed_through` says how far the events are processed, and `synthetic`
is true because the workspace is a demo. What was served, accepted and
processed on each of the last seven days, which has to add up, is one more
request:

```sh
curl --silent $api/v1/ingestion --header "Authorization: Bearer $key"
```

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
- [docs/privacy.md](docs/privacy.md): what a request to a short link
  carries, what is kept of it, where and for how long, and what a
  deployment has to add.
- [docs/known-issues.md](docs/known-issues.md): what is known not to work
  or to work within limits, and the date of each clean-machine drill.

## Run it

One command starts PostgreSQL 18, applies the migrations and starts three
processes: `api`, which serves the management API and the panel on port
8080,
`redirect`, which answers on the host of the short links on port 8082, and
`worker`, which adds what `redirect` serves to the figures of each link.
Their health endpoints are on ports 8081, 8083 and 8084. The first start
also seeds two demo workspaces with their links and ninety days of
synthetic traffic, which takes some seconds to write, and prints an API
key for each, that once:

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
`http://127.0.0.1:8082/spring-reading` is one of them. In a browser,
`http://127.0.0.1:8080/` is the panel, which enters either demo workspace
without a secret. `make seed` deletes the demo workspaces and creates them
again, with new keys and their history written again.

The command needs
Docker, `make` and `curl`. It generates the database password of the stack
in `deploy/.env`, which is never versioned; `.env.example` lists the
variables the binary reads. `make down` stops the stack and deletes its
data.

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
`make bench-replica` measures the memory the copy of the links takes in
`redirect`.

## Licence

MIT. See [LICENSE](LICENSE).
