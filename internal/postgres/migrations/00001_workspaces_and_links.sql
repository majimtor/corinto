-- +goose Up

-- A workspace is the root of ownership and isolation: every other row
-- belongs to exactly one, and no query crosses workspaces.
CREATE TABLE workspaces (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    name text NOT NULL CHECK (name <> ''),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'blocked')),
    demo boolean NOT NULL DEFAULT false,
    may_choose_aliases boolean NOT NULL DEFAULT false,
    -- The destination policy: the entries a destination must match. An empty
    -- list allows any destination.
    allowed_destinations jsonb NOT NULL DEFAULT '[]'
        CHECK (jsonb_typeof(allowed_destinations) = 'array'),
    -- A quota of the workspace. Whoever creates it gives the value; there is
    -- no default here, so that defaults have one source.
    max_links integer NOT NULL CHECK (max_links >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE links (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL REFERENCES workspaces (id),
    domain text NOT NULL CHECK (domain <> ''),
    identifier text NOT NULL CHECK (identifier <> ''),
    -- Expiry is not a state: it is derived from expires_at on every request.
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'disabled', 'blocked', 'deleted')),
    destination text,
    -- What the redirect serves: the destination with the campaign
    -- parameters appended, computed when the link is written.
    resolved_destination text,
    utm_source text,
    utm_medium text,
    utm_campaign text,
    utm_term text,
    utm_content text,
    title text,
    tags text[] NOT NULL DEFAULT '{}',
    expires_at timestamptz,
    -- Incremented by every change. The replica applies a change only when
    -- its version is newer than the one it holds.
    version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Unique for ever. A deleted link keeps its row, so its identifier is
    -- never issued again: a link handed out lives in documents nobody
    -- controls.
    CONSTRAINT links_domain_identifier_key UNIQUE (domain, identifier),

    -- A deleted link is a tombstone: it keeps its identifier and nothing of
    -- what it led to. A link that is not deleted always has a destination.
    CONSTRAINT links_tombstone CHECK (
        CASE WHEN state = 'deleted' THEN
            destination IS NULL AND resolved_destination IS NULL
            AND utm_source IS NULL AND utm_medium IS NULL AND utm_campaign IS NULL
            AND utm_term IS NULL AND utm_content IS NULL
            AND title IS NULL AND tags = '{}' AND expires_at IS NULL
        ELSE
            destination IS NOT NULL AND resolved_destination IS NOT NULL
        END
    )
);

-- What the replica of the redirect asks for: the links changed since a mark.
CREATE INDEX links_updated_at_idx ON links (updated_at, id);

-- The links of a workspace, in a stable order.
CREATE INDEX links_workspace_id_idx ON links (workspace_id, id);
