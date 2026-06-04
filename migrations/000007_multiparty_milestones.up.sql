-- Phase 3: milestone model for multi-party contracts + poster_user_id on contracts.
-- No FK constraints per CLAUDE.md §9 — referential integrity enforced in service layer.

-- Extend multi_party_contracts with poster_user_id so the tender owner can be
-- identified without calling back to marketplace. Nullable for backward compatibility
-- with rows created before this migration (Phase 2 rows will have NULL).
ALTER TABLE multi_party_contracts
    ADD COLUMN IF NOT EXISTS poster_user_id uuid;

CREATE INDEX IF NOT EXISTS multi_party_contracts_poster_user_id_idx
    ON multi_party_contracts (poster_user_id)
    WHERE poster_user_id IS NOT NULL;

-- Milestones: each row represents one named payment checkpoint on a multi-party contract.
-- The amount carries the value that payment should disburse when the milestone is completed.
-- Milestone amounts are NOT required to sum to any contract total — the poster may add
-- milestones incrementally and the sum invariant is NOT enforced by this service.
-- (Documented decision: payment enforces its own independent check at settlement-plan creation.)
CREATE TABLE multiparty_milestones (
    id                uuid            PRIMARY KEY DEFAULT gen_random_uuid(),
    multi_contract_id uuid            NOT NULL,
    name              text            NOT NULL CHECK (char_length(name) BETWEEN 1 AND 255),
    amount            numeric(14,2)   NOT NULL CHECK (amount > 0),
    currency          char(3)         NOT NULL DEFAULT 'TWD',
    sequence          int             NOT NULL DEFAULT 0,
    status            text            NOT NULL DEFAULT 'PENDING'
                                      CHECK (status IN ('PENDING','COMPLETED')),
    completed_at      timestamptz,
    created_at        timestamptz     NOT NULL DEFAULT now(),
    updated_at        timestamptz     NOT NULL DEFAULT now()
);

-- Contract + sequence for ordered listing and uniqueness advisory.
-- No UNIQUE constraint on sequence — poster may reuse sequence numbers or leave gaps.
CREATE INDEX multiparty_milestones_contract_seq_idx
    ON multiparty_milestones (multi_contract_id, sequence);

CREATE INDEX multiparty_milestones_contract_id_idx
    ON multiparty_milestones (multi_contract_id);

CREATE INDEX multiparty_milestones_status_idx
    ON multiparty_milestones (multi_contract_id, status);
