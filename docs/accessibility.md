# Accessibility

The panel targets WCAG 2.2 at level AA (PLAN.md section 16). This document
says what checks that on every change, what was reviewed by hand and on
which day, and what does not conform or was not checked. It is about the
pages of the panel. The page the short links answer when there is no link
has its own rules in `contract/redirect.md`, and was not part of this
review.

## What is checked on every change

Each of these runs in CI, and a change that breaks one does not merge.

- **axe in a browser**, on each of the fourteen pages of the path that
  `e2e/tests/path.spec.js` walks in Chromium and in Spanish, and again on
  six pages at 320 pixels wide. It runs every rule it runs unasked, and the
  rule of the size of a target, its only rule of WCAG 2.2, which is off
  unless asked. By its own statement axe finds on average 57 % of the
  issues a page can have (PLAN.md section 16): what follows is for the
  rest.
- **The contrast of every colour.** The stylesheet names its ten colours in
  one block and uses no other. `TestTheColoursOfTheStylesheetKeepTheirContrast`
  in `internal/panel` computes the contrast of each pair the stylesheet
  puts together and fails under 4.5 to 1 for text and under 3 to 1 for
  what a chart draws and for the edges of a page.
- **The keyboard, the focus, the titles, the language, colour and the
  narrow screen**, in `e2e/tests/access.spec.js`, which is the review
  below turned into a test wherever it could be.
- **What is wrong with a form**, through the handlers, in
  `TestWhatIsWrongWithAFormIsSaidNextToItsField` and
  `TestWhatIsWrongWithTheFormOfAKeyIsSaidNextToItsField` in
  `internal/panel`: each problem has its text next to its field and in a
  summary, in both languages.
- **Both languages have every text.** A template holds no word of its own,
  and the two catalogues are compared key by key
  (`docs/engineering-rules.md`).

## The review of 2026-11-27

Made on the pages of the panel as the local stack serves them with the
seed, in Chromium 159 at 1280, 390 and 320 pixels wide, with the keyboard
alone where the criterion is about the keyboard, and by reading each page
and its markup. The plan lists what a tool does not see (section 16); the
table follows that list.

| Criterion | What was looked at | Result |
| --- | --- | --- |
| 1.4.1 Use of colour | The two series of a chart differ in their line, solid and dashed, and in their marker, a circle and a square, in the drawing and in its legend. The period that is shown is heavier and has a rule under it. What is wrong with a field is said in words and marked for a screen reader. The state of a link and of a key is a word. A link is underlined | Conforms |
| 1.4.3 Contrast | Text on the page 19.2 to 1; the text that explains, and the labels inside a chart, which are set at 12 pixels, 7.7; a link 6.1; what is wrong with a form 6.4; the text of the two kinds of button 6.1 and 6.4; the text of a notice 17.8 | Conforms |
| 1.4.11 Non-text contrast | The two series against the page, 4.3 and 3.1 to 1; the bar of a breakdown on its track, 3.1; the edges of the page, 3.4; the edge of a field, 7.7; the ring of the focus, 6.1. The lines of the grid of a chart and the lines between the rows of a table are at 1.4 on purpose: no value is read from them alone, since every value of a chart is in its table | Conforms. The second series is close to the limit |
| 1.4.10 Reflow | At 320 pixels no page scrolls sideways. The drawing of a series and the tables of a series and of the status are wider than that: each is inside a region of its own that scrolls, has a name and takes the keyboard | Conforms, by the exception for what needs two dimensions |
| 2.1.1 Keyboard, 2.1.2 No keyboard trap | On the door, the list, the page of a link, the two forms, the keys and the status, the Tab key visits every link, button, field and region once, in the order of the page, and goes on. The table of a series opens with Enter. The first stop of every page skips to its content | Conforms |
| 2.4.2 Page titled | Every title names its page. The page of a link and the status name their period, and the title changes with it. The title of a form that came back wrong begins with "Error:" | Conforms |
| 2.4.7 Focus visible, 2.4.11 Focus not obscured | Every stop of the keyboard shows a ring three pixels wide. No part of a page stays in place over the rest, so nothing covers what has the focus | Conforms |
| 2.5.8 Target size | The rule of axe on every page. Links that stand alone in a bar or in a row are 24 pixels high by their padding; the boxes of a form are 24 pixels a side | Conforms |
| 3.1.1 Language of page, 3.1.2 Language of parts | A page says the language its address names. The link to the other language is written in that language and marked as such | Conforms |
| 3.3.1 Error identification, 3.3.3 Error suggestion | What is wrong with a form is a sentence that says what to do, next to its field and described by it, and in a summary at the top that leads to the field | Conforms |

## What does not conform

- **2.2.1 Timing adjustable.** A session ends thirty minutes after its last
  request and twelve hours after it began (PLAN.md section 9). The door
  says so before anyone enters. Nothing warns that the time is about to
  run out, and nothing extends it but another request: a form that is
  sent after it is not taken, and what was typed in it is lost. The panel
  has no script to warn with. In a demo workspace entering again is one
  press and nothing private is behind the session; it has to be solved
  before the panel has accounts.

## What was not checked

- **A screen reader.** The pages have the names, the roles and the
  structure that axe asks for, and they were not listened to with one.
- **Text spacing and zoom.** The pages were read at three widths, of which
  320 pixels is what a zoom of 400 % gives a screen 1280 wide. They were
  not read with the spacing of text overridden (1.4.12), or with a zoom
  that enlarges text alone.
- **Forced colours and a dark scheme.** The panel has one colour scheme,
  light. It was not read in the high-contrast mode of an operating system.
- **The field of date and time.** It is the control of the browser, and
  how it behaves with a keyboard and with a screen reader differs between
  engines. It was read in Chromium alone.
- **Firefox and WebKit** run the demo entry in the browser tests, and
  nothing else: the review was made in Chromium.
- **People.** No person who depends on an assistive technology has used
  the panel.
