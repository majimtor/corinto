# Known issues and drills

What is known not to work, or to work within limits, in the code and in
its repository, with the date it was seen and what answers it; and the
drills of section 20 of the plan. A row leaves this file when its issue is
fixed, with the pull request that fixed it. The declared limits of how an
event is delivered have their own document, `docs/delivery-semantics.md`.

## Known issues

| Issue | Seen | Effect | Answer |
| --- | --- | --- | --- |
| The commit of the release `v0.1.0` on `main` carries a trailer that names a second author and, as its body, the description the release tool wrote | 2026-11-20 | One commit departs from the rule on messages. What is on `main` is never changed after a push, so it stays | From the next release on, a release pull request is merged with its title as the subject and one line as the body (the engineering rules). That GitHub then adds no trailer is checked at that release |
| The release tool builds its pull request again only when the release notes change. After a change that adds no note (`docs`, `ci`, `test`) the release pull request stays behind `main`, without that change, and the rules of `main` do not let it merge | 2026-11-20 | A release waits until its pull request is built again | Changing the description of the release pull request made the next run of the workflow build it again on `main`. Done once, for `v0.1.0` |
| A `redirect` that is running when `corinto seed` creates the demo workspaces again keeps the links that are gone until it reads its whole copy again, every five minutes. A link created again starts at the first version, and the copy takes a change only when its version is newer | 2026-11-20 | Until then the accesses to the short links of the seed are redirected and not counted: the inbox refuses their events | `make seed` starts `redirect` again. The rule of the copy stays: it is what makes a change that arrives twice harmless |
| The memory limit of `redirect` holds the 100,000 links the plan expects, and about twice as many, and no more | 2026-11-21 | Past that the process goes over its limit each time it reads its whole copy again, every five minutes. `docs/benchmarks.md` has the sizes that were measured and the two the limit falls between | The condition that reopens ADR 0006 |
| At the design volume the raw events take 9.2 GB of the 10 GB of their objective, and took up to 9.7 GB in one run | 2026-11-20 | Little room: a wider event or one more index misses the objective | `make bench` fails when it is missed. `docs/benchmarks.md` says what the figure depends on |
| Where the system breaks is not known. The break test reached its ceiling, 6,000 requests a second, with no objective missed, and the load generator, on the same machine, gave way first | 2026-11-20 | The published figure is a floor | A load generator on another machine (`docs/benchmarks.md`) |
| No command starts the system from the image of a release: `make up` and the codespace build it from source | 2026-11-21 | The image is published so that a reader can start without compiling (PLAN.md section 13), and none can yet without a Compose file of their own. Started that way by hand, `v0.1.0` works | The next slice |

## Clean-machine drill

Performed on 2026-11-21 on a fresh clone in a new directory, as section 20
of the plan asks: clone over HTTPS at `v0.1.0`, then `make verify`
(3 minutes, every step green: the static analysers, the contract against
the release, the unit and the integration tests with the four failure
tests, the secret scan over 63 commits, the Markdown lint, the image and
the load smoke). The machine was not a clean one: it was the maintainer's,
with the Go modules and the container images of earlier runs already on
it. Two things were done beyond what the plan asks. The stack of the clone
was started from the published image of `v0.1.0` in the place of a build,
and the tour of the README followed against it: a link created, a
destination outside the list of the demo workspace refused, the link
followed, its access counted and the reconciliation adding up. And
`make bench` ran once and met its objectives. Nothing had to change. Next
drill due by 2027-02-21.
