# Redirect contract

What the host that serves the short links answers. This file is the only
source of it: `PLAN.md` section 3 summarises it, the `redirect` process
implements it, and its tests assert it from outside, status codes and
headers included. ADR 0007 gives the reasons for each choice.

## A request that names a link

The path of a short link is a slash and the identifier of the link. The
letters `A` to `Z` of the path are lower-cased before the link is looked
up, and nothing else in it is changed.

| Case | Response | Event |
| --- | --- | --- |
| Active link, `GET` | `302 Found`, `Location` with the resolved destination, no body | `redirected` |
| Active link, `HEAD` | The same | `redirected`, classified as automated |
| Disabled, expired or blocked | `404` with the common page | `inactive` |
| Deleted, unknown, or a path outside the grammar | The same `404` | None; a counter |
| Any other method | `405` with `Allow: GET, HEAD`, no body | None |
| Over the rate limit | `429` with `Retry-After`, no body | None; a counter |

- The resolved destination is the one stored when the link was written,
  with its campaign parameters, and is sent exactly as stored.
- The query string of the request is ignored. It does not choose the link,
  it is not added to the destination and it is not recorded.
- The `Host` of the request plays no part. The process serves one redirect
  domain, and no response is built from that header.
- A link past its `expires_at` does not redirect from that instant on.
- The grammar of an identifier is the one of `PLAN.md` section 2: 3 to 40
  characters among `a` to `z`, `0` to `9` and the hyphen, with no hyphen at
  either end. A path that is not a slash and an identifier names no link.

## The 404 page

One page for every `404`, so that the answer does not say whether a link
ever existed: an inactive link, a deleted one, an unknown one and a path
outside the grammar get the same bytes.

- It is static, in Spanish and in English, with each part marked with its
  language.
- It carries no script and loads nothing from anywhere: it is one
  self-contained response.
- Its only style is inline, and its content security policy allows that
  style by its hash and nothing else: `default-src 'none'`, the hash in
  `style-src`, and `base-uri`, `form-action` and `frame-ancestors` set to
  `'none'`.
- A `HEAD` gets the headers without the page.

## Other paths

| Path | Response |
| --- | --- |
| `/` | `302 Found` to the page of the project, no body |
| `/.well-known/security.txt` | `200` with the file of RFC 9116 as `text/plain` |

Neither can collide with an identifier: the grammar admits neither dots,
nor slashes, nor an empty identifier. Both answer `405` to a method other
than `GET` and `HEAD`.

`security.txt` names where to report a vulnerability, the policy in
`SECURITY.md` and the languages a report can be written in. Its `Expires`
is 90 days after the process started: a process that has not been deployed
for longer than that says, truthfully, that nobody has reviewed the file.

## Every response

- `Cache-Control: no-store`. Nothing between the host and the visitor keeps
  an answer: a link that is disabled stops redirecting for everyone.
- `X-Content-Type-Options: nosniff`, and a content type with its character
  set whenever there is a body.
- No cookie, no script and no cross-origin header.
