-- Backstop dual-sign integrity at the DB level.
--
-- Each contract version must have at most ONE signature per signer_role
-- (one CLIENT signature and one FREELANCER signature). The dual-sign
-- completion logic in SignContract counts DISTINCT signer_role; this constraint
-- guarantees that count can never be inflated by two rows carrying the same role
-- for the same (contract_id, contract_version) even under a race or a buggy code
-- path. Combined with the existing (contract_id, signer_user_id, contract_version)
-- unique index, a contract can reach >=2 valid signatures only when two DISTINCT
-- users each occupy a DISTINCT role.
--
-- No FK by project policy (CLAUDE.md #9): referential integrity to contracts is
-- enforced in the service layer (deriveSignerRole rejects non-parties).
CREATE UNIQUE INDEX contract_signatures_role_version_unique
    ON contract_signatures (contract_id, signer_role, contract_version);
