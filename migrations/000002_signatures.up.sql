CREATE TABLE contract_signatures (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid        NOT NULL,
    signer_user_id      uuid        NOT NULL,
    signer_role         text        NOT NULL CHECK (signer_role IN ('CLIENT','FREELANCER')),
    contract_version    integer     NOT NULL CHECK (contract_version >= 1),
    signed_content_hash text        NOT NULL,
    signer_ip           inet,
    user_agent          text,
    signed_at           timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- A party may sign a given contract version at most once (re-sign is idempotent -> 23505 swallowed/409).
CREATE UNIQUE INDEX contract_signatures_party_version_unique
    ON contract_signatures (contract_id, signer_user_id, contract_version);
-- Fast "all signatures for this contract" lookup to evaluate dual-sign completion.
CREATE INDEX contract_signatures_contract_id_idx ON contract_signatures (contract_id);
-- Audit by signer.
CREATE INDEX contract_signatures_signer_user_id_idx ON contract_signatures (signer_user_id);
CREATE INDEX contract_signatures_signed_at_idx ON contract_signatures (signed_at DESC);
