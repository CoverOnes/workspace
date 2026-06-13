-- contract_audit_logs: append-only tamper-evident hash-chain per contract.
-- Each row records an event with a SHA-256 hash chaining prev_hash to detect tampering.
-- Retention: no time-bound TTL since audit logs are legal/compliance records.
-- No FK constraints per CLAUDE.md §9 — referential integrity enforced in service layer.

CREATE TABLE contract_audit_logs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id  uuid        NOT NULL,
    event_type   text        NOT NULL,
    actor_id     uuid        NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}',
    prev_hash    text        NOT NULL DEFAULT '',
    hash         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Fast lookup of audit entries by contract (primary query path).
CREATE INDEX contract_audit_logs_contract_id_idx
    ON contract_audit_logs (contract_id);

-- Ordered chain traversal: oldest-first per contract.
CREATE INDEX contract_audit_logs_contract_created_at_idx
    ON contract_audit_logs (contract_id, created_at ASC);
