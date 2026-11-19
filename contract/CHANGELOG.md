# Contract changelog

Changes to the contracts in this directory, newest first. A change that
`oasdiff breaking` reports is marked as breaking.

## Unreleased

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
- The skeleton of the management API: OpenAPI 3.1, the API key scheme and
  the shape of errors. No operations yet.
