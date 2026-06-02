CREATE TABLE worklogs (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id uuid        NOT NULL,
    user_id     uuid        NOT NULL,
    description text        NOT NULL DEFAULT '',
    minutes     integer     NOT NULL CHECK (minutes > 0 AND minutes <= 1440),
    logged_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Worklog timeline for a contract, newest first (live rows only).
CREATE INDEX worklogs_contract_id_logged_at_idx ON worklogs (contract_id, logged_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX worklogs_user_id_idx ON worklogs (user_id) WHERE deleted_at IS NULL;
CREATE INDEX worklogs_created_at_idx ON worklogs (created_at DESC) WHERE deleted_at IS NULL;
