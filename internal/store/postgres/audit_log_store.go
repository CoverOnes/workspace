package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/google/uuid"
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

// Append inserts a new audit log entry inside a transaction with a tx-level advisory
// lock keyed on the contract_id. The advisory lock serializes concurrent appends for the
// same contract so that the hash chain is never forked.
//
// The caller MUST pre-compute entry.PrevHash and entry.Hash using domain.AuditEntryDigest.
func (s *AuditLogStore) Append(ctx context.Context, entry *domain.ContractAuditLog) error {
	payloadBytes, err := json.Marshal(entry.Payload)
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin audit append tx: %w", err)
	}

	defer func() {
		// Rollback is a no-op when the tx was already committed.
		_ = tx.Rollback(ctx) // rollback on error path is best-effort; original error takes precedence
	}()

	// Acquire a tx-level advisory lock keyed on a stable int64 derived from the
	// contract_id. pg_advisory_xact_lock is automatically released when the
	// transaction commits or rolls back — no manual unlock needed.
	lockKey := contractIDLockKey(entry.ContractID)
	if _, lockErr := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); lockErr != nil {
		return fmt.Errorf("acquire advisory lock for contract %s: %w", entry.ContractID, lockErr)
	}

	const query = `
INSERT INTO contract_audit_logs
    (id, contract_id, event_type, actor_id, payload, prev_hash, hash, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
`
	if _, execErr := tx.Exec(
		ctx, query,
		entry.ID, entry.ContractID, entry.EventType, entry.ActorID,
		payloadBytes, entry.PrevHash, entry.Hash, entry.CreatedAt,
	); execErr != nil {
		return fmt.Errorf("insert contract_audit_log: %w", execErr)
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit audit append tx: %w", commitErr)
	}

	return nil
}

// ListByContract returns all audit log entries for a contract ordered by created_at ASC.
func (s *AuditLogStore) ListByContract(ctx context.Context, contractID uuid.UUID) ([]*domain.ContractAuditLog, error) {
	const query = `
SELECT id, contract_id, event_type, actor_id, payload, prev_hash, hash, created_at
FROM contract_audit_logs
WHERE contract_id = $1
ORDER BY created_at ASC
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
			&e.ID, &e.ContractID, &e.EventType, &e.ActorID,
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

// contractIDLockKey derives a stable int64 advisory-lock key from a contract UUID.
// We use FNV-64a for speed and uniform distribution. Two different UUIDs may
// theoretically produce the same key (hash collision), which would cause a spurious
// wait — this is safe because the lock is still released when the tx ends, and the
// probability of collision is negligible for the expected number of contracts.
func contractIDLockKey(contractID uuid.UUID) int64 {
	h := fnv.New64a()
	h.Write(contractID[:])

	// Safe: converting uint64 to int64 for pg_advisory_xact_lock($1::bigint).
	// The bit pattern is preserved; Postgres treats bigint as signed 64-bit.
	return int64(h.Sum64()) // deliberate uint64->int64 reinterpretation for advisory lock key; bit pattern preserved
}
