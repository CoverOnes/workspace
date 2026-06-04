DROP TABLE IF EXISTS multiparty_milestones;

ALTER TABLE multi_party_contracts
    DROP COLUMN IF EXISTS poster_user_id;
