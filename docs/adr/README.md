# Architecture decision records

Each record captures one decision that is expensive to reverse: the context
that forced it, the alternatives that were really on the table, what was
accepted knowingly, and the observable condition under which it is
reopened. Records are numbered in the order they were written and never
edited after acceptance except to change their status; a change of mind is a
new record that supersedes the old one.

Template: [0000-adr-template.md](0000-adr-template.md).

| Number | Title | Status |
| --- | --- | --- |
| [0001](0001-reproducible-first-without-a-public-deployment.md) | Reproducible first, without a public deployment | accepted |
| [0002](0002-one-image-three-processes.md) | One image, three processes | accepted |
| [0003](0003-backend-in-go.md) | Backend in Go | accepted |
| [0004](0004-postgresql-as-store-and-queue.md) | PostgreSQL as store and queue | accepted |
| [0005](0005-delivery-semantics-and-degradation.md) | Delivery semantics and degradation | accepted |
| [0006](0006-in-memory-replica-for-link-resolution.md) | In-memory replica for link resolution | accepted |
| [0007](0007-http-semantics-of-the-redirect.md) | HTTP semantics of the redirect | accepted |
| [0008](0008-access-event-model-and-data-minimisation.md) | Access event model and data minimisation | accepted |
| [0009](0009-traffic-classification-by-versioned-rules.md) | Traffic classification by versioned rules | accepted |
| [0010](0010-aggregation-and-retention.md) | Aggregation and retention | accepted |
| [0011](0011-api-first-openapi-contract.md) | API-first OpenAPI contract | accepted |
| [0012](0012-workspaces-api-keys-and-sessions.md) | Workspaces, API keys and sessions | accepted |
| [0013](0013-webhooks-following-standard-webhooks.md) | Webhooks following Standard Webhooks | accepted |
| [0014](0014-panel-rendered-by-the-server.md) | Panel rendered by the server | accepted |
| [0015](0015-single-repository-with-the-go-module-at-the-root.md) | Single repository with the Go module at the root | accepted |
| [0016](0016-dependencies-and-pinned-toolchains.md) | Dependencies and pinned toolchains | accepted |
| [0017](0017-github-flow-with-squash-merges-and-automated-releases.md) | GitHub Flow with squash merges and automated releases | accepted |
| [0018](0018-mit-licence.md) | MIT licence | accepted |
| [0019](0019-english-codebase-with-a-bilingual-panel.md) | English codebase with a bilingual panel | accepted |
| [0020](0020-published-figures-and-how-they-are-measured.md) | Published figures and how they are measured | accepted |

To add a record: copy the template to `NNNN-title-in-kebab-case.md` with the
next number, fill every section, add the row above, and merge it in the same
pull request as the first change that depends on it.
