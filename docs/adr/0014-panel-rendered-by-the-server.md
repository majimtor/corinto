# 0014. Panel rendered by the server

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The panel is a reading interface with moderate state: lists, time series,
breakdowns, filters and a few forms. It is peripheral to what this project
demonstrates. It must be bilingual and meet WCAG 2.2 level AA, and the
repository must build after six months untouched. The backend is written in
Go, and the sister project Delfos already shows a React client with types
generated from a contract.

Facts checked between 2026-11-12 and 2026-11-14. `html/template` generates
HTML "safe against code injection" and escapes by context. Since Go 1.25
the standard library rejects forged requests from another origin without
tokens (ADR 0012). On the client side, the toolchain counted in ADR 0003
kept publishing majors, and `react-intl` published five in eleven months.

In a probe on 2026-11-13 and 2026-11-14, on the laptop named in ADR 0020,
one page with a chart, its summary, its data table and two languages took
136 lines of Go with the standard library alone. Served
with a policy that allows no script, and extended with a second chart and a
form, it showed no violation under axe-core 4.14.0 in Chrome in either
language, text contrast included.

## Decision

The panel is HTML rendered by the `api` process with `html/template`.
Templates, the stylesheet and the message catalogues are embedded in the
binary. Charts are SVG built on the server from numbers, each with the
text alternatives the engineering rules require. Every page works without
JavaScript and is sent with a content security policy that allows no
script; forms are plain posts. The session and the defence against forged
requests are in ADR 0012; the catalogues and the language of a page, in
ADR 0019 and PLAN.md section 16.

The panel is not a client of the HTTP API: it reaches the domain inside
the process, under the engineering rules for the panel.

A bounded check opens S2: the first real screen, the detail of a link with
its series, a breakdown and the table, in both languages and clean under
axe in a browser, within eight hours. If it does not come out, the panel is
built in React with Vite, as in the sister project, without further
discussion. A new record then supersedes this one and revises what depends
on it: the single image (ADR 0002), the contract's one authentication
scheme (ADR 0011, ADR 0012) and the rule that no response carries
cross-origin resource sharing headers.

## Alternatives considered

- React 19 with Vite, as a client of the API, with the libraries the sister
  project uses. Known hours, a typed client generated from the contract,
  interactive charts. It adds a toolchain to what ships, the part of a
  repository that ages fastest, a second image and a cross-origin setup,
  and shows nothing the sister project does not.
- Angular 22. Published support dates and fewer independently versioned
  parts. Nothing carries over, and its official internationalisation builds
  one application per locale.
- htmx for partial page updates. One file of 52 kB, no build step. No
  screen needs partial updates yet.

## Consequences

- Positive: one language and one image in what ships; no request crosses
  origins; nothing to build for the interface; escaping by default.
- Negative: every screen needs an HTML handler besides the API operation;
  templates are checked when they run, not when they compile, so a test
  has to render every page; charts are static; end-to-end tests need a
  browser, so Playwright and axe are test-only tooling.
- Strongest argument against: the panel does not exercise the public API
  as a client would, and the hours are unknown in a language the maintainer
  is learning.

## Reopen when

The check that opens S2 fails, or a screen needs interaction that a full
page load cannot give.

## Links

ADR 0002, ADR 0003, ADR 0011, ADR 0012, ADR 0019, ADR 0020. PLAN.md sections
10, 16 and 18. pkg.go.dev/html/template, pkg.go.dev/net/http, htmx.org, the
release and internationalisation guides of angular.dev, and npm registry
release dates, consulted between 2026-11-12 and 2026-11-14.
