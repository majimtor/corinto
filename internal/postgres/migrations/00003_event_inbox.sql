-- +goose Up

-- An access event, from the moment it is accepted: the response that owed
-- it was served, and from here on the event is durable
-- (docs/delivery-semantics.md). It carries what PLAN.md section 5 lists,
-- and nothing that identifies a visitor.
CREATE TABLE event_inbox (
    -- Given by the process that served the response, before the first
    -- attempt to insert the event: a batch written twice conflicts with
    -- itself. There is no default here.
    id uuid PRIMARY KEY,
    -- A deleted link keeps its row, so an event never loses its link.
    link_id uuid NOT NULL REFERENCES links (id),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    -- The time of the access by the clock of the process that served it, to
    -- the second: the column holds no finer time.
    occurred_at timestamptz(0) NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('redirected', 'inactive')),
    method text NOT NULL CHECK (method IN ('GET', 'HEAD')),
    -- The host of the page the request says it came from, when it names one
    -- that is a domain name. Never the whole address of that page.
    referrer_host text CHECK (referrer_host <> '' AND length(referrer_host) <= 253),
    -- The enumerations are code, not rows. They are repeated here so that
    -- the database refuses what the code would not write.
    device text NOT NULL CHECK (device IN ('desktop', 'mobile', 'tablet', 'other')),
    browser text NOT NULL CHECK (
        browser IN ('chrome', 'safari', 'firefox', 'edge', 'opera', 'samsung_internet', 'other')
    ),
    class text NOT NULL CHECK (
        class IN ('prefetch', 'automation', 'preview', 'crawler', 'presumed_human')
    ),
    -- The rule that gave the class, and the version of the rules it is of.
    rule text NOT NULL CHECK (rule <> ''),
    rules_version integer NOT NULL CHECK (rules_version >= 1),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    -- Empty while the event waits to be added to the aggregates.
    processed_at timestamptz
);

-- The counts along the path of an event, per UTC day of the access: the
-- terms that the reconciliation of docs/delivery-semantics.md adds up. Each
-- is counted where the thing happens and written as an increment, so that
-- several processes add to one row.
CREATE TABLE pipeline_counters (
    day date NOT NULL,
    -- Empty for what is of no workspace: a request that named no link, or
    -- one over the rate limit.
    workspace_id uuid REFERENCES workspaces (id),
    name text NOT NULL CHECK (name IN ('served', 'accepted', 'not_found', 'rate_limited')),
    value bigint NOT NULL CHECK (value >= 0),

    CONSTRAINT pipeline_counters_key UNIQUE NULLS NOT DISTINCT (day, workspace_id, name),
    -- What is served and what is accepted is of a link, and so of a
    -- workspace; the rest is of none.
    CONSTRAINT pipeline_counters_owner CHECK ((name IN ('served', 'accepted')) = (workspace_id IS NOT NULL))
);
