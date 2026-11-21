# Engineering rules

These rules apply to every change in this repository, whoever makes it.
Each exists to prevent a failure, named where it is not obvious. When a
rule and a situation do not fit, the exception is written down next to the
code with the reason, and the rule is revisited.

## Language

- Code, identifiers, comments, commit messages, pull requests, documentation
  and configuration are written in English.
- Sample names in the seed are content, not interface text: they use
  proper nouns and channel names, and English where a common noun is
  needed.
- Error responses of the API are not translated: clients translate from
  the code, and a response does not change with a header.

## Commits

- One logical change per commit. Messages follow Conventional Commits:
  `type(scope): summary` in the imperative, at most 72 characters, with a
  body that explains why when the diff does not. Types in use: `feat`,
  `fix`, `docs`, `test`, `ci`, `build`, `chore`, `refactor`, `perf`.
- Forbidden summaries: `wip`, `update`, `fix` without an object, `misc`.
- A commit has one author. Messages carry no trailers.
- History is never rewritten after a push. A mistake gets a new commit.
- Commits are signed. The author identity and the signing key are
  configured per repository and checked with
  `git config --show-origin user.email` before the first commit on a new
  machine. The one exception is the commit of the release pull request,
  which the release tool writes through the API without a signature: it
  never reaches `main` as such, because the squash merge creates the commit
  on `main`, signed by GitHub. For that reason the rules of `main` do not
  require signed commits on pull request branches; every commit on `main`
  is signed all the same.
- The release pull request is merged by the maintainer, never
  automatically, with its title as the subject and one line as the body.
  The tool's own description stays in the pull request, and the release
  notes are in `CHANGELOG.md`.

## Branches and pull requests

- `main` always builds and passes `make verify`. Work happens on
  short-lived branches named `type/short-description`, merged through a
  pull request with squash. The pull request title becomes the commit
  message, so it follows the commit rules.
- A pull request answers five questions in its description: what changes,
  which failure it prevents or which capability it adds, which test covers
  it, what is deliberately left out, which document changes with it.
- Keep pull requests small enough to read in one sitting, as a guide under
  400 changed lines. Split otherwise.
- Required checks must pass before merging, for the maintainer too
  (ADR 0017). There is no second reviewer; the description above is the
  review.
- Exception, written here on purpose: the documentation commits that
  precede the first CI workflow go directly to `main`, because there is
  neither a base branch nor a check to require. From the commit that adds
  the workflow on, everything goes through a pull request.

## The API contract

- The contracts of PLAN.md section 7 are the single source of truth: code
  follows them, never the reverse. Their tests assert them from outside,
  against the running server, status codes and headers included.
- The schema file of a webhook event validates payloads at runtime and in
  tests alike. An incompatible schema is a new type, never an edit.
- A change to a contract and the code that implements it travel in the same
  pull request, with an entry in `contract/CHANGELOG.md`. After a release,
  the entries under `Unreleased` move under a heading with its version.
- Breaking changes are what `oasdiff breaking` reports. What they require
  is the versioning of PLAN.md section 7.
- No endpoint returns a bare string or an ad hoc object on failure: errors
  have the one shape of the contract.
- No endpoint returns an unbounded collection.

## Schema and migrations

- The schema lives in forward-only SQL migrations embedded in the binary
  and applied by `corinto migrate`. Nothing else creates or alters tables.
- A process refuses to start when the schema version in the database is not
  the one it was built with. This failure is intended and must not be
  silenced.
- A migration is immutable once merged. A correction is a new migration.
- Uniqueness is enforced by a database constraint, never by a check before
  the insert.
- A statement that updates several rows of an aggregate table inserts them
  in key order. Concurrent consumers that upsert the same rows in different
  orders deadlock; a regression test keeps this rule.
- Each package owns its tables and its queries. There is no generic
  repository layer.
- Enumerations of the domain (states, traffic classes, scopes) are code,
  not lookup tables.

## Environment and lockfiles

- Tool versions are pinned in one place per ecosystem: `go.mod` and
  `go.sum` for the binary; `.node-version`, `engines` and
  `package-lock.json` for the browser tests in `e2e/`, installed with
  `npm ci`. A mismatch fails the build instead of warning.
- Container base images are pinned by digest and the CI runner image by its
  versioned label. The command-line tools that CI and `make` run (linters,
  `oasdiff`, `gitleaks`, k6) are pinned by exact version in one place that
  both read.
- Lockfiles are committed. A change to a lockfile without a change to the
  corresponding manifest is reviewed as a dependency change.

## Dependencies

- Every dependency has a reason written at the point where it is added: the
  pull request description and, for the long-lived ones, the ADR that
  justifies it. "Convenient" is not a reason. The standard library comes
  first.
- Node dependencies exist only in `e2e/`, for the browser tests. Nothing
  from that directory ships in the image.
- Automated updates are grouped per ecosystem. Patch and minor versions are
  merged when checks pass. Major versions are updated by hand, one at a
  time, with the migration notes read first.
- A pull request of the dependency bot is merged by the maintainer, never
  automatically, with its title as the subject and one line as the body.
  The bot's own description, with its release notes and trailers, stays in
  the pull request. One that falls behind `main` is rebased by the bot on
  request, not updated by hand, so that its branch keeps one author. One
  that changes what a published figure depends on is merged with the
  figure redone (ADR 0020).
- A critical vulnerability in a dependency is fixed within seven days while
  the project is active. When the project is paused the README says so, and
  no time frame runs.
- GitHub Actions are pinned to a commit SHA, run with the minimum
  permissions and never use `pull_request_target`.

## Infrastructure and experimental technology

- No broker, distributed cache, analytical store, orchestrator or
  additional service enters without a named requirement and a measurement
  showing that the mechanism already in place fails it. The measurement is
  written next to the component.
- Nothing in beta, preview or experimental status enters without an ADR
  that names the stable alternative it displaces, where it is contained and
  how it is removed.
- A deprecated API or library version is not introduced. One already in use
  is replaced before the next major update of its ecosystem.

## Secrets and personal data

- No secret in the repository, ever: no keys, passwords, tokens, connection
  strings or signing material, in code, comments, examples, tests or
  history. `.env.example` documents the variables without values.
- API keys and webhook signing secrets of the seed are generated when
  seeding or read from the environment. They are never committed, not even
  as examples.
- A secret scan runs before every commit and in CI over the whole history,
  and push protection stays enabled. A secret that reaches the history is
  revoked; the commit stays.
- No real personal data in the repository, the seed or the tests.
  Workspaces, links and webhook sources are fictitious, the event history
  is synthetic, and addresses use reserved documentation domains.
- A visitor's IP address is never persisted or logged in any form: not raw,
  not truncated, not hashed. Logs carry no user-agent string, no full
  referrer URL, no query string, no webhook body, no signature and no key.
  A test sends sentinel values through the system and fails if any of them
  appears in the log output.
- Rate limiting keeps its counters in memory only, under a keyed hash of
  the client address that expires (PLAN.md section 17).
- What differs between installations (addresses, hosts, credentials) comes
  from the environment. A missing value fails startup with a message naming
  the variable. Limits and quotas are not in that group (Code
  conventions).

## Names of metrics

- No metric is named above what it measures. The panel and the API say
  "requests", "estimated human clicks" and "reported conversions"; they
  never say "users", "visitors" or "unique users".
- A metric that is an estimate says so in its name, and its definition and
  its limit sit next to the number.
- No chart exists without the question it answers. Every chart carries a
  title, a one-sentence summary and its data table.

## Published figures

- Every performance figure carries the machine, the container limits, the
  load model and rate, the duration, the tool and its version, the command
  and the date.
- A published figure is the median of at least five runs, with the minimum
  and the maximum, taken on a machine with no other load.
- A figure that cannot be reproduced with one command from this repository
  is not published. What has not been measured is stated as an objective
  with its method of measurement, never as a fact.
- Words such as "fast" or "low latency" do not appear without a number and
  its method.

## The panel

- Pages are rendered by the server from templates, and every page works
  with scripts disabled. The panel ships no script; a screen that cannot do
  without one reopens ADR 0014.
- Outside text reaches a page only through `html/template`. The types that
  bypass its escaping (`template.HTML` and its siblings) are used only in
  the panel's chart package, on markup built from numbers; a lint rule
  keeps them out of every other package.
- State changes are form posts, never a `GET`. Every non-safe request to
  the panel passes the standard library's cross-origin check; a test posts
  from another site and from a sibling subdomain and expects a refusal.
- The session cookie is read only by the panel's handlers. A test calls an
  API operation with a valid cookie and no key and expects `401`.
- The form of the session cookie without `Secure` (PLAN.md section 9)
  exists for the local run alone. A test asks for a session in that form
  under a host name that is not loopback and expects a refusal.
- Panel handlers reach the domain through the same packages as the API and
  through the same authorization point. A handler holds no query of its
  own.
- Templates carry no literal interface text, and both catalogues change in
  the same pull request. One test compares the two catalogues key by key
  and placeholder by placeholder. Another walks the
  parsed templates and fails on literal words in elements, in the
  attributes people read and in string arguments, outside a short list of
  language-neutral words.
- Every HTML response carries a content security policy that allows no
  script, no object and no base URL. Panel pages take their styles only
  from the server itself, so templates and charts use classes, never a
  `style` attribute; the redirect's 404 page allows only its one inline
  style, by hash. A test asserts the header on every page.
- A test renders every page with seeded data.

## Security headers and CI gates

- Every response carries `X-Content-Type-Options: nosniff`, and every
  response with a body declares its content type with its character set.
  No response carries a cross-origin resource sharing header: no browser
  calls the API from another origin. A test asserts all three.
- The cross-origin check trusts the `Host` the process sees. A proxy in
  front passes it unchanged, and `X-Forwarded-Host` is never read.
- A destination is parsed once, validated, stored in its normalised form
  and served exactly as stored. PLAN.md section 2 lists the checks.
- Every gate of PLAN.md section 13 blocks the merge; there is no "allow
  failure".

## Code conventions

- Names say what a thing is in the domain: `Link`, `Workspace`,
  `resolve.Replica`, `ingest.Buffer`. Generic names such as `data`,
  `items`, `result`, `Manager`, `utils`, `common`, `helpers` and `base` are
  not used for packages or types.
- Packages are organised by responsibility, not by layer. An interface is
  declared where it is consumed, and only when a second implementation or a
  test needs it. There is no DTO, mapper or service created by reflex.
- Comments explain why, not what: the constraint, the bug or the
  measurement behind a line that looks odd. No comment restates the code.
- Configuration that came from an example (a Dockerfile, a workflow, a
  compose file) carries a comment on every line that is not obvious, saying
  why it is there.
- Facts have one source: the schema (migrations), the contracts
  (`contract/`), tool versions (the pinned files), limits and default quotas
  (one configuration file with a comment per value). Code may be duplicated
  where abstracting it would couple unrelated parts; the duplication is
  noted at both sites.
- Every abstraction has a documented escape hatch for the case that does
  not fit.
- Failures are loud: a broken invariant returns an error that stops the
  operation, and a failed migration stops the run. Errors are mapped to
  contract codes in one place.
- A degraded mode is a written decision. What happens when the database,
  the worker or the buffer fails is stated in `docs/delivery-semantics.md`
  before the code exists, and each statement names the test that checks
  it or says that it is a declared limit.
- The critical parts (the redirect, ingestion, signatures) are polished;
  the peripheral ones are deliberately simple, with a comment saying why
  they did not deserve more.
- Known limitations are recorded in `docs/known-issues.md` with the reason
  they were accepted and what would reopen them.

## Documentation and writing

- Decisions that are expensive to reverse get an ADR, under the
  conventions of `docs/adr/README.md`. A draft that only confirms a choice
  is rewritten or dropped before it is accepted.
- The README describes what the project does, how to run it with one
  command, how to run the tests and where the decisions are. It does not
  describe what the code already says. Badges are limited to the CI status
  and the licence.
- A principle is never cited by name as a justification. The text states
  the failure it prevents.
- Omissions carry the condition that reopens them.
- Text avoids promotional adjectives ("robust", "scalable", "seamless",
  "blazing fast"), headings phrased as questions, and closing paragraphs
  that repeat what was said. Prose is used where a list would break an
  argument into fragments.
- Personal tooling files (editor settings, local notes, local task runners)
  are excluded through `.git/info/exclude` or a global ignore file, not
  through the repository's `.gitignore`.
