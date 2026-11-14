# 0015. Single repository with the Go module at the root

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The repository holds a server that also renders the panel, the contracts
it serves, the files that run it, load scenarios and browser tests. One
maintainer reviews everything, and a reader should understand the shape of
the project from the root listing.

Facts checked on 2026-11-13. Go's guide on module layout puts `go.mod` at
the root of a server project, the server's packages under `internal`, the
commands under `cmd`, and the directories with non-Go code next to them.
The `embed` package only embeds files inside the module and below the
directory of the embedding package. Since Go 1.25, the `ignore` directive
of `go.mod` excludes a directory from package patterns.

## Decision

One public repository, `corinto`, with the Go module
`github.com/majimtor/corinto` at its root, laid out as Go's guide
describes: the binary's commands under `cmd`, the server's packages under
`internal`, and the directories that are not Go next to them.

Packages are cut by responsibility, not by layer. Each owns its tables and
queries, and the database package is plumbing, not a repository layer. The
contracts live inside the module so that the binary can embed them, and the
browser tests are kept out of Go's package patterns with the `ignore`
directive.

A `Makefile` is the single entry point. PLAN.md section 11 holds the tree
and the targets; its list of packages is a starting point for S0, and the
criterion above is what is fixed.

## Alternatives considered

- The Go module in a subdirectory, leaving the root to documents. A
  shorter root listing. It departs from Go's documented layout, and the
  contract could not be embedded from outside the module without copying it
  at build time.
- A separate repository for the contract, so that clients depend on it
  alone. A clean boundary for outside consumers. Every contract change
  becomes two coordinated pull requests and a published version, before a
  second consumer exists.

## Consequences

- Positive: a contract change and its implementation are one pull request;
  the release tag of the repository is the version of the module; the
  contract has one copy.
- Negative: the root mixes Go files with documents and tooling.
- Strongest argument against: browser tests bring a second toolchain into
  a repository that otherwise has one.

## Reopen when

A client of the contract is maintained by someone else, or a second Go
module appears in the repository.

## Links

ADR 0003, ADR 0011, ADR 0014. PLAN.md section 11. go.dev/doc/modules/layout,
pkg.go.dev/embed, go.dev/doc/go1.25, consulted 2026-11-13.
