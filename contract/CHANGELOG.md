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
- The skeleton of the management API: OpenAPI 3.1, the API key scheme and
  the shape of errors. No operations yet.
