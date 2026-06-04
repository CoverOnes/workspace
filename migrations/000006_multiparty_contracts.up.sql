-- Phase 2: multi-party contract tables.
-- 1:1 dual-sign tables (contracts, contract_signatures) are UNTOUCHED.
-- No FK constraints per CLAUDE.md §9 — referential integrity in code.

CREATE TABLE multi_party_contracts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tender_id    uuid        NOT NULL,
    status       text        NOT NULL DEFAULT 'DRAFT'
                             CHECK (status IN ('DRAFT','PENDING_SIGNATURES','ACTIVE','COMPLETED','CANCELLED')),
    content_hash text        NOT NULL DEFAULT '',
    version      int         NOT NULL DEFAULT 1 CHECK (version >= 1),
    currency     char(3),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

-- One LIVE contract per tender (idempotent S2S create; concurrent creates collapse to one winner).
CREATE UNIQUE INDEX multi_party_contracts_tender_id_live_unique
    ON multi_party_contracts (tender_id)
    WHERE deleted_at IS NULL;

-- Tender lookup and status board.
CREATE INDEX multi_party_contracts_tender_id_idx   ON multi_party_contracts (tender_id);
CREATE INDEX multi_party_contracts_status_idx       ON multi_party_contracts (status, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX multi_party_contracts_created_at_idx   ON multi_party_contracts (created_at DESC) WHERE deleted_at IS NULL;


CREATE TABLE multi_party_contract_parties (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id    uuid        NOT NULL,
    vendor_user_id uuid        NOT NULL,
    role_id        uuid,           -- soft ref to a marketplace role; nullable (coordinator, etc.)
    share_bps      int         NOT NULL CHECK (share_bps >= 0 AND share_bps <= 10000),
    status         text        NOT NULL DEFAULT 'ACTIVE'
                               CHECK (status IN ('ACTIVE','EXITED','REPLACED')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- One ACTIVE party record per (contract, vendor) — prevents duplicate active party rows.
-- EXITED / REPLACED rows are kept for audit (no WHERE-clause filter on those).
CREATE UNIQUE INDEX multi_party_contract_parties_active_vendor_unique
    ON multi_party_contract_parties (contract_id, vendor_user_id)
    WHERE status = 'ACTIVE';

-- Fast lookups used by Σ-sum check, quorum check, and roster reads.
CREATE INDEX multi_party_contract_parties_contract_id_idx    ON multi_party_contract_parties (contract_id);
CREATE INDEX multi_party_contract_parties_vendor_user_id_idx ON multi_party_contract_parties (vendor_user_id);


CREATE TABLE multi_party_contract_signatures (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid        NOT NULL,
    signer_user_id      uuid        NOT NULL,
    version             int         NOT NULL CHECK (version >= 1),
    signed_content_hash text        NOT NULL,
    signed_at           timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- A party signs a given contract version exactly once (idempotent re-submit -> 23505 -> ErrAlreadySigned).
CREATE UNIQUE INDEX multi_party_contract_signatures_signer_version_unique
    ON multi_party_contract_signatures (contract_id, signer_user_id, version);

-- Fast quorum check: all signatures for a given (contract, version).
CREATE INDEX multi_party_contract_signatures_contract_version_idx
    ON multi_party_contract_signatures (contract_id, version);
CREATE INDEX multi_party_contract_signatures_signer_user_id_idx
    ON multi_party_contract_signatures (signer_user_id);
