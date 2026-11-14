# 0019. English codebase with a bilingual panel

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

The repository is read by engineers anywhere; the panel is used first by
Spanish speakers. Every translated string is two artefacts, and the usual
way this fails is one of them going stale. The panel is rendered by the
server (ADR 0014), so its text is resolved in Go.

Facts checked on 2026-11-13 and 2026-11-14. `golang.org/x/text` 0.44.0
formats numbers and plurals by language. Its catalogue and plural packages
state that they are under construction and that their API may change, and
in a test its catalogue did not fall back to another language key by key.
In a probe, a test of about 230 lines over the parsed templates found
literal text in elements, attributes and actions, with no false positive on
clean templates. WCAG 2.2 requires the language of the page and of its
parts to be programmatically determinable (3.1.1 and 3.1.2).

## Decision

- Everything versioned except interface text is in English.
- The panel is bilingual, Spanish first. Its text lives in two catalogues
  embedded in the binary, with no translation library: a small helper
  written with the standard library formats plurals, numbers and dates,
  which is all the panel's messages need in these two languages.
- The API returns codes, not translated messages, and has no catalogue.
- Content is not interface text and is not translated.

PLAN.md section 16 specifies the mechanism, and the engineering rules hold
what each change must respect and the tests that check it. Those tests
catch a missing or malformed translation. A text changed in one language
only is not caught by a tool; the rule that both catalogues change
together, and review, cover it.

## Alternatives considered

- An English-only panel. Half the strings and no catalogue to compare. The
  panel's first readers would be using a second language.
- `golang.org/x/text` for catalogues, plurals and numbers. Rules for any
  language, maintained by the Go project. Two of the packages needed
  declare themselves under construction, key-by-key fallback would still
  be the project's own code, and two languages with two plural forms do not
  need it.
- Translated messages from the API. Clients would show them as they come.
  The public surface would carry a catalogue, and responses would change
  with a header.
- The seed in both languages. Each sample would exist twice, and the two
  copies would drift.

## Consequences

- Positive: one language for everything a contributor reads; the panel in
  the language of its first users; the API stays language-neutral; no
  dependency for translation.
- Negative: every interface string is written twice (PLAN.md section 16
  estimates how many); the formatting helper is code to maintain; no tool
  detects an English text left behind when the Spanish one changes.
- Strongest argument against: a bilingual interface doubles a cost for a
  panel that is not what this project is about.

## Reopen when

A third language is requested, a message needs a plural or number rule the
helper does not cover, or translations left behind reach `main` in spite of
the comparison test and review.

## Links

ADR 0014. PLAN.md section 16. pkg.go.dev/golang.org/x/text; WCAG 2.2
(w3.org/TR/WCAG22); consulted 2026-11-13 and 2026-11-14.
