-- Add optional file attachment reference to contract_signatures.
-- nullable: most signatures will not have an attachment (file_id IS NULL).
-- NO FK constraint per project policy — referential integrity enforced in service layer.
ALTER TABLE contract_signatures ADD COLUMN file_id uuid;

-- Advisory index: queries for "which signatures have an attached file" will be infrequent,
-- but a partial index keeps maintenance cost near-zero on the (vast) NULL majority.
CREATE INDEX contract_signatures_file_id_idx
    ON contract_signatures (file_id)
    WHERE file_id IS NOT NULL;
