DROP INDEX IF EXISTS contract_signatures_file_id_idx;
ALTER TABLE contract_signatures DROP COLUMN IF EXISTS file_id;
