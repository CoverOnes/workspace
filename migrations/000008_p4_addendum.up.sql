-- Phase 4: addendum + re-sign quorum flow for multi-party contracts.
-- Model B: late joiner added at 0 bps → ADDENDUM_PENDING → owner patches shares
-- (Σ=10000) → re-submit → all ACTIVE parties re-sign the new digest → ACTIVE.
-- No FK constraints per CLAUDE.md §9 — referential integrity enforced in service layer.

-- Extend status CHECK on multi_party_contracts to include ADDENDUM_PENDING.
ALTER TABLE multi_party_contracts
    DROP CONSTRAINT IF EXISTS multi_party_contracts_status_check;

ALTER TABLE multi_party_contracts
    ADD CONSTRAINT multi_party_contracts_status_check
    CHECK (status IN ('DRAFT','PENDING_SIGNATURES','ACTIVE','ADDENDUM_PENDING','COMPLETED','CANCELLED'));

-- contract_addenda records each addendum event: which party was added, by whom,
-- and which version transition it represents.
-- No FK constraints — contract_id and party IDs are soft references.
CREATE TABLE contract_addenda (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid        NOT NULL,
    from_version        int         NOT NULL CHECK (from_version >= 1),
    to_version          int         NOT NULL CHECK (to_version > from_version),
    new_party_id        uuid        NOT NULL,
    new_vendor_user_id  uuid        NOT NULL,
    triggered_by        uuid        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Fast lookup of addenda by contract.
CREATE INDEX contract_addenda_contract_id_idx
    ON contract_addenda (contract_id);

-- Ordered listing: newest addendum first per contract.
CREATE INDEX contract_addenda_contract_created_at_idx
    ON contract_addenda (contract_id, created_at DESC);
