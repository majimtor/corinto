-- +goose Up

-- A run of the job that deletes what has been kept for its time
-- (PLAN.md section 5): when it began, when it ended and what it deleted,
-- table by table. The last row that has an end says when the job last ran.
CREATE TABLE retention_runs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    started_at timestamptz NOT NULL DEFAULT now(),
    -- Empty while the run is on its way, and for ever when its process
    -- stopped before the end. What such a run deleted is still here.
    finished_at timestamptz CHECK (finished_at >= started_at),
    inbox_events bigint NOT NULL DEFAULT 0 CHECK (inbox_events >= 0),
    dead_letters bigint NOT NULL DEFAULT 0 CHECK (dead_letters >= 0),
    hourly_rows bigint NOT NULL DEFAULT 0 CHECK (hourly_rows >= 0),
    daily_rows bigint NOT NULL DEFAULT 0 CHECK (daily_rows >= 0),
    counters bigint NOT NULL DEFAULT 0 CHECK (counters >= 0)
);

-- The events that were processed, the oldest first: what a run deletes.
-- With the index of the events that wait, every event is in one of the two
-- and in no more.
CREATE INDEX event_inbox_processed_idx ON event_inbox (accepted_at) WHERE processed_at IS NOT NULL;
