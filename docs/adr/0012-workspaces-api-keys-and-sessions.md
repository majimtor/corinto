# 0012. Workspaces, API keys and sessions

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Corinto is used first through its API, by systems rather than people. There
is no self-service sign-up in the first stretch and no teams. The panel
needs a way in for someone evaluating the project.

OWASP ASVS 5.0.0 accepts a standard hash for stored secrets with at least
112 bits of entropy (V6.5.2), asks for session reference tokens with at
least 128 bits from a secure generator (V7.2.3), forbids keys and tokens in
URLs (V14.2.1), and asks that requests to sensitive functions be validated
as coming from the application itself (V3.5.1). Checked on 2026-11-12.

Go's `http.CrossOriginProtection` rejects non-safe browser requests whose
`Sec-Fetch-Site` is neither `same-origin` nor `none`, or whose `Origin` does
not match `Host` when `Sec-Fetch-Site` is absent. In a test with Go 1.27 it
rejected a post from a sibling subdomain and from another site, and let
through a request with neither header, as a non-browser client sends. Behind
a proxy that rewrote `Host`, and only for requests without `Sec-Fetch-Site`,
it refused a legitimate post and accepted one whose `Origin` named the
upstream address. A cookie's `SameSite` attribute does not replace the
check: sibling subdomains count as the same site. With default settings,
nginx 1.31 and Apache 2.4 rewrote `Host`; Caddy 2.11, Traefik 3.7 and
HAProxy 3.4 kept it. Checked on 2026-11-14.

The local run serves plain HTTP on a loopback address. In a test on
2026-11-14, Chrome 157, Edge 157 and Firefox 159 kept a `__Host-` cookie
with `Secure` set that way; WebKit 27.2, the engine of Safari, discarded it
and every other `Secure` cookie, and kept one without the attribute.

## Decision

The workspace is the root of ownership and isolation (PLAN.md section 2).
Four principals act on it:

- An API key, for systems. Only a hash of its secret is stored, it is
  shown once and it travels only in a header. A key cannot create keys;
  PLAN.md section 9 says who can.
- A panel session, for a person evaluating the project: an opaque
  reference token, stored hashed and revocable, in a host-only cookie that
  scripts cannot read. The form of the cookie is set by configuration and
  not inferred from the connection, because TLS usually ends at a proxy;
  the form without `Secure` is confined to the local run. Only the panel's
  pages accept the cookie: the API authenticates by key and never reads
  it. Every non-safe request to the panel passes the standard library's
  cross-origin check, and nothing changes state on a `GET`.
- A webhook source, which can only post to its own endpoint (ADR 0013).
- The operator, who acts through a command of the binary against the
  database and has no HTTP endpoint.

A session starts from a demo entry that needs no secret and opens a session
on a seeded demo workspace. There are no personal accounts and no
passwords.

Authorisation is decided in one place, and a resource of another workspace
cannot be told from one that does not exist.

PLAN.md sections 6 and 9 specify each principal: formats, scopes,
lifetimes, the two forms of the cookie, the operator's actions, response
codes and limits.

## Alternatives considered

- Personal accounts with passwords now. A familiar login page. The demo
  passwords would be public and protect nothing, and the sister project
  already shows password sessions.
- An external identity provider. No credentials to store. A dependency on
  a third party for a panel whose only users are evaluators.
- Self-contained signed tokens for sessions. No lookup per request.
  Revocation needs a list anyway.
- HTTPS on the local run, to keep one cookie everywhere. A self-signed
  certificate greets the reader with a warning, and installing a local
  certificate authority is too much to ask of someone who came to look.
- A token in every form against forged requests. It also covers a browser
  that sends neither `Sec-Fetch-Site` nor `Origin`. It is state to issue
  and compare on every form; Go's documentation gives `Sec-Fetch-Site` as
  available in all browsers since 2023, and for an older one the cookie's
  `SameSite` attribute remains.

## Consequences

- Positive: no password handling to get wrong; keys belong to systems, not
  to people; adding accounts later is additive.
- Negative: the panel has no conventional login to show; the local run
  uses a weaker cookie than any HTTPS installation; the cross-origin
  check trusts the `Host` the process sees, so a proxy in front must pass
  it unchanged.
- Strongest argument against: a product without sign-in looks unfinished to
  someone who expects one.

## Reopen when

A public deployment exists and people the operator does not know ask for
workspaces, which is what self-service sign-up answers; or a second real
person needs access to a workspace. The password requirements of ASVS then
apply in full.

## Links

ADR 0011, ADR 0013, ADR 0014. PLAN.md sections 2, 6 and 9. OWASP ASVS 5.0.0,
consulted 2026-11-12; pkg.go.dev/net/http and the MDN glossary entry
"Site", consulted 2026-11-14.
