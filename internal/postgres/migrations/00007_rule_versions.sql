-- +goose Up

-- The versions of the classification rules, and the hours of UTC between
-- which each classified events that were counted. The aggregates do not
-- say which version classified what they count: a response with metrics
-- takes from here the versions of its period, and the hour a version
-- began at is where a change of rules is marked on a series
-- (PLAN.md section 5).
--
-- A row per version of the rules, which is a few a year: nothing deletes
-- them. A consumer writes a row only when a batch reaches an hour that the
-- row does not cover, which is the first batch of each hour.
CREATE TABLE rule_versions (
    version integer PRIMARY KEY CHECK (version >= 1),
    -- The hours of the first and of the last event, by the moment of the
    -- access: the beginning of each, which in UTC is on the hour.
    first_hour timestamptz NOT NULL
        CHECK ((first_hour AT TIME ZONE 'UTC') = date_trunc('hour', first_hour AT TIME ZONE 'UTC')),
    last_hour timestamptz NOT NULL
        CHECK ((last_hour AT TIME ZONE 'UTC') = date_trunc('hour', last_hour AT TIME ZONE 'UTC')),

    CONSTRAINT rule_versions_hours CHECK (last_hour >= first_hour)
);
