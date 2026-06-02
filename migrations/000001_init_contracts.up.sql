CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE contracts (
    id                 uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id         uuid          NOT NULL,
    accepted_bid_id    uuid          NOT NULL,
    client_user_id     uuid          NOT NULL,
    freelancer_user_id uuid          NOT NULL,
    title              text          NOT NULL,
    terms              text          NOT NULL DEFAULT '',
    amount             numeric(14,2) NOT NULL,
    currency           text          NOT NULL DEFAULT 'TWD' CHECK (char_length(currency) = 3),
    content_hash       text          NOT NULL,
    version            integer       NOT NULL DEFAULT 1 CHECK (version >= 1),
    status             text          NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','PENDING_SIGNATURE','SIGNED','ACTIVE','COMPLETED','CANCELLED')),
    activated_at       timestamptz,
    completed_at       timestamptz,
    deleted_at         timestamptz,
    created_at         timestamptz   NOT NULL DEFAULT now(),
    updated_at         timestamptz   NOT NULL DEFAULT now()
);

-- One contract per awarded deal (idempotent create from a single accepted bid).
-- Concurrent create attempts collapse to one winner (23505 -> 409 CONFLICT).
CREATE UNIQUE INDEX contracts_accepted_bid_id_unique ON contracts (accepted_bid_id);

-- Party dashboards: contracts where I am client / freelancer (live rows only).
CREATE INDEX contracts_client_user_id_idx ON contracts (client_user_id) WHERE deleted_at IS NULL;
CREATE INDEX contracts_freelancer_user_id_idx ON contracts (freelancer_user_id) WHERE deleted_at IS NULL;
-- Existence / cross-service lookup by originating listing.
CREATE INDEX contracts_listing_id_idx ON contracts (listing_id) WHERE deleted_at IS NULL;
-- Status board ordered newest-first.
CREATE INDEX contracts_status_created_at_idx ON contracts (status, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX contracts_created_at_idx ON contracts (created_at DESC) WHERE deleted_at IS NULL;
