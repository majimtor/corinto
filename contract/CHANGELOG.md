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
- The skeleton of the management API: OpenAPI 3.1, the API key scheme and
  the shape of errors. No operations yet.
