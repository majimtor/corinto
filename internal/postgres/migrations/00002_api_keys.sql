-- +goose Up

-- A quota of the workspace, like max_links: the keys it holds that are not
-- revoked. The workspaces that already exist take the initial value of
-- PLAN.md section 9. The default is then dropped: whoever creates a
-- workspace gives the value, so that defaults have one source.
ALTER TABLE workspaces ADD COLUMN max_keys integer NOT NULL DEFAULT 10 CHECK (max_keys >= 0);
ALTER TABLE workspaces ALTER COLUMN max_keys DROP DEFAULT;

-- An API key: what a system sends to act in a workspace. What it sends is
-- the identifier of this row and a secret.
CREATE TABLE api_keys (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    name text NOT NULL CHECK (name <> ''),
    -- The SHA-256 of the secret. The secret is shown once, when the key is
    -- issued, and is stored nowhere.
    secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    -- The scopes are code, not rows. The list is repeated here so that the
    -- database refuses what the code would not write.
    scopes text[] NOT NULL CHECK (
        cardinality(scopes) >= 1
        AND scopes <@ ARRAY['links:read', 'links:write', 'metrics:read', 'webhooks:read', 'webhooks:write']
    ),
    expires_at timestamptz,
    -- The day, in UTC, of the last request the key authenticated, and no
    -- finer: it tells a key in use from a forgotten one at the cost of one
    -- write a day.
    last_used_on date,
    -- Revoking a key does not delete its row: its name and its last use are
    -- what is asked for when something has gone wrong.
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- A key that had expired before it was issued could never be used.
    CONSTRAINT api_keys_expiry CHECK (expires_at > created_at)
);

-- The keys that count against the quota of a workspace.
CREATE INDEX api_keys_workspace_id_idx ON api_keys (workspace_id) WHERE revoked_at IS NULL;
