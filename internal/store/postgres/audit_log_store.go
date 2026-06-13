package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditLogStore is a pool-backed store for contract audit logs.
// It is append-only: no Update or Delete methods are exposed.
type AuditLogStore struct {
	pool *pgxpool.Pool
}

// NewAuditLogStore returns an AuditLogStore backed by pool.
func NewAuditLogStore(pool *pgxpool.Pool) *AuditLogStore {
	return &AuditLogStore{pool: pool}
}

// Append acquires a tx-level advisory lock on contract_id, reads the chain tail,
// computes prev_hash and hash, then inserts the new entry — all inside a single
// transaction. This eliminates the TOCTOU window: there is no gap between reading
// the tail and inserting the new entry during which another goroutine could read
// the same tail and produce a forked chain.
//
// The UNIQUE (contract_id, prev_hash) constraint provides defense-in-depth: even if
// the advisory lock were somehow bypassed, two rows with the same prev_hash for the
// same contract cannot coexist.
func (s *AuditLogStore) Append(ctx context.Context, in *store.AuditAppendInput) (*domain.ContractAuditLog, error) {
	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin audit append tx: %w", err)
	}

	defer func() {
		// Rollback is a no-op when the tx was already committed.
		_ = tx.Rollback(ctx) // best-effort; original error takes precedence
	}()

	// Step 1: acquire tx-level advisory lock BEFORE reading the tail.
	// pg_advisory_xact_lock is released automatically when the tx ends.
	lockKey := contractIDLockKey(in.ContractID)
	if _, lockErr := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); lockErr != nil {
		return nil, fmt.Errorf("acquire advisory lock for contract %s: %w", in.ContractID, lockErr)
	}

	// Step 2: read the current tail (highest seq) while holding the lock.
	// Any concurrent Append for the same contract_id blocks on the advisory lock
	// until this tx commits, so this read is consistent.
	prevHash, err := tailHash(ctx, tx, in.ContractID)
	if err != nil {
		return nil, fmt.Errorf("read tail hash for contract %s: %w", in.ContractID, err)
	}

	// Step 3: compute the new entry's hash.
	hash, err := domain.AuditEntryDigest(prevHash, in.ContractID, in.EventType, in.ActorID, payload)
	if err != nil {
		return nil, fmt.Errorf("compute audit entry digest: %w", err)
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal audit payload: %w", err)
	}

	// Step 4: insert.
	entryID := uuid.New()
	createdAt := time.Now().UTC()

	const query = `
INSERT INTO contract_audit_logs
    (id, contract_id, event_type, actor_id, payload, prev_hash, hash, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING seq
`
	var seq int64

	if scanErr := tx.QueryRow(
		ctx, query,
		entryID, in.ContractID, in.EventType, in.ActorID,
		payloadBytes, prevHash, hash, createdAt,
	).Scan(&seq); scanErr != nil {
		return nil, fmt.Errorf("insert contract_audit_log: %w", scanErr)
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return nil, fmt.Errorf("commit audit append tx: %w", commitErr)
	}

	return &domain.ContractAuditLog{
		Seq:        seq,
		ID:         entryID,
		ContractID: in.ContractID,
		EventType:  in.EventType,
		ActorID:    in.ActorID,
		Payload:    payload,
		PrevHash:   prevHash,
		Hash:       hash,
		CreatedAt:  createdAt,
	}, nil
}

// ListByContract returns all audit log entries for a contract ordered by seq ASC.
// seq is a BIGINT GENERATED ALWAYS AS IDENTITY column — monotonically increasing,
// collision-free even when multiple rows share the same created_at microsecond.
func (s *AuditLogStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAuditLog, error) {
	const query = `
SELECT seq, id, contract_id, event_type, actor_id, payload, prev_hash, hash, created_at
FROM contract_audit_logs
WHERE contract_id = $1
ORDER BY seq ASC
`

	rows, err := s.pool.Query(ctx, query, contractID)
	if err != nil {
		return nil, fmt.Errorf("list contract_audit_logs: %w", err)
	}

	defer rows.Close()

	var entries []*domain.ContractAuditLog

	for rows.Next() {
		var (
			e           domain.ContractAuditLog
			payloadJSON []byte
		)

		if scanErr := rows.Scan(
			&e.Seq, &e.ID, &e.ContractID, &e.EventType, &e.ActorID,
			&payloadJSON, &e.PrevHash, &e.Hash, &e.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan contract_audit_log: %w", scanErr)
		}

		if unmarshalErr := json.Unmarshal(payloadJSON, &e.Payload); unmarshalErr != nil {
			return nil, fmt.Errorf("unmarshal audit payload for entry %s: %w", e.ID, unmarshalErr)
		}

		entries = append(entries, &e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contract_audit_logs: %w", err)
	}

	return entries, nil
}

// tailHash returns the hash of the highest-seq entry for contractID, or "" if none.
// MUST be called while holding the advisory lock for contractID.
func tailHash(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) (string, error) {
	const q = `
SELECT hash
FROM contract_audit_logs
WHERE contract_id = $1
ORDER BY seq DESC
LIMIT 1
`

	var h string

	err := tx.QueryRow(ctx, q, contractID).Scan(&h)
	if err != nil {
		if err == pgx.ErrNoRows { //nolint:errorlint // pgx.ErrNoRows is not wrapped; direct comparison is idiomatic
			return "", nil // no entries yet — genesis
		}

		return "", fmt.Errorf("query tail hash: %w", err)
	}

	return h, nil
}

// contractIDLockKey derives a stable int64 advisory-lock key from a contract UUID.
// FNV-64a is used for speed and uniform distribution. Hash collisions are safe: a
// spurious wait is the only consequence and collision probability is negligible.
func contractIDLockKey(contractID uuid.UUID) int64 {
	h := fnv.New64a()
	h.Write(contractID[:])

	// uint64→int64 reinterpretation: bit pattern preserved; Postgres treats bigint
	// as signed 64-bit. G115 is excluded in .golangci.yml.
	return int64(h.Sum64()) // uint64→int64 reinterpretation for advisory lock key; bit pattern preserved (G115 excluded project-wide)
}
