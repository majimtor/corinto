-- +goose Up

-- The time series of a link: how many events of an outcome and a traffic
-- class it had in an hour of UTC (PLAN.md section 5). The three metrics of
-- a link are sums over these rows.
CREATE TABLE link_hourly (
    link_id uuid NOT NULL REFERENCES links (id),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    -- The beginning of the hour, which in UTC is on the hour.
    hour timestamptz NOT NULL
        CHECK ((hour AT TIME ZONE 'UTC') = date_trunc('hour', hour AT TIME ZONE 'UTC')),
    -- The enumerations are code, not rows, repeated here as in event_inbox.
    outcome text NOT NULL CHECK (outcome IN ('redirected', 'inactive')),
    class text NOT NULL CHECK (
        class IN ('prefetch', 'automation', 'preview', 'crawler', 'presumed_human')
    ),
    events bigint NOT NULL CHECK (events > 0),

    PRIMARY KEY (link_id, hour, outcome, class)
);

-- The breakdowns of a link: how many estimated human clicks it had in a
-- UTC day, one dimension at a time and never crossed. The value of a
-- referrer host comes from the requester, so the code keeps at most 100 of
-- them per link and day; the rest are under '(other)', and the clicks that
-- named no referrer under '(none)'. No host has a parenthesis.
CREATE TABLE link_daily_dimension (
    link_id uuid NOT NULL REFERENCES links (id),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    day date NOT NULL,
    dimension text NOT NULL CHECK (dimension IN ('referrer_host', 'device', 'browser')),
    value text NOT NULL CHECK (value <> '' AND length(value) <= 253),
    events bigint NOT NULL CHECK (events > 0),

    PRIMARY KEY (link_id, day, dimension, value),
    CONSTRAINT link_daily_dimension_device CHECK (
        dimension <> 'device' OR value IN ('desktop', 'mobile', 'tablet', 'other')
    ),
    CONSTRAINT link_daily_dimension_browser CHECK (
        dimension <> 'browser'
        OR value IN ('chrome', 'safari', 'firefox', 'edge', 'opera', 'samsung_internet', 'other')
    )
);

-- The events that wait to be added to the aggregates, the oldest first:
-- what a consumer takes, and what says how far the metrics reach.
CREATE INDEX event_inbox_pending_idx ON event_inbox (accepted_at, id) WHERE processed_at IS NULL;

-- What was processed is counted like what was accepted, by the transaction
-- that processes it: the events are deleted when their time is up, and the
-- reconciliation of a day has to add up after that.
ALTER TABLE pipeline_counters
    DROP CONSTRAINT pipeline_counters_name_check,
    DROP CONSTRAINT pipeline_counters_owner,
    ADD CONSTRAINT pipeline_counters_name_check
        CHECK (name IN ('served', 'accepted', 'processed', 'not_found', 'rate_limited')),
    ADD CONSTRAINT pipeline_counters_owner
        CHECK ((name IN ('served', 'accepted', 'processed')) = (workspace_id IS NOT NULL));
