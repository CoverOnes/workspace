CREATE TABLE tasks (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id      uuid        NOT NULL,
    title            text        NOT NULL,
    status           text        NOT NULL DEFAULT 'TODO' CHECK (status IN ('TODO','DOING','DONE')),
    assignee_user_id uuid,
    deleted_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Task board for a contract, status-grouped, newest-first (live rows only).
CREATE INDEX tasks_contract_id_status_idx ON tasks (contract_id, status, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX tasks_assignee_user_id_idx ON tasks (assignee_user_id) WHERE deleted_at IS NULL;
CREATE INDEX tasks_created_at_idx ON tasks (created_at DESC) WHERE deleted_at IS NULL;
