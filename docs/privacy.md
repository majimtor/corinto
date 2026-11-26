# Privacy

What Corinto does with a request to a short link: what it reads, what it
keeps, where and for how long. It is written for whoever deploys the
software. It describes what is built so far, which has no webhooks; a
slice that keeps something new adds it here. PLAN.md section
17 holds the summary, and ADR 0008 and ADR 0010 the reasons.

This is a technical reading of the code and of the sources those records
cite. It is not legal advice.

## What a request carries

Corinto writes nothing to the visitor's device and reads nothing from it:
no cookie, no script, no local storage, no fingerprint.

| Datum | What happens to it | What is kept |
| --- | --- | --- |
| IP address | Taken from the connection, or from the forwarding header when the connection comes from a proxy named in the configuration. Used as the key of a rate limit, after a keyed hash | A key in memory, for seconds. Never persisted or logged in any form: not raw, not truncated, not hashed |
| User-agent | Its first 512 bytes are classified, inside the request, into a traffic class, a device class and a browser family | The three values, in the event. The string, in memory only |
| Referrer | Its host is taken inside the request and the rest is dropped | The host, in the event and in a daily breakdown |
| Query string of the short link | Ignored: it does not choose the link and it is not added to the destination | Nothing |
| Headers that say a request is a prefetch | Read to classify the request | The traffic class |
| Method | Read to classify the request | The method, in the event |
| Cookies and every other header | Not used | Nothing |
| Time of the access | The clock of the server | To the second in the event; then only the hour and the day |

The three classifications are closed enumerations. A traffic class is one
of `prefetch`, `automation`, `preview`, `crawler` and `presumed_human`; a
device class, one of `desktop`, `mobile`, `tablet` and `other`; a browser
family, one of `chrome`, `safari`, `firefox`, `edge`, `opera`,
`samsung_internet` and `other`.

Neither the country nor the operating system of an access is worked out.
Nothing links two accesses to each other: there is no identifier of a
visitor, of a session or of a click.

Checked by `TestAnEventCarriesNothingOfTheVisitor` in `internal/redirect`,
which sends marked values in every part of a request and looks for them in
the event.

## What is stored

Everything stored is in PostgreSQL. A job of `worker` deletes once a day
what is due, and the ingestion status of the API says when it last
finished.

| What | What it holds | Deleted |
| --- | --- | --- |
| Access event (`event_inbox`) | Its identifier, the link and its workspace, the time to the second, the outcome, the method, the referrer host, the device class, the browser family, the traffic class with the rule that gave it and the version of the rules, and when it was accepted and when it was processed | 30 days after it was accepted, once it has been processed |
| Event set aside (`event_dead_letters`) | The same, and why the database did not take it: an error code and the name of a constraint | 30 days after it was set aside |
| Hourly series (`link_hourly`) | A count per link, hour of UTC, outcome and traffic class | 13 months after its day |
| Daily breakdowns (`link_daily_dimension`) | A count of estimated human clicks per link, day of UTC and one value of one dimension: referrer host, device class or browser family. Never two dimensions together | 13 months after its day |
| Reconciliation counts (`pipeline_counters`) | Counts per workspace and day of UTC: how many responses were served and what became of their events | With the aggregates of that day |
| Versions of the rules (`rule_versions`) | For each version of the classification rules, the first and the last hour in which it classified an event | Never. It describes the rules, not an access |
| Retention runs (`retention_runs`) | When the job ran and how many rows it deleted | 13 months after the run |
| Panel session (`sessions`) | Its workspace, the SHA-256 of its token, and when it was opened, when it ends and when it was last used, to the minute. Nothing of whoever opened it: no address and no user-agent | When it is closed, and when its workspace is blocked or seeded again. One that ends by itself, twelve hours after it was opened or thirty minutes after its last request, is deleted by the daily job |

The referrer host is the one thing kept that the requester writes. An
event keeps the host it named for its 30 days. A breakdown keeps at most
100 distinct hosts per link and day and counts the rest together. A host
name is not chosen from a list, so it can be one that says something: an
internal site, for example.

Counts are kept as they are: a small one is neither rounded nor left out.
An hour with one request to a link says that one request happened.

## What lives in memory only

- **Rate-limit keys.** An address becomes a key through HMAC-SHA-256 under
  a secret that each process generates when it starts and writes nowhere,
  so a key cannot be turned back into an address, by reading it or by
  trying every address. An IPv6 address counts as its /64. A key is
  forgotten once its client has its whole allowance back: within three
  seconds of its last request on the redirect, and within about a minute
  in the API, which counts by address only the requests that bring no
  valid key. Checked by `TestAnAddressIsKeptOnlyAsAKeyedHash` and
  `TestALimiterForgetsAKeyWithoutGivingAnythingAway` in `internal/limits`.
- **User-agent strings.** A cache remembers up to 10,000 of them with
  their classification, so that the same one is not classified again. A
  string stays there until the cache fills, when it is emptied whole, or
  until the process stops. Corinto writes it to no disk and to no log.
  Checked by `TestTheCacheIsBoundedAndChangesNothing` in
  `internal/classify`.
- **Events on their way.** Up to 50,000 events wait in the memory of
  `redirect` to be written, with the fields of the table above and no
  other.

## Logs

Each process writes JSON lines to its standard output. Where they go from
there, and for how long, belongs to the deployment.

- `redirect` writes no line for a request. What it logs about a failure
  carries nothing of the request that failed: not the address, the path,
  the query string or a header. Checked by
  `TestTheAnswerDoesNotDependOnWhatItLeavesBehind` and
  `TestAPanicIsA500ThatLogsNothingOfTheRequest` in `internal/redirect`.
- `api` writes one line for each request: its identifier, the method, the
  route as a pattern and never as a path, the status, the duration and,
  when the request brought a valid key, the identifier of that key. No
  address, no user-agent, no query string and no credential. Checked by
  `TestTheLogNamesAValidKeyAndNeverACredential` in `internal/api`. A
  request to the panel leaves the same line, with the identifier of its
  session where that of a key would be, and never its token. Checked by
  `TestTheLogNamesTheSessionAndNeverTheTokenOrThePath` in `internal/panel`.

## What the people who manage links leave

There are no accounts, no passwords and no e-mail addresses yet: the
operator creates a workspace from the command line, and it is used with
API keys. The panel is entered through the demo workspaces alone, without
a secret.

- A workspace and a key each have a name, which is whatever was typed.
- The secret of a key is stored only as a SHA-256 hash. The key is shown
  once, when it is issued: by the command that issues it, or in the answer
  to the form of the panel that does, which no cache keeps.
- For each key, the day it was last used: a day, not a time.
- The links: their destinations, campaign parameters, titles and tags.
- The address a request to the API or to the demo entry of the panel
  comes from is treated like the address of a visitor: a rate-limit key in
  memory, and nothing else. The changes a session of the panel makes are
  counted in memory too, by the identifier of the session, for a minute.
- A session of the panel is one cookie, `__Host-session`, or `session` in
  the local run. It holds a random token that names the session and
  nothing else, a script cannot read it, it goes back only to the host
  that set it, and it goes when the browser is closed. It is all the panel
  writes to a browser: no other cookie, no script and no storage. Leaving
  takes it away and tells the browser to drop what it cached of the panel.
  Checked by `TestEnteringOpensASessionAndLeadsToTheLinks` and
  `TestLeavingClosesTheSessionOnTheServerAndInTheBrowser` in
  `internal/panel`.

The demo workspaces of the seed are fictitious, and so is their history:
the seed makes up its requests and nobody made them. The load scenario
sends its requests from addresses reserved for benchmarks.

## What Corinto sends elsewhere

Nothing. It calls no external service, and no datum of a request leaves
the process and its database. The destination of a link receives the
visitor directly, as after any redirect. Its address carries the campaign
parameters of the link, which name a campaign and are the same for every
visitor.

## What a deployment adds

There is no public deployment of Corinto, so the project processes no real
visitor's data. Using the address and the user-agent in passing, as above,
is still processing of personal data: whoever puts Corinto in front of
real visitors decides on that processing, and needs what the software
cannot bring.

- **A privacy notice, a legal basis and a record of processing.** The
  design aims at what the Spanish data protection authority lists as
  audience measurement exempt from consent, with statistics aggregated by
  day and kept well under its limit of twenty-five months (ADR 0008,
  ADR 0010). Whether a deployment is inside that list is for its owner to
  establish.
- **Whatever sits in front.** A proxy, a load balancer or a hosting
  platform keeps logs of its own, usually with the address and the
  user-agent of every request. They are outside this software, and they
  are where an address is most likely to be written down.
- **Backups.** The periods above are those of the live database. A backup
  keeps what has been deleted since it was taken, for as long as the
  backup lives.
- **The log lines.** They carry no datum of a visitor, and an identifier
  of a key in the lines of `api`. How long they are kept is decided where
  they are collected.
- **Memory.** The user-agent strings and the rate-limit keys are in the
  memory of a process. A platform that writes memory to disk, in a dump
  or in swap, can write them with it.
