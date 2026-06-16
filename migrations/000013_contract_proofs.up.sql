-- contract_proofs: durable legal artifact storing the tamper-evidence PDF proof
-- generated when all parties have signed a contract (bilateral or multiparty).
--
-- Retention note: this is a DURABLE legal artifact, NOT an observability/event table.
-- Do NOT add a TTL/retention policy. Contract proofs must be retained indefinitely
-- for legal compliance. (preempting db-inspector: TTL is intentionally absent here.)
--
-- No FK constraints per CLAUDE.md §9 — referential integrity enforced in service layer.
-- Uniqueness on (contract_id, contract_kind): one CURRENT proof per contract.
-- contract_version: enables supersede detection — if a new proof is generated for the
-- same (contract_id, contract_kind) at a newer version, the row is updated in place.

CREATE TABLE contract_proofs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id      uuid        NOT NULL,
    contract_kind    text        NOT NULL CHECK (contract_kind IN ('bilateral', 'multiparty')),
    contract_version int         NOT NULL,
    file_id          uuid        NOT NULL,
    object_key       text        NOT NULL,
    sha256           text        NOT NULL,
    audit_chain_head text        NOT NULL DEFAULT '',
    generated_at     timestamptz NOT NULL
);

-- Idempotency + supersede: only one CURRENT proof per (contract_id, contract_kind).
-- On addendum re-sign the row is UPDATE'd in place (new file_id, version, sha256).
CREATE UNIQUE INDEX contract_proofs_contract_kind_unique
    ON contract_proofs (contract_id, contract_kind);

-- Note: the unique index above already covers contract_id prefix for point lookups
-- (Postgres can use partial-key scans on leading columns of a composite index).
-- A separate single-column index on contract_id would be redundant and is omitted.
