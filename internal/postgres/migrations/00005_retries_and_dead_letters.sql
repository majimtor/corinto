-- +goose Up

-- The attempts of an event that could not be added to the aggregates
-- (docs/delivery-semantics.md, statement 5): how many have failed, and the
-- moment before which the event is not tried again. An event that never
-- failed has neither. The fifth failure takes the event out of this table.
ALTER TABLE event_inbox
    ADD COLUMN attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 4),
    ADD COLUMN next_attempt_at timestamptz,
    -- An event waits for an attempt because another one failed.
    ADD CONSTRAINT event_inbox_next_attempt CHECK (next_attempt_at IS NULL OR attempts > 0);

-- An event that failed on every attempt, set aside with the reason of the
-- last one. It left the inbox in the transaction that put it here, and the
-- operator can send it back. Its values were checked by the inbox, where
-- every row of this table comes from and goes back to.
CREATE TABLE event_dead_letters (
    id uuid PRIMARY KEY,
    link_id uuid NOT NULL REFERENCES links (id),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    occurred_at timestamptz(0) NOT NULL,
    outcome text NOT NULL,
    method text NOT NULL,
    referrer_host text,
    device text NOT NULL,
    browser text NOT NULL,
    class text NOT NULL,
    rule text NOT NULL,
    rules_version integer NOT NULL,
    accepted_at timestamptz NOT NULL,
    -- What the database answered to the last attempt: its code, and the
    -- constraint it names when it names one. Never its text, which quotes
    -- the row.
    reason_sqlstate text NOT NULL CHECK (length(reason_sqlstate) = 5),
    reason_constraint text CHECK (reason_constraint <> ''),
    dead_lettered_at timestamptz NOT NULL DEFAULT now()
);

-- What was dead-lettered is counted like what was processed, so that the
-- second equality of the reconciliation has all its terms:
-- accepted = processed + pending + dead-lettered.
ALTER TABLE pipeline_counters
    DROP CONSTRAINT pipeline_counters_name_check,
    DROP CONSTRAINT pipeline_counters_owner,
    ADD CONSTRAINT pipeline_counters_name_check
        CHECK (name IN ('served', 'accepted', 'processed', 'dead_lettered', 'not_found', 'rate_limited')),
    ADD CONSTRAINT pipeline_counters_owner
        CHECK ((name IN ('served', 'accepted', 'processed', 'dead_lettered')) = (workspace_id IS NOT NULL));
