# 0011. API-first OpenAPI contract

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Corinto is used first through its API, which is public from S1 and will
have external clients. Webhook events need schemas of their own, and the
redirect is an HTTP contract that no client is generated from. The panel is
rendered by the server and is not a client of the API (ADR 0014), so the
contract's readers are other systems and their authors.

Facts checked on 2026-11-12 and 2026-11-13. OpenAPI 3.2.1 is the latest
release. OpenAPI 3.1 has a top-level `webhooks` field, bases its Schema
Object on JSON Schema 2020-12, and lets a security requirement list role
names for schemes other than OAuth. In Go, support for 3.1 in the main code
generator is described as initial and dates from July 2026, and the
libraries that validate against a 3.1 document are below version 1.
`oasdiff` reports breaking changes and has a GitHub Action.

## Decision

Three contracts, each with one source: the management API, in a
hand-written OpenAPI 3.1 document that the binary embeds and serves, with
requests validated by the server and responses by the contract tests; the
redirect, in a table asserted by
tests from outside; and webhook events, in one JSON Schema per type and
version, referenced from the OpenAPI document and used to validate payloads
at runtime.

The document is written before the code and reviewed as a design, and
nothing is generated from it. PLAN.md section 7 holds the file of each
contract and the conventions of the API: versioning, errors, pagination and
authentication.

Whether the contract stays on OpenAPI 3.1 depends on the trial of PLAN.md
section 21. Its fallback lowers the version of the specification and keeps
the language.

## Alternatives considered

- Generate the document from code. One definition, and the server cannot
  drift. The contract becomes an output, shaped by the emitter, and is no
  longer reviewed as a design before the code.
- OpenAPI 3.0.3 from the start. Mature tooling everywhere. No `webhooks`
  section and a different schema dialect from the webhook schemas.
- Describe the redirect in OpenAPI too. One format for everything. Its
  semantics (what is cached, what is counted) do not fit a schema, and
  nobody generates a client from it.

## Consequences

- Positive: a contract change is one reviewed diff; breaking changes are
  caught by a tool; an external client can be written from the document
  alone.
- Negative: tests must prove that the server complies; the Go tooling for
  3.1 is young; no client in this repository consumes the contract.
- Strongest argument against: with generation from code, drift is
  impossible by construction, and here it is only detected.

## Reopen when

The trial fails, or a maintained way to validate a Go server against a 3.1
document stops existing.

## Links

ADR 0003, ADR 0013, ADR 0014. PLAN.md sections 7 and 21. OpenAPI Specification
3.1.1 (spec.openapis.org), RFC 9457, `oasdiff`, consulted 2026-11-12 and
2026-11-13.
