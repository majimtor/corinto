# Corinto development plan

Corinto is a link distribution and analytics platform with a webhook core.
A manager creates short links to a destination and hands them out through
channels; Corinto resolves every access with a redirect, records it without
making the redirect wait, processes the events asynchronously with declared
delivery semantics, and shows metrics that are named for what they measure.
Conversions arrive from other systems through signed, idempotent webhooks.
This plan says what gets built, in which order, with which stack and why,
how long each step takes, and how the result stays reproducible without
attention. Decisions that are expensive to reverse have an architecture
decision record in `docs/adr/`; rules that every change must follow are in
`docs/engineering-rules.md`.

Status of this document: version 1, 2026-11-14, written before the first
line of application code and revised when a slice closes, last on
2026-11-16 for S0. Every fact about a provider or a tool carries the date
it was checked; every estimate carries its assumptions.

## 1. Vision and success criterion

Corinto exists to show, with a system anyone can run and a repository
anyone can read, how one engineer designs a small infrastructure service:
a measured hot path, a replica with invalidation, asynchronous ingestion
with idempotency, retries, back-pressure and explicit degradation, temporal
aggregation, explainable classification of automated traffic, limits on an
endpoint without a session, privacy by design over visitor telemetry, and
secure webhooks in both directions. Its sister project, Delfos, is a domain
application; between the two a reader sees both kinds of work. Corinto is
named after Corinth, the city on the isthmus between two seas that every
route between them had to cross.

Success is observable: a senior engineer who reads the repository for
fifteen minutes and runs the system for ten concludes that the decisions
were deliberate, that the trade-offs are understood, and that the system
works. Two proofs decide it. Performance figures are published with
everything needed to repeat them, and anyone can.
Delivery semantics are written before the code and demonstrated with
provoked failures. Without those two, Corinto is a list of links with a
counter.

What this project is not: ornamental complexity (a broker, a cache server
or an analytical store that solves no named requirement), one more link
shortener with a dashboard, estimates presented as exact counts, or figures
as an end in themselves.

A mechanism enters the plan through one of two doors. Either it
demonstrates a competence a reader will judge, which holds only when the
simplest mechanism fails a product requirement with a measurement or a
scenario; or it prevents a named failure, or would be expensive to add
later. In both cases it states its cost, including the hours needed to be
able to defend it. Everything else is left out with the condition that
would admit it (section 22).

## 2. Domain model

**Workspace.** The root of ownership and isolation. Every link, key,
webhook source and destination belongs to exactly one, and no query crosses
workspaces. It has a name, a destination policy (an allowlist, or none),
permission to choose aliases, quotas and a state (`active` or `blocked`).

**Link.** A stable opaque identifier, a redirect domain, a short
identifier, a destination, optional campaign parameters, a title, tags, an
optional expiry and a state.

- The short identifier is generated: 8 characters from the alphabet
  `23456789bcdfghjkmnpqrstvwxz`, chosen with a cryptographic generator and
  without modulo bias. The 27 symbols have no vowels, so no words appear;
  no `0`, `1` or `l`; and no upper case. The space is 27^8, about
  2.8 × 10^11: with 10,000 links, a blind guess succeeds with probability
  3.5 × 10^-8.
- A workspace with permission may choose an alias instead: 3 to 40
  characters of `a-z`, `0-9` and hyphen, no hyphen at the ends, outside a
  reserved list. Demo workspaces do not have the permission.
- The pair of domain and short identifier is unique forever, deleted links
  included. A database constraint enforces it; generation retries a bounded
  number of times on conflict. An identifier is never reissued, because a
  link handed out lives in documents nobody controls.
- The destination is validated on the server at creation and on every
  change, stored in normalised form, and served exactly as stored: scheme
  `http` or `https`; no credentials; a domain name as host, not an IP
  literal, `localhost` or a single-label name; not the redirect domain
  itself; at most 2,048 characters; and, when the workspace has an
  allowlist, a match on scheme, host and path prefix. Go's parser follows
  RFC 3986 and browsers follow the WHATWG URL Standard, so validation also
  rejects every input on which the two can disagree (backslashes, control
  characters, whitespace, hosts that are neither ASCII nor punycode), and a
  test runs the web platform's URL test data to check that the host the
  server sees is the host a browser would open. Corinto never fetches the
  destination.
- The campaign parameters (`utm_source`, `utm_medium`, `utm_campaign`,
  `utm_term`, `utm_content`) are attributes of the link. The resolved
  destination, with them appended, is computed on write. One channel is one
  link; links are grouped by campaign or tag.
- States: `active` and `disabled`, chosen by the owner and reversible;
  `blocked`, set by the operator; `deleted`, terminal, which erases the
  destination, attributes and analytics and leaves a tombstone holding the
  identifier. Expiry is not a stored state: it is derived from `expires_at`
  on every request. Every change increments the link's version.

**Redirect domain.** One in the first stretch. A link stores it from the
start so that customer domains are an addition, not a migration.

**Principals.** An API key, a panel session, a webhook source and the
operator (sections 6 and 9). There are no personal accounts in the first
stretch.

**Access events, conversions and aggregates** are facts, not editable
resources: they are inserted, aggregated and deleted by retention
(sections 4 to 6).

Identifiers exposed by the API are opaque strings; the contract does not
promise their format.

## 3. The redirect

The redirect is served by its own process, `redirect`, from an in-memory
replica of every link that is not deleted (ADR 0002, ADR 0006). Once
started, it depends on nothing else to answer.

What it answers is in `contract/redirect.md`, the only source of it. An
active link answers `302` with its resolved destination. A link that does
not redirect and one that does not exist answer one and the same `404`,
which does not say whether the link ever existed. No answer can be stored
by a cache, and the query string of the request plays no part. Besides
links, the host serves its root, which leads to the project's page, and
`/.well-known/security.txt`. The reasons for 302 over 307, 301 and 308, and
for one 404 over 410, are in ADR 0007.

**Replica and invalidation.** The process loads all links at start and asks
for changes every two seconds, with a thirty-second overlap window,
idempotent application by identifier and version, and a full reconciliation
every five minutes. Disabling, blocking, deleting or retargeting a link
takes effect within a published time, measured by a test that disables a
link and polls until the first 404. Expiry needs no propagation
(section 2).

**When something fails.** With the database down, the process keeps
serving its last known state and exposes its age. If the database is absent
at start it fails instead of serving nothing. There is no cache to miss, so
no hit rate is published; the lag and the memory of the replica are.

**Objectives.** Service time and deactivation time are in section 15.

## 4. Ingestion and delivery semantics

The redirect always answers; when something fails, analytics give way
(ADR 0005). The response never waits for the event. The first hop, from
`redirect` to the inbox table, is at most once, and what it loses is
counted. From acceptance on, delivery is at least once and each accepted
event counts exactly once. An event that keeps failing is dead-lettered,
never dropped in silence. Metrics are eventually consistent, and every
metrics response says up to which instant data has been processed.

`docs/delivery-semantics.md` is the only source of the statements, their
parameters, the table of failures and the declared limits, each with the
test that checks it. The panel shows per day what was served, accepted and
lost, and how the accepted divide into processed, pending and
dead-lettered.

## 5. Metrics and aggregation

The metrics, as the panel and the API name them:

- **Requests** (`requests`): events with outcome `redirected`, of every
  traffic class. Includes crawlers, previews, prefetches and repeated
  clicks.
- **Requests to inactive links** (`inactive_requests`): events with outcome
  `inactive`.
- **Estimated human clicks** (`estimated_human_clicks`): `redirected`
  events classified `presumed_human` under the stated rule version. An
  estimate from request headers: anything that presents itself as a browser
  counts, so the known error is by excess. Not people and not unique
  clicks.
- **Reported conversions** (`reported_conversions`): conversions accepted
  from a source and attributed to the link the source names. Corinto does
  not verify that they happened or that they followed a click.
- **Conversions per 100 estimated clicks**: the ratio of the two. Not a
  conversion rate of people.

**Classification.** Deterministic rules in a fixed order assign one class;
the first match wins (ADR 0009):

1. `prefetch`: the request says it is a prefetch.
2. `automation`: `HEAD`, an empty user-agent, an HTTP library or an
   automated browser.
3. `preview`: a messaging or social previewer.
4. `crawler`: search engines, monitors, scanners, archivers and other
   declared crawlers.
5. `presumed_human`: everything else.

The event stores the rule that matched and the version of the rule set.
History is never reclassified; a change of rules is marked on the series
with its date.

**The access event** carries its identifier, link, workspace, server time
to the second, outcome (`redirected` or `inactive`), method, referrer host,
device class and browser family from closed enumerations whose values are
fixed in S1, the classification, and two operational timestamps, of
acceptance and of processing. It never carries the IP
address in any form, the user-agent string, the full referrer, the query
string, a cookie, a fingerprint, or any identifier of a click or a visitor
(ADR 0008).

**Aggregation.** A time series per link, hour (UTC), outcome and class;
breakdowns
per link and UTC day, one dimension at a time: referrer host, device class
and browser family. The breakdowns count estimated human clicks and no
other request. The referrer host comes from the requester: at most
100 distinct hosts per link and day, the rest under `(other)`, and the
clicks that name no referrer under `(none)`, always
rendered as text. The panel shows times in UTC and says so. Raw events are
kept 30 days and aggregates 13 months; a daily job deletes what is due
and shows when it last ran (ADR 0010). An event is due 30 days after it was
accepted, once it has been processed: one that still waits is not deleted.
A dead-lettered event is due 30 days after it was set aside. The counts of
the reconciliation are kept as long as the aggregates. An analytical store
enters only if a
panel query over the seed misses its objective (section 15).

## 6. Webhooks

Both directions follow the Standard Webhooks specification, version 1.0.0,
with its three headers and its HMAC-SHA256 signature (ADR 0013).

**Inbound conversions, from S3.** A source belongs to a workspace and
posts to its own endpoint, authenticated only by its signature. Its secret
is 32 random bytes generated by Corinto, shown once and stored encrypted
with AES-GCM under a key from the environment; during a rotation two
secrets are valid, for at most 24 hours. The signature is verified in
constant time over the body as received; a timestamp more than five minutes
off is rejected; the pair of source and `webhook-id` is unique for 30 days.

```json
{
  "type": "conversion.reported.v1",
  "timestamp": "2026-11-12T10:00:00Z",
  "data": { "link_id": "…", "kind": "signup",
            "occurred_at": "2026-11-12T09:58:41Z", "count": 1 }
}
```

The schema admits no additional property, so there is no field where
personal data could travel. `kind` is 1 to 40 characters of `a-z`, `0-9`
and underscore; `count` is 1 to 1,000; `occurred_at` is neither in the
future nor older than 30 days.

| Situation | Response |
| --- | --- |
| Accepted | `202` |
| Duplicate | `200`, not processed again |
| Body over the limit of section 9 | `413` |
| Unknown or disabled source | `404` |
| Missing or malformed headers | `400 webhook_headers_invalid` |
| Invalid signature | `401 signature_invalid` |
| Timestamp out of tolerance | `401 timestamp_out_of_tolerance` |
| Payload fails the schema, or unknown type | `422 payload_invalid` |
| Too many requests | `429` with `Retry-After` |

Every reception is logged with its outcome and reason, never with a
rejected body or a signature, and the panel lists the latest per source.
Conversions are attributed to the link, per day and `kind`.

**Outbound notifications, from S4.** One secret per destination, the
same identifier across retries, success only on `2xx`, no redirects
followed, `410` disables the destination, exponential back-off with jitter,
dead letters, manual retry, a circuit breaker per destination and a
delivery log. Destinations are `https`, and the address connected to is
checked against non-public ranges. In demo workspaces the only destination
allowed is a built-in test inbox. The event types and the retry values are
closed when the slice starts (section 21).

## 7. API contract

Three contracts, each with one source (ADR 0011).

1. **Management API.** JSON over HTTP under `/v1`, defined by the
   hand-written OpenAPI 3.1 document `contract/openapi.yaml`, which the
   binary embeds and serves. The server validates requests against it, and
   no code is generated from it. The panel is rendered by the server and is
   not one of its clients (ADR 0014).
2. **Redirect.** `contract/redirect.md`: the table of what the host of
   the short links answers, asserted by tests from outside.
3. **Webhook events.** One JSON Schema per type and version under
   `contract/schemas/webhooks/`, referenced from the OpenAPI document.

- **Versioning.** A major prefix in the path. Within `/v1`, only additive
  changes; clients must tolerate fields and enumeration values they do not
  know. Until the first external client, a breaking change is allowed with
  an entry in `contract/CHANGELOG.md`; after it, only under `/v2` with
  ninety days of overlap.
- **Errors.** RFC 9457 Problem Details with a stable `code` from an
  enumeration in the contract and field-level `errors[]`. Text is for
  developers, in English; clients translate from the code.
- **Pagination.** An opaque cursor and a bounded `limit`, in a stable
  order.
- **Authentication.** Management operations have one scheme, the API key,
  and each declares the scope it requires. Webhook reception is
  authenticated by its signature (section 6), and health by nothing.
- **Metrics queries** take a bounded interval, a granularity and one
  dimension; responses carry the rule versions of the period, the instant
  up to which data is processed and a flag for synthetic data.
- **Contract tests.** Integration tests validate every response against
  the document and fail on an undocumented status code; `oasdiff breaking`
  fails on a breaking change against the last released contract that the
  contract's changelog does not announce.

Operations by slice. S1: links (create, read, list, update, disable,
enable, delete), metrics (summary, series, breakdown), ingestion status,
health and the key of the request. S2 adds none: sessions and keys are
handled in the panel's pages. S3: sources, reception and receipts. S4:
destinations, deliveries and retry.

The contract stays on OpenAPI 3.1, and both validations use
`getkin/kin-openapi`. Section 21 records the trial that decided it.

## 8. Data and migrations

PostgreSQL 18 holds everything (ADR 0004). The initial schema, by owner
package:

- `link`: `workspaces`, `links` (section 2).
- `auth`: `api_keys`, `sessions`.
- `ingest`: `event_inbox`, `event_dead_letters`, `pipeline_counters`.
- `rollup`: `link_hourly`, `link_daily_dimension`, `retention_runs`.
- `webhook`: `webhook_sources`, `webhook_receipts`, `conversion_daily`;
  from S4, `webhook_destinations` and `webhook_deliveries`.

Migrations are SQL files applied by `corinto migrate`, which runs them with
`goose` as a library, under the engineering rules on schema and migrations.

**Seed.** One deterministic seed for local runs, Codespaces and end-to-end
tests: a fixed generator seed for the data and dates relative to the day it
runs. Two
demo workspaces, so that isolation can be checked with a key of one against
resources of the other; 12 to 20 links in three campaigns; 90 days of
synthetic history with weekly seasonality and a launch peak; traffic of all
five classes; requests to an expired link; conversions from a test source;
one dead-lettered event and one rejected reception, so the reconciliation
is not a row of zeros. The synthetic history goes through the same
classifier and the same consumer as real traffic, and the panel labels it
as synthetic. Keys and signing secrets do not come from that generator and
are never versioned (engineering rules); `corinto seed` shows them once and
recreates everything.

## 9. Authentication and authorization

- **API key**: `cor_<id>_<secret>` with 32 random bytes; only the SHA-256
  of the secret is stored; shown once; name, scopes, optional expiry and
  last use to the day; sent only in the `Authorization` header. Revocation
  is immediate, since the API reads the key on each request.
- **Scopes**: `links:read`, `links:write`, `metrics:read`, `webhooks:read`,
  `webhooks:write`. In a demo workspace keys are created and revoked in the
  panel, and the first ones come from the seed. In any other workspace the
  operator issues and revokes them, since nobody can enter its panel until
  accounts exist.
- **Session**, from S2: an opaque token of at least 128 bits, stored
  hashed and revocable, with an absolute lifetime of 12 hours and an idle
  timeout of 30 minutes, in a host-only `HttpOnly` cookie with
  `SameSite=Lax`. The cookie has two forms, chosen by configuration:
  `__Host-session` with `Secure`, the default, for every installation a
  browser reaches over HTTPS, the Codespaces link included; and, in the
  configuration of the local run alone, a cookie without the prefix or
  `Secure`, issued only to requests whose `Host` is a loopback name. Only
  the panel's pages accept the cookie; the API never reads it. Nothing
  changes state on a `GET`, and every non-safe request to the panel passes
  the standard library's cross-origin check. ADR 0012 gives the reasons.
- **Demo entry**: opens a session on a demo workspace without a secret.
  Personal accounts enter with self-service sign-up (section 22).
- **Operator**: the `operator` command of the binary, against the
  database, with six actions: create a workspace, issue a key, revoke a
  key, block a link, block a workspace and send dead-lettered events back
  to the inbox (`docs/delivery-semantics.md`). Blocking a workspace revokes
  its sessions and keys, and its links answer as blocked. There is no
  administrative HTTP endpoint.
- **One point of authorization**: the principal is resolved to its
  workspace and scopes, and a resource is loaded by workspace and
  identifier. Another workspace's resource answers `404`; a missing scope
  answers `403`. A test walks the matrix of operations against foreign
  resources.

Initial limits:

| Limit | Initial value |
| --- | --- |
| Redirect, per client address | 20 per second sustained, burst of 40; then `429` |
| API, per key | 120 requests a minute; 60 writes a minute |
| Demo entry, per address | 10 sessions a minute |
| Failed key authentications, per client address | 10 a minute; then `429` |
| Webhook reception, per source | 600 a minute |
| Request body | 16 kB for the API; 64 kB for webhooks |
| Per workspace | 1,000 links (100 in demo), 10 keys, 5 sources, 5 destinations |

The client address is the socket's; a forwarding header is honoured only
from proxies named in the configuration.

## 10. Stack

| Piece | Choice | Record | Best alternative left out |
| --- | --- | --- | --- |
| Backend | Go 1.27, standard library first | ADR 0003 | TypeScript on Node 26 |
| Processes | One image, three processes | ADR 0002 | A single process |
| Store and queue | PostgreSQL 18 | ADR 0004 | Redis Streams |
| Resolution | Full in-memory replica | ADR 0006 | Read-through cache |
| Contract | Hand-written OpenAPI 3.1 | ADR 0011 | Generated from code |
| Panel | HTML rendered by the server with Go templates | ADR 0014 | React 19 with Vite |
| Published figures | A named machine, k6 with an open model | ADR 0020 | Figures from the CI runner |

Each record holds the strongest argument against its choice and the
condition that reopens it.

**Breadth against depth.** Go adds a third language next to the sister
project's Kotlin and TypeScript; TypeScript on the server would have added
depth in what the maintainer uses daily. ADR 0003 weighs the two. The panel
follows from the choice (ADR 0014): the sister project already shows a
client-side application, so this one is rendered by the server and
everything that ships is written in one language. A language is not an
achievement; what it is used for is.

## 11. Repository

One public repository with the Go module at its root (ADR 0015).

```text
go.mod  go.sum  Makefile  README.md  PLAN.md  LICENSE  CHANGELOG.md  SECURITY.md
cmd/corinto/      the binary: redirect, api, worker, migrate, seed, operator
internal/         one package per responsibility
  link/  resolve/  redirect/  classify/  ingest/  rollup/
  webhook/  auth/  api/  panel/  postgres/  seed/  health/  limits/
contract/         openapi.yaml, schemas/webhooks/, redirect.md, CHANGELOG.md,
                  embed.go
deploy/           compose.yaml and the Dockerfile
bench/            load scenarios
e2e/              browser tests, the only directory with Node tooling
docs/             adr/, engineering-rules.md, delivery-semantics.md,
                  benchmarks.md, accessibility.md, privacy.md, known-issues.md
.github/          workflows, dependabot.yml, pull request template
.devcontainer/    the environment the Codespaces link opens
```

The listing shows the shape, not every file. Names are in English, carry
no technology name unless the technology is the content, and are
unambiguous in a URL. The panel's templates, stylesheet and catalogues live
in `internal/panel` and are embedded in the binary; its charts are a
package of their own inside it. The image is `ghcr.io/majimtor/corinto`; no
library is published to a package registry.

In the first minute a reader sees `cmd/` and `internal/`, which anyone who
knows Go recognises, five directories with obvious names, and a README with
the command that starts everything. The `Makefile` is the single entry
point: `make up` starts the system, `make test` runs the unit tests,
`make verify` runs every check CI runs except code scanning, which runs on
GitHub, and `make bench` reproduces the published figures.

Flow and history are decided in ADR 0017 and the licence in ADR 0018.

## 12. Tests

Each layer enters in the slice the table names. CI minutes are estimates
until that slice measures them.

| Layer | Domain failure it catches | Slice | CI |
| --- | --- | --- | --- |
| Unit tests with table cases and native fuzzing | A hostile destination that passes validation; a crawler counted as human; a signature accepted with an altered body; an identifier outside the alphabet; a full buffer that drops without counting | S1; signatures in S3 | 1 min |
| Race detector over the buffer and the replica | An event lost or duplicated by concurrency in memory | S1 | 2 min |
| Integration against a real PostgreSQL in a container | Two links with one identifier; double counting with several consumers; the deadlock of an unordered aggregate upsert; a retried batch that duplicates; an event that exhausts its attempts and is not dead-lettered | S1 | 3 min |
| Contract | A response that departs from the document; a foreign resource that answers anything but 404; an unannounced breaking change; a redirect that departs from its table | S1 | 2 min |
| Failure, four tests | A consumer stopped mid-batch; the database absent; a duplicated event; a stale replica, where a change committed late must still arrive | S1 | 4 min |
| Load | An objective that stops being met, in the full scenario of `make bench`; in CI, a regression of an order of magnitude | S1 | 2 min |
| Panel pages through their handlers | A page that fails to render; a page sent without its security policy; the plain form of the session cookie issued to a host name that is not loopback; a form accepted from another origin; a page that shows another workspace's resource; a session cookie accepted by the API; a catalogue with a missing key or placeholder | S2 | 1 min |
| End to end, with Playwright and axe, in Spanish; the demo entry also in Firefox and WebKit | The product does not work as a whole: enter, create a link, follow it, see it counted; accessibility regressions; a session cookie one browser engine discards | S2 | 5 min |

Left out, with their triggers in section 22: mutation testing and mobile
end-to-end tests. Coverage is reported, not pursued as a target.

## 13. CI and containers

GitHub Actions on standard hosted runners, which are free for public
repositories (checked 2026-11-13). The gates:

| Gate | What it protects |
| --- | --- |
| Build, `go vet`, `staticcheck`, `golangci-lint` | Code that does not compile or that departs from the language's idiom |
| Unit tests with the race detector | Section 12 |
| Integration, contract, failure and panel tests | Section 12 |
| Load smoke with wide thresholds | Section 12 |
| End-to-end tests with axe | Section 12 |
| Secret scan over the whole history | A secret in a commit |
| Code scanning for Go | Known vulnerable patterns |
| Image build | An image that no longer builds |
| Markdown lint | A document that does not render as it reads |

The ruleset of `main` (ADR 0017) requires these checks. CI holds no secret:
publishing the image uses the workflow's own token, and so does the release
tool.

The image of ADR 0002 is published for each release, built for amd64 and
arm64, so that a reader can start the system without compiling or
emulating.

## 14. Running, demonstrating and deploying

**Running.** `make up` starts PostgreSQL, applies migrations, seeds, and
starts `redirect`, `api` and `worker`, with no external service and no
account anywhere. There is no second mode with other components.

**In one click.** A Codespaces link opens the repository in a browser
environment that already has Docker and runs the same command, with the
session cookie in its HTTPS form (section 9), since the browser reaches the
panel over HTTPS. With the repository itself on the default two-core
machine, it took 164 seconds from the start of the environment to the stack
reporting ready: 63 to create the environment and 101 for `make up`, which
downloads PostgreSQL and builds the image from source (one run, 2026-11-16,
with the stack of S0: the database and the `api` process). Above three
minutes, the README names the local run first. This is 16 seconds under,
with the smallest stack the project will have, so S1 measures it again
before the link takes the first place in the README. The free allowance is
charged to whoever opens the environment. The forwarded port is a proxy
this project does not configure, and how it treats request headers is not
documented, so S2 checks that the demo entry works through it.

**What a reader meets.** Demo workspaces with synthetic data, labelled as
such; a short link that redirects; the panel with its series and its
reconciliation; and the figures document. Demo workspaces create links only
to destinations on an allowlist.

**Public deployment** is a later slice, started by the first of the two
triggers of ADR 0001. The host names are chosen, on domains the maintainer
already uses: `go.manuelalejandro.dev` for redirects, on a registrable
domain separate from the maintainer's site so that their reputations do not
mix; `corinto.manuelalejandro.com` for the panel and
`api.corinto.manuelalejandro.com` for the API. The last two names reach the
one `api` process, which tells pages from operations by path. The form of
the deployment is an open decision (section 21). That slice also brings the
privacy notice, the legal basis, the record of processing, the deployed
secrets, a proxy that passes `Host` unchanged (ADR 0012) and the table of
what dies after six months.

## 15. Published figures

**Objectives.** They are requirements, written before measuring:

- Design volume: one million events a day, 12 per second on average.
- Burst: 300 redirects per second for ten minutes, with no counted loss and
  ingestion lag under 30 seconds at the 95th percentile.
- Service time of the redirect, without network: under 10 ms at the 99th
  percentile at the burst rate, with `redirect` limited to one CPU and
  256 MB and PostgreSQL to one CPU and 1 GB.
- Deactivation: five seconds at the 99th percentile.
- Storage: at the design volume, raw events over their whole life
  (section 5) take under 10 GB.
- Panel queries over the seed: under 200 ms at the 95th percentile.
- Break test: raise the rate until the first objective fails, and publish
  where and why.

When a measurement misses an objective, the design changes or the
objective changes with a note; neither happens silently.

**Method.** ADR 0020 decides how figures are measured, and the engineering
rules say what every published figure carries. Figures live in
`docs/benchmarks.md`, and `make bench` reproduces them.

**Sizing probes.** The measurements taken while planning sized decisions
and are not published figures. Each is in the context of the record it
supports: the runtime (ADR 0003), the queue (ADR 0004), the replica
(ADR 0006), classification (ADR 0009) and the panel (ADR 0014). ADR 0020
names the machine they ran on.

## 16. Accessibility and internationalisation

**Accessibility.** The panel targets WCAG 2.2 level AA. It is a quality
decision: the European Accessibility Act and its Spanish transposition
cover specific services offered to consumers and exempt microenterprises
providing services, and the Spanish decree on websites applies to the
public sector (texts read 2026-11-13; a technical reading, not legal
advice).

- Charts are static SVG drawn by the server and follow the W3C guidance on
  complex images: each comes with the text alternatives the engineering
  rules require, visible to everyone. They have no tooltips of their own
  and nothing to drag.
- The automated check is axe, run in a browser by the end-to-end tests. By
  its own statement it finds on average 57 % of issues. Its only WCAG 2.2
  rule, target size, is disabled by default and is enabled here.
- A manual review in S2 covers what the tool does not: series not
  distinguished by colour alone and graphical objects at 3:1 contrast
  (1.4.1, 1.4.11); the labels inside charts, whose contrast the tool
  leaves undetermined (1.4.3); full keyboard use with no trap (2.1.1,
  2.1.2); a page title that names the view and its filter after each load
  (2.4.2); focus not obscured and 24-pixel targets, both new in 2.2
  (2.4.11, 2.5.8); use at 320 pixels wide (1.4.10); declared language
  following the selected one (3.1.1, 3.1.2); and form errors described in
  text next to their field (3.3.1).
- `docs/accessibility.md` records the date of the manual review and what
  does not conform.

**Internationalisation** (ADR 0019). Spanish is the source and default
language and English the second; a missing key falls back to Spanish. Text
comes from two catalogues, one file per language, embedded in the binary,
and a small helper written with the standard library formats plurals,
numbers and dates. The language is the first segment of the path, and the
declared language of the page follows it. The cost is real: every string is
two artefacts, an estimated 150 to 250 strings.

## 17. Security, privacy and observability

**Security.** The baseline is OWASP ASVS 5.0.0 level 1. Of its 70
requirements, 38 apply now, 4 apply on deployment (TLS and HSTS) and 28 do
not apply, because there are no passwords, OAuth, self-contained tokens,
file uploads, XML, WebSockets, rich-text input, operating-system commands
or cross-origin resource sharing.

| Requirements | Control | Checked by |
| --- | --- | --- |
| V1.2.1 to V1.2.4, V1.3.2 | Outside text reaches a page only through templates that escape by context; destinations are built with a URL parser and limited to `http` and `https`; queries are parameterised; no dynamic code execution | Unit tests with hostile input; lint |
| V2.1.1 to V2.3.1 | Validation rules written in the contract; positive lists; validation on the server; signatures verified before processing | Contract and unit tests |
| V3.2.1, V3.2.2, V4.1.1 | Content type with character set on every response with a body, and `nosniff` | A test over every operation and every page |
| V3.3.1, V3.5.1, V3.5.3 | The session cookie, the cross-origin check and the rule on `GET` of section 9 | Panel tests with requests from another origin |
| V6.1.1, V6.3.1, V6.3.2, V6.4.2 | The limits of section 9 on failed key authentications and on the demo entry; no default accounts; operator without an HTTP endpoint | A test of the limit; review |
| V7.2.1 to V7.4.2 | Opaque sessions verified on the server; a new token on each entry; invalidation on the server at logout and when a workspace is blocked | Integration tests |
| V8.1.1 to V8.3.1 | Scopes per operation; every resource loaded by workspace and identifier, on the server | The authorization matrix |
| V11.3.1, V11.3.2, V11.4.1 | Signing secrets encrypted at rest and key secrets stored hashed, with the algorithms of sections 6 and 9 | Round-trip test; review |
| V13.4.1, V14.2.1, V14.3.1 | An image without source-control metadata; keys and tokens only in headers; pages behind a session are sent with `no-store`, the panel keeps nothing in client storage, and logout sends `Clear-Site-Data` | Image listing; contract, panel and end-to-end tests |
| V15.1.1 to V15.3.1 | A written time frame for dependency fixes; alerts enabled; responses with explicit fields | Review; contract tests |

Five level 2 requirements are adopted. Four because the domain demands
them: webhook destinations checked against request forgery and redirects
not followed (V1.3.6, V15.3.2), limits against automation (V2.4.1), and the
allowlist of destinations in demo workspaces (V3.7.2). The fifth because a
panel without scripts makes it nearly free: the content security policy of
the engineering rules (V3.4.3). A link shortener open to any destination
departs from V3.7.2 by definition; the day the allowlist is lifted, the
departure is documented with its mitigations.

**Privacy.** Corinto writes nothing to and reads nothing from the
visitor's device: no cookies, no local storage, no fingerprint.

| Datum | Where it appears | What happens to it |
| --- | --- | --- |
| IP address | The connection | Used as the key of a rate-limit counter through a keyed hash, in memory, for at most 60 seconds. Never stored or logged |
| User-agent | A request header | Classified inside the request into two closed enumerations and a traffic class. The string is never persisted or logged; a bounded cache in the process's memory keeps strings already seen with their class |
| Referrer | A request header | Only its host is kept |
| Query string of the short link | The request | Ignored |
| Time of access | The server clock | Kept to the second while the raw event lives (section 5), then only in hourly and daily aggregates |

The breakdowns stay within what the Spanish data protection authority
lists as audience measurement exempt from consent (ADR 0008), the hourly
series carries only counts per traffic class, and everything is kept well
under the authority's retention limit (ADR 0010). The transient handling of
the IP address and the user-agent is still processing of personal data.
While there is no public deployment, no real visitor's data is processed;
what a deployment adds is listed in section 14. `docs/privacy.md` holds
this inventory for whoever deploys the software. This is a technical
reading of the sources, not legal advice.

**Observability.** Measured: the redirect's service time, in the process
(ADR 0020); the age of the replica; served, accepted, lost, pending and
dead-lettered events; ingestion lag; failed batches; webhook receptions by
outcome; failed deliveries from S4; the date of the last retention run. The
reconciliation lives in PostgreSQL and the panel shows it on a status page:
it is the visible proof. Logs are structured JSON; the engineering rules
say what they never carry and the test that checks it. Each process has a
liveness endpoint and a readiness endpoint, on a port of their own, apart
from the one that serves traffic. Distributed tracing and a
metrics server are left out (section 22).

On returning after six months there is no running system to look at, so
the first thing checked is whether `make verify` passes on a clean
machine. In a running system it would be the lost and dead-lettered
counts.

## 18. Roadmap by vertical slices

Estimates are real hours: optimistic, probable, pessimistic. Weeks are
given for ten hours a week and for five, since the weekly hours are shared
with Delfos and work alternates by whole slices rather than splitting
weeks. Each slice records its real hours when it closes.

Revision of 2026-11-16, at the close of S0, the calibration slice. The
first estimates assumed that this is the maintainer's first project in Go
and that 30 to 70 of their hours were learning. S0 took about 8 real hours
against a probable 20. That ratio, 0.4, replaces the learning assumption:
every later estimate below is the first one multiplied by it, once. It is
one measurement, taken on the slice with the least design in it, so the
hours each slice records are what corrects it from here. The hours of S2
remain an unmeasured estimate until the check that opens that slice.

| Slice | Hours | Weeks at 10 h | Weeks at 5 h |
| --- | --- | --- | --- |
| S0 Foundations, closed | 8, against an estimate of 14 / 20 / 30 | 1 | 2 |
| S1 Redirect and count | 52 / 74 / 110 | 5 / 7 / 11 | 10 / 15 / 22 |
| S2 Panel and metrics | 27 / 38 / 58 | 3 / 4 / 6 | 5 / 8 / 12 |
| S3 Inbound webhooks and launch | 14 / 22 / 34 | 1 / 2 / 3 | 3 / 4 / 7 |
| **Up to launch** | **101 / 142 / 210** | **10 / 14 / 21** | **20 / 28 / 42** |
| S4 Outbound webhooks | 18 / 25 / 38 | 2 / 3 / 4 | 4 / 5 / 8 |
| S5 Approximate unique clicks | 5 / 8 / 14 | 1 / 1 / 1 | 1 / 2 / 3 |
| **Whole vision** | **124 / 175 / 262** | **12 / 18 / 26** | **25 / 35 / 52** |
| Public deployment, by trigger | 10 / 16 / 28 | 1 / 2 / 3 | 2 / 3 / 6 |

S0 to S5 are the first stretch. The slices by trigger start when their
trigger fires.

**S0 Foundations.** Licence, a first README, this plan, the records and the
rules; the toolchain pinned; CI with build, static analysis, unit tests,
secret scan and code scanning; `make up` starting PostgreSQL and a server
that answers its health endpoints; the contract skeleton; the trial of
section 21, with its result written down; the Codespaces start measured.
Done when a clean clone passes `make verify`, the trial has an outcome, and
the estimates above have been rescaled with the real hours. If the probable
doubles, the Codespaces measurement moves to S1. Closed on 2026-11-16 in
about 8 real hours, with the Codespaces start measured.

**S1 Redirect and count.** `docs/delivery-semantics.md` first. Then the
link API with keys; `redirect` with its replica, limits and
classification; the buffer and the inbox; `worker` with aggregates and
retention; the metrics and ingestion-status operations; the `operator`
command; the seed with links and keys; the failure tests; the load
scenario with the redirect and ingestion objectives as thresholds; the
values this plan leaves to S1; `docs/benchmarks.md` with the
first published figures. Done when someone who clones the repository starts
the system with one command, creates a link with a seeded key, follows it,
and sees it counted through the API within the lag objective; when
`make verify` runs the failure tests; and when `make bench` reproduces the
figures. If the probable doubles, breakdowns other than referrer and the
break test move to S2; redirect, ingestion, semantics and one published
figure stay.

**S2 Panel and metrics.** First, the bounded check of ADR 0014; if it
fails, this slice is estimated again. Then sessions and the demo entry; the
panel with links, series, breakdowns, keys and the status page; the
synthetic history of the seed; both catalogues; the manual accessibility
review; the end-to-end path and the screenshots it generates. Done when the
end-to-end path passes in Spanish with axe clean, the review is recorded,
both catalogues verify, panel queries over the seed meet their objective,
and the demo entry works in Chromium, Firefox and WebKit on the local run
and through a Codespaces forwarded port. If the
probable doubles, the browser breakdown moves to S3.

**S3 Inbound webhooks and launch.** Sources, signed reception, the receipt
log, conversions in the aggregates and the panel, and an action that sends
a signed test conversion through the public path; the README complete with
video and screenshots. Done when the signature test vectors, the replay
test and the duplicate test pass, and a conversion sent with a documented
command appears in the panel. If the probable doubles, secret rotation with
overlap moves to S4.

**S4 Outbound webhooks.** The outbound half of section 6, with tests for
request forgery through destinations.

**S5 Approximate unique clicks.** Starts with its own record, after a
privacy analysis of the transient identifier it needs. If the metric is
offered, its method is named next to the number.

**By trigger.** Public deployment (section 14). Smart links with mobile
deep linking: after the core is deployed and measured, and with a demo
application that can be tested without a paid developer programme;
deferred deep linking is a separate investigation, never a promise.
Integration with Delfos: when Delfos publishes a signed "response
completed" event and accepts an opaque channel reference. Starting it fires
the deployment trigger of ADR 0001, because it needs Corinto reachable from
outside. It never blocks participation in a survey and never correlates a
click with a response per person.

## 19. Launch

Launch is observable: the repository, with reproducible figures, a video
and screenshots, is linked from the maintainer's site. Its threshold is the
end of S3: links, the redirect, ingestion with demonstrated semantics, the
panel with honest metrics, and inbound conversion webhooks, with CI green
and `docs/benchmarks.md` published.

A later milestone follows S5: Corinto is presented as a reference project.
Before that, both webhook directions, the decision on approximate unique
clicks with its record, `docs/known-issues.md` up to date and a
clean-machine drill dated within the last quarter must be complete.

## 20. Sustainability

The horizon is six months without touching the repository, then a clean
machine, one command, and the published figures reproduced. With no
deployment there is no instance, database, certificate or domain in use;
nothing dies, and some things go stale.

Maintenance while active is estimated at two hours a month, mostly grouped
dependency updates, with the time frame for critical vulnerabilities that
the engineering rules set; a pause is declared in the README. A quarterly drill
of under an hour clones the repository clean, runs `make verify` and records
the date in `docs/known-issues.md`.

| What goes stale | What happens | What covers it |
| --- | --- | --- |
| The browser test tooling | Playwright publishes often, and each version pins its browsers | Pinned versions, grouped updates, the drill; none of it ships |
| Scheduled analysis | Scheduled workflows stop after 60 days without activity | The first push reactivates them; without changes there is no new code to scan |
| Dependency alerts | They keep arriving | By e-mail to the maintainer |
| Base images pinned by digest | They collect known vulnerabilities | Rebuilt on return |
| The pinned runner label | It will be retired one day | CI fails visibly |
| The published image | Free today, with a month's notice of change | The README says how to build |
| The Codespaces link | Depends on the default image keeping Docker | The README gives the local path |
| The crawler list | New crawlers count as people | The rule version is shown next to the metric |
| The secret scanner | It is in maintenance mode | Replaced when it stops receiving fixes |

How each piece is updated:

| Piece | Touches | After | Way back |
| --- | --- | --- | --- |
| Go | `go.mod`, build image, CI | `make verify` | Revert the pull request |
| Go dependencies | `go.mod`, `go.sum` | CI | Revert |
| PostgreSQL | `deploy/compose.yaml`, test image | Integration and failure tests; queue figures | The previous major; local data is regenerated by the seed |
| Browser tests | `e2e/package.json` and its lockfile | The end-to-end run | Revert |
| Contract tooling | The pinned tool versions | Contract tests | Revert |
| k6 | The pinned tool versions, `bench/` | The figures, when ADR 0020 says they are redone | Revert |
| Actions, runner, base images | `.github/workflows/`, the Dockerfile | CI and `make verify` | Revert |
| Crawler list | The versioned file and the rule version | The classification test cases | Revert; events keep the version they were classified with |

When several coincide: database, Go, Go dependencies, browser tests, CI.

## 21. Open decisions

Each is the maintainer's to take, at the moment named.

| Decision | Options | Recommended | When |
| --- | --- | --- | --- |
| Event types and retry values of outbound webhooks | Types: conversion reported; link expired, disabled or blocked; a threshold reached. Values: attempts, waits and the thresholds of the circuit breaker | Start with the first two types; values set with the slice | Start of S4 |
| Method for approximate unique clicks | A daily-salted identifier kept only in memory; not offering the metric | Decided by the privacy analysis | Start of S5 |
| Form of the public deployment | An edge front in front of an origin that may sleep; a small paid virtual machine | Decided with the limits of ADR 0001 rechecked | When a trigger fires |

Closed in S0, on 2026-11-14:

| Decision | Options | Outcome |
| --- | --- | --- |
| Library that validates against the OpenAPI document in Go. A trial of at most six hours: a 3.1 document with a `webhooks` section, requests validated by the server and responses by the tests | `getkin/kin-openapi`; `pb33f/libopenapi-validator`, both below version 1; or fall back to OpenAPI 3.0, the version the sister project uses | `getkin/kin-openapi`, on OpenAPI 3.1; the fallback was not needed. The trial document had a `webhooks` section, an external reference to a JSON Schema 2020-12 file, type lists, `const` and security requirements with scopes, and was loaded from embedded files; 23 requests and 13 responses had a known verdict. Both libraries loaded it and validated requests and responses. `getkin/kin-openapi` 0.153.0 gave the expected verdict in 35 of the 36 cases: it does not assert `format: uri`. `pb33f/libopenapi-validator` 0.16.3 gave it in 34 by default and in all 36 with format assertions switched on. `oasdiff` 1.37.0 read the same document and reported the breaking changes seeded in it. The first was chosen because it adds 6 modules to the binary where the other adds 13, its JSON Schema engine is an independent library where the other's is a fork of that library, and `oasdiff` reads documents with its parser. Against it: it has to be given the route of each request, and it was the slower of the two in the one pass that was timed. The limit of the trial: its document had three paths, and the real contract will use more constructs |
| Migration runner | `goose` as a library; a runner of the project's own | `goose` as a library, which adds four modules to the binary. The `migrate` command and the dependency arrive in S1 with the first table; S0 has no migration to apply |

## 22. Omissions with triggers

Where a record decides an omission, its "Reopen when" holds the condition.

| Left out | Reopened by |
| --- | --- |
| Public deployment | ADR 0001 |
| Personal accounts, sign-up and passwords; teams, members and roles | ADR 0012 |
| Audit log | Members, or an external client asking who changed something |
| Click identifier and per-click attribution | A need the per-link aggregate cannot meet, with its privacy analysis |
| Country of the access | A platform that supplies it with the request, or a geolocation database with a compatible licence |
| Operating system of the access | A use that needs it, with its privacy analysis (ADR 0008) |
| Minimum threshold before showing conversions | The integration with Delfos, where a conversion is one person's response |
| Forwarding request parameters to the destination | A client that needs one link for several channels |
| Permanent redirects per link | ADR 0007 |
| Customer domains, QR codes, A/B tests, geographic rules | A real use case; none shows what the core does not |
| Destination previews and metadata | A design of its own against request forgery |
| Moderation and a report form | Lifting the allowlist or opening sign-up |
| Asymmetric webhook signatures | ADR 0013 |
| Amount and currency in conversions | A source that reports sales |
| `Idempotency-Key` on creation | An external client that retries creations |
| Conditional updates with `ETag` | Two clients editing the same link |
| A time zone per workspace; partitioning of raw events | ADR 0010 |
| Broker, cache server, analytical store | A measurement missing an objective of section 15 |
| A local file for the first hop | ADR 0005 |
| Metrics server and its client library | ADR 0020 |
| Distributed tracing | A request that crosses more than one process |
| A client-side framework, a chart library, scripts in the panel | ADR 0014 |
| A translation library | ADR 0019 |
| Published SDK | A second external client |
| Mutation testing | A defect the tests should have caught |
| Mobile end-to-end tests | A mobile client |
| Smart links; integration with Delfos | Section 18 |

## 23. Architecture decision records

`docs/adr/` holds twenty records, one per decision that is expensive to
reverse. Its README indexes them and says what a record is.

## 24. Implementation rules

`docs/engineering-rules.md` turns the decisions of this plan into rules
that can be checked in a type, a contract, a test, a build or a review.

## 25. Risks

| Risk | Observable signal | Mitigation |
| --- | --- | --- |
| Hours shared with Delfos | A slice exceeds its pessimistic estimate, or neither project closes a slice in a quarter | Alternate by whole slices; record real hours; the estimates were rescaled after S0 (section 18) |
| Go is new to the maintainer | S0 closed under its optimistic estimate, so the review point of ADR 0003 did not fire. From here: a slice exceeds its pessimistic estimate | The real hours each slice records, against the rescaled estimates of section 18 |
| Young OpenAPI 3.1 tooling in Go | The validation library, below version 1, changes its interface in an update, or stops being maintained | Its version is pinned and an update that breaks it fails the build; the other library of section 21 passed the same trial; ADR 0011 says when the choice is reopened |
| A server-rendered panel in a language new to the maintainer | The check of ADR 0014 fails | Its fallback |
| Infrastructure added for its own sake | A component enters without a measurement next to it | The admission rule of section 1 |
| Evidence that goes stale | Screenshots or figures that no longer match the code | Screenshots come from the end-to-end run; figures are redone as ADR 0020 says |
| Abuse and privacy never exercised | No public traffic reaches the system | Failure and limit tests; stated as a limit; the deployment slice |
| Tooling that goes stale during a pause | The quarterly drill takes more than an afternoon | Pinned versions and few dependencies (ADR 0016) |
| Figures that say less than they seem | A figure quoted without its limits | The engineering rules on published figures |

## 26. Audience

- **This plan and the records**: an engineer deciding whether the reasoning
  holds, in the fifteen minutes of section 1.
- **The history**: the same reader, checking that the work was done in the
  order and for the reasons stated. One change per commit.
- **Tests**: someone who wants to see a claim fail. The failure tests and
  the authorization matrix are written to be read.
- **Figures**: someone who doubts them. Hence the command.
- **The running system**: someone with ten minutes and Docker, or a
  browser.
- **Internal value only**: the engineering rules, `docs/known-issues.md`
  and the drill serve the maintainer returning after a pause.

## 27. Sources

The sources behind each decision are in the Links of its record. Those
behind facts stated only in this plan, consulted between 2026-11-12 and
2026-11-14:

- RFC 9116, for `security.txt`.
- URLs: Go `net/url` documentation; the web-platform-tests URL test data.
- Security: OWASP ASVS 5.0.0 (30 May 2025); OWASP Unvalidated Redirects and
  Forwards Cheat Sheet.
- Accessibility: WCAG 2.2 (W3C Recommendation, 12 December 2024); WAI
  tutorial on complex images; axe-core 4.14.0; Directive (EU) 2019/882;
  Spanish Act 11/2023; Spanish Royal Decree 1112/2018.
- PostgreSQL versioning policy (postgresql.org).
- GitHub: hosted runners, scheduled workflows, Codespaces and Packages
  billing (docs.github.com).
