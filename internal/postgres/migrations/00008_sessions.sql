-- +goose Up

-- A session of the panel: what a browser holds to act in a workspace
-- (ADR 0012). What the browser holds is a token, and the row a hash of it.
-- The row says nothing of whoever opened the session: no address and no
-- user-agent.
CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    -- The SHA-256 of the token, which is also what a session is found by:
    -- the token is all a browser sends. The token is stored nowhere.
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- When the session ends however much it is used.
    expires_at timestamptz NOT NULL,
    -- The last request of the session, to the minute and no finer: it is
    -- written at most once a minute, so that a page is a read of this row
    -- and not a write. A session without a request for its idle time has
    -- ended too.
    last_seen_at timestamptz NOT NULL DEFAULT now(),

    -- Two sessions with one token would be one session.
    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash),
    CONSTRAINT sessions_expiry CHECK (expires_at > created_at),
    CONSTRAINT sessions_last_seen CHECK (last_seen_at >= created_at)
);

-- The sessions of a workspace: what blocking it ends.
CREATE INDEX sessions_workspace_id_idx ON sessions (workspace_id);

-- A session that is closed is deleted at once. One that ended by itself
-- stays until the daily run, which says here how many it deleted.
ALTER TABLE retention_runs ADD COLUMN sessions bigint NOT NULL DEFAULT 0 CHECK (sessions >= 0);
