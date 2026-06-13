-- contract_audit_logs: append-only hash-chain audit log per contract.
--
-- Security scope: this chain detects accidental or application-layer tampering —
-- reordering, deletion, payload modification, and partial writes. It does NOT prevent
-- a DB-privileged attacker who can recompute the entire chain (no HMAC key outside DB).
-- External anchoring or HMAC signatures are a separate hardening step (see GTD backlog).
--
-- No FK constraints per CLAUDE.md §9 — referential integrity enforced in service layer.
-- Retention: no time-bound TTL (audit logs are compliance records).

CREATE TABLE contract_audit_logs (
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id  uuid        NOT NULL,
    event_type   text        NOT NULL CHECK (char_length(event_type) BETWEEN 1 AND 100),
    actor_id     uuid        NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}',
    prev_hash    text        NOT NULL DEFAULT '',
    hash         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Ordered chain traversal: oldest-first per contract (seq is monotonic, collision-free).
-- This composite index also covers single-column contract_id lookups.
CREATE INDEX contract_audit_logs_contract_seq_idx
    ON contract_audit_logs (contract_id, seq ASC);

-- Defense-in-depth: DB-level uniqueness prevents a concurrent fork at the same
-- prev_hash position (genesis "" also only permitted once per contract_id).
CREATE UNIQUE INDEX contract_audit_logs_contract_prev_hash_unique
    ON contract_audit_logs (contract_id, prev_hash);
