# Contract changelog

Changes to the contracts in this directory, newest first. A change that
`oasdiff breaking` reports is marked as breaking: its entry begins with
`**Breaking.**`. `make contract` compares the contract with the one of the
last release, and fails on a breaking change when no entry added since
that release carries the mark. An entry waits under `Unreleased` until a
release, and then moves under its version: `make contract` fails while the
entries of the last release are still under `Unreleased`.

## Unreleased

- The API is every path under `/v1/`, and the contract says so. A path
  outside it used to be answered `404` as Problem Details; the host now
  serves the panel there, as pages for a browser. No operation changes.
- The values of a breakdown by `device` and by `browser` are in the
  contract: the schemas `Device` and `Browser` list them, and the
  description of a value names them. They are the values the API already
  returned.

## 0.1.0 (2026-11-20)

- `info.version` is the version of the release the document belongs to,
  and the release tool writes it. It is `0.0.0` in a document that no
  release holds yet.
- `getIngestionStatus` (`GET /v1/ingestion`), which asks for
  `metrics:read`: for each of the last days of the workspace, what was
  served, accepted, lost, processed, pending and dead-lettered, with how
  far the events are processed and when the retention job last finished.
- The metrics of a link, in three operations that ask for `metrics:read`:
  `getLinkMetricsSummary`, `getLinkMetricsSeries` and
  `getLinkMetricsBreakdown` (`GET /v1/links/{id}/metrics/summary`,
  `/series` and `/breakdown`). Each takes a period, `from` and `to`, and
  answers with how far the figures reach (`processed_through`), whether
  they are of generated traffic (`synthetic`) and the versions of the
  rules of the period (`rule_versions`).
- Three more values of `code` in an entry of `errors`, for a period:
  `not_aligned`, `not_after_from` and `period_too_long`.
- The first two operations, neither of which needs a key: `getHealth`
  (`GET /v1/health`) and `getContract` (`GET /v1/openapi.yaml`).
- `code` in an error is an enumeration, with its first five values:
  `invalid_request`, `not_found`, `method_not_allowed`, `payload_too_large`
  and `internal_error`. `type` is always `about:blank`.
- Every response carries `X-Request-Id`.
- `getKey` (`GET /v1/key`), the first operation that needs a key: it says
  which key the request was made with.
- Four more values of `code`: `unauthenticated` (`401`, with
  `WWW-Authenticate`), `forbidden` (`403`), `rate_limited` (`429`, with
  `Retry-After`) and `unavailable` (`503`).
- The limits of a key and of an address without a valid key, in the
  description of the document.
- The link, and its first three operations: `createLink`
  (`POST /v1/links`), `getLink` (`GET /v1/links/{id}`) and `listLinks`
  (`GET /v1/links`, paged with `limit` and an opaque `cursor`).
- An entry of `errors` says where the rejected value was sent (`in`), which
  one it is (`field`) and why (`code`, an enumeration with its first
  nineteen values).
- Three more values of `code` in an error: `unsupported_media_type`
  (`415`), and `alias_taken` and `quota_exceeded` (`409`).
- The other four operations of a link: `updateLink` (`PATCH
  /v1/links/{id}`, a JSON Merge Patch sent as
  `application/merge-patch+json`), `disableLink` and `enableLink` (`POST
  /v1/links/{id}/disable` and `/enable`) and `deleteLink` (`DELETE
  /v1/links/{id}`).
- `link_blocked` (`409`), for a change to a link the operator blocked.
- The redirect contract, in `redirect.md`: it was a table of `PLAN.md`
  section 3 until now. New with it: the `Host` of a request plays no part,
  and what the root and `/.well-known/security.txt` answer.
- The rate limit of the redirect: what counts as one client, that every
  request counts, and the unit of `Retry-After`.
- The skeleton of the management API: OpenAPI 3.1, the API key scheme and
  the shape of errors. No operations yet.
