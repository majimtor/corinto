# 0007. HTTP semantics of the redirect

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

A short link is useful only if every click is counted and a disabled link
stops working. Both depend on what clients cache.

Facts checked on 2026-11-12. RFC 9110 lists 301, 308, 404 and 410 among the
heuristically cacheable status codes and says all others are not (section
15.1); 302 and 307 are therefore stored only with explicit freshness. In
RFC 9111, `no-store` forbids private and shared caches from storing a
response (section 5.2.2.5). Chromium's source gives 301, 308 and 410 without
explicit freshness a lifetime that never expires, and Firefox's does the
same for 410. RFC 9110 allows 404 when
the server "is not willing to disclose" that a resource exists (section
15.5.5).

## Decision

An active link answers with a `302` and everything else that names a link
with one and the same `404`. Neither can be stored by a cache, the 404 does
not say whether the link ever existed, and the query string of the request
plays no part.

The full table, with methods, headers, the rate limit, the event each case
records and the 404 page, is in PLAN.md section 3 until S1 and in
`contract/redirect.md` from then on.

## Alternatives considered

- 307. It behaves like 302 for caches and also guarantees that the method
  does not change. That guarantee adds nothing when only `GET` and `HEAD`
  are redirected, and 302 has existed since HTTP/1.0.
- 301 or 308 with a short cache lifetime. Repeat clicks would be faster and
  would survive an outage. They would also go uncounted, and a disabled
  link would keep working in browsers that had visited it.
- 410 for expired and deleted links. More precise, and the specification
  suggests it for limited-time promotions. It confirms that the identifier
  existed, and Chromium and Firefox cache it without limit unless told
  otherwise.

## Consequences

- Positive: every click reaches the service; deactivation is honoured;
  identifiers cannot be probed for past existence.
- Negative: every click pays a round trip, and the visitor of an expired
  campaign gets a less precise message.
- Strongest argument against: if the service is down, every link is down
  with it; permanent redirects would have survived in browsers.

## Reopen when

A client or previewer in the classification test cases mishandles 302, or a
use needs permanent redirects; that would be an option per link.

## Links

ADR 0006, ADR 0008. PLAN.md section 3. RFC 9110 and RFC 9111 (June 2022);
`net/http/http_response_headers.cc` in chromium/chromium, branch `main`,
and `nsHttpResponseHead.cpp` in Firefox, consulted 2026-11-12.
