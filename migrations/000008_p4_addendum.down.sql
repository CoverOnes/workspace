DROP TABLE IF EXISTS contract_addenda;

ALTER TABLE multi_party_contracts
    DROP CONSTRAINT IF EXISTS multi_party_contracts_status_check;

ALTER TABLE multi_party_contracts
    ADD CONSTRAINT multi_party_contracts_status_check
    CHECK (status IN ('DRAFT','PENDING_SIGNATURES','ACTIVE','COMPLETED','CANCELLED'));
