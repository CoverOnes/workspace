package service_test

// proof_outbox_handler_test.go — tests for HandleProofOutboxEntry.
//
// Unit-test coverage (all branches):
//   1. Malformed JSON payload → return nil (drop, programmer error)
//   2. Nil ContractID in valid JSON → return nil (drop, programmer error)
//   3. Unknown contract kind → return nil (drop, programmer error)
//   4. Valid payload but GenerateAndStore transient failure → return non-nil error (retry)
//   5. Valid payload, GenerateAndStore succeeds → return nil
//
// Full-chain integration test (Finding 8, uses sharedServicePool):
//   SignContract → outbox entry enqueued → poller dispatches via local handler → proof row stored

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/events"
	"github.com/CoverOnes/workspace/internal/outbox"
	"github.com/CoverOnes/workspace/internal/service"
	pgstore "github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- unit tests for HandleProofOutboxEntry ----

// makeOutboxEntry constructs a minimal OutboxEntry with the given payload bytes.
func makeOutboxEntry(payload []byte) *domain.OutboxEntry {
	return &domain.OutboxEntry{
		ID:      uuid.New(),
		Payload: payload,
	}
}

// makeProofPayload encodes a ProofGenerationPayload as JSON bytes.
func makeProofPayload(t *testing.T, contractID uuid.UUID, kind string) []byte {
	t.Helper()

	b, err := json.Marshal(service.ProofGenerationPayload{
		ContractID: contractID,
		Kind:       kind,
	})
	require.NoError(t, err)

	return b
}

// TestHandleProofOutboxEntry_MalformedJSON_DropsEntry verifies that a malformed JSON
// payload causes the handler to return nil (mark published / drop) rather than returning
// an error that would trigger indefinite retries.
func TestHandleProofOutboxEntry_MalformedJSON_DropsEntry(t *testing.T) {
	t.Parallel()

	entry := makeOutboxEntry([]byte(`not-valid-json`))

	// ProofService can be nil — malformed JSON must be caught before it is used.
	err := service.HandleProofOutboxEntry(context.Background(), nil, entry)

	assert.NoError(t, err, "malformed payload is a programmer error; handler must return nil to drop it")
}

// TestHandleProofOutboxEntry_NilContractID_DropsEntry verifies that a zero-value (nil)
// ContractID causes the handler to return nil rather than calling GenerateAndStore with
// a meaningless UUID.
func TestHandleProofOutboxEntry_NilContractID_DropsEntry(t *testing.T) {
	t.Parallel()

	payload := makeProofPayload(t, uuid.Nil, "bilateral")
	entry := makeOutboxEntry(payload)

	err := service.HandleProofOutboxEntry(context.Background(), nil, entry)

	assert.NoError(t, err, "nil ContractID is a programmer error; handler must return nil to drop it")
}

// TestHandleProofOutboxEntry_UnknownKind_DropsEntry verifies that an unrecognized
// contract kind (not "bilateral" or "multiparty") is treated as a programmer error and
// dropped without retrying.
func TestHandleProofOutboxEntry_UnknownKind_DropsEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind string
	}{
		{"empty string", ""},
		{"unknown value", "trilateral"},
		{"uppercase", "Bilateral"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := makeProofPayload(t, uuid.New(), tc.kind)
			entry := makeOutboxEntry(payload)

			err := service.HandleProofOutboxEntry(context.Background(), nil, entry)

			assert.NoError(t, err,
				"unknown kind %q is a programmer error; handler must return nil to drop it", tc.kind)
		})
	}
}

// TestHandleProofOutboxEntry_TransientError_ReturnsError verifies that a transient
// error from GenerateAndStore (e.g. network/DB unavailability) is propagated as a
// non-nil error so the outbox poller retries with exponential backoff.
//
// This test uses a real ProofService wired to a fakeFileClient but with a stubbed
// store that always returns an error for GenerateAndStore's DB writes.
func TestHandleProofOutboxEntry_TransientError_ReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping proof handler transient-error test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	ctx := context.Background()
	env := newProofTestEnv(t)

	// Create a valid ACTIVE contract to produce a realistic ContractID.
	contract, _ := createActiveBilateralContract(t, env)

	// Build a ProofService wired to an error-returning stub store.
	errStore := &alwaysErrProofStore{err: errors.New("simulated DB unavailability")}

	fakeFC := newFakeFileClient()

	svc, err := service.NewProofService(&service.ProofServiceConfig{
		ProofStore:               errStore,
		AuditStore:               pgstore.NewAuditLogStore(sharedServicePool),
		ContractStore:            pgstore.NewContractStore(sharedServicePool),
		SignatureStore:           pgstore.NewSignatureStore(sharedServicePool),
		MultipartyContractStore:  pgstore.NewMultipartyContractStore(sharedServicePool),
		MultipartyPartyStore:     pgstore.NewMultipartyPartyStore(sharedServicePool),
		MultipartySignatureStore: pgstore.NewMultipartySignatureStore(sharedServicePool),
		FileClient:               fakeFC,
	})
	require.NoError(t, err)

	payload := makeProofPayload(t, contract.ID, string(domain.ContractKindBilateral))
	entry := makeOutboxEntry(payload)

	handlerErr := service.HandleProofOutboxEntry(ctx, svc, entry)

	require.Error(t, handlerErr,
		"transient GenerateAndStore error must be propagated so the poller retries")
	assert.Contains(t, handlerErr.Error(), "generate and store")
}

// TestHandleProofOutboxEntry_Success verifies the happy path: a valid payload with
// an ACTIVE bilateral contract causes GenerateAndStore to run and the handler to
// return nil (so the poller marks the entry published).
func TestHandleProofOutboxEntry_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping proof handler success test in short mode")
	}

	ctx := context.Background()
	env := newProofTestEnv(t)

	contract, _ := createActiveBilateralContract(t, env)

	payload := makeProofPayload(t, contract.ID, string(domain.ContractKindBilateral))
	entry := makeOutboxEntry(payload)

	err := service.HandleProofOutboxEntry(ctx, env.proofSvc, entry)

	require.NoError(t, err, "valid payload + ACTIVE contract must succeed")
}

// ---- full-chain integration test ----

// TestSignContract_TriggerOutbox_ProofStored verifies the full chain:
//
//	SignContract → workspace.proof_generation_required outbox entry enqueued
//	→ outbox poller dispatches via local handler → proof row stored in DB.
//
// Uses the sharedServicePool testcontainer; requires Docker (not short mode).
func TestSignContract_TriggerOutbox_ProofStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full-chain proof integration test in short mode")
	}

	require.NotNil(t, sharedServicePool, "sharedServicePool must be initialized by TestMain")

	ctx := context.Background()
	pool := sharedServicePool

	// Store layer.
	contractStore := pgstore.NewContractStore(pool)
	sigStore := pgstore.NewSignatureStore(pool)
	txMgr := pgstore.NewTxManager(pool)
	outboxStore := pgstore.NewOutboxStore(pool)
	auditLogStore := pgstore.NewAuditLogStore(pool)
	proofStore := pgstore.NewContractProofStore(pool)

	// Service layer — proofEnabled=true so SignContract enqueues the outbox entry.
	pub := events.NewNoopPublisher()
	contractSvc := service.NewContractService(contractStore, sigStore, txMgr, pub, nil, true)

	fakeFC := newFakeFileClient()

	proofSvc, err := service.NewProofService(&service.ProofServiceConfig{
		ProofStore:               proofStore,
		AuditStore:               auditLogStore,
		ContractStore:            contractStore,
		SignatureStore:           sigStore,
		MultipartyContractStore:  pgstore.NewMultipartyContractStore(pool),
		MultipartyPartyStore:     pgstore.NewMultipartyPartyStore(pool),
		MultipartySignatureStore: pgstore.NewMultipartySignatureStore(pool),
		FileClient:               fakeFC,
	})
	require.NoError(t, err)

	// Create and fully sign a bilateral contract (produces the proof outbox entry).
	clientID := uuid.New()
	freelancerID := uuid.New()

	c, err := contractSvc.CreateContract(ctx, &service.CreateContractInput{
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Amount:           decimal.RequireFromString("500.00"),
		Currency:         testCurrencyTWD,
		Title:            fmt.Sprintf("FullChain Proof Test %s", uuid.New()),
		Terms:            "Full chain integration test terms.",
	})
	require.NoError(t, err)

	c, err = contractSvc.SubmitContract(ctx, c.ID, clientID)
	require.NoError(t, err)

	_, err = contractSvc.SignContract(ctx, service.SignContractInput{
		ContractID:        c.ID,
		CallerID:          clientID,
		SignedContentHash: c.ContentHash,
	})
	require.NoError(t, err)

	activeResult, err := contractSvc.SignContract(ctx, service.SignContractInput{
		ContractID:        c.ID,
		CallerID:          freelancerID,
		SignedContentHash: c.ContentHash,
	})
	require.NoError(t, err)
	require.Equal(t, domain.ContractStatusActive, activeResult.Status)

	contractID := activeResult.ID

	// Verify the outbox entry was enqueued using a non-claiming SELECT.
	// We intentionally do NOT call outboxStore.FetchPending here because that
	// method claims the row for 30s (claimed_until = now()+30s), which would
	// prevent the poller started below from picking it up within the 10s deadline.
	var proofEntryExists bool

	checkRow := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM event_outbox
			WHERE channel = $1
			  AND published_at IS NULL
			  AND convert_from(payload, 'UTF8')::jsonb @> jsonb_build_object('contractId', $2::text)
		)`, service.ChannelProofGenerationRequired, contractID.String())

	require.NoError(t, checkRow.Scan(&proofEntryExists))
	require.True(t, proofEntryExists,
		"workspace.proof_generation_required outbox entry must be enqueued after both parties sign")

	// Wire up the outbox poller with the local proof handler.
	// Use a NoopPublisher so only the local handler path is exercised.
	noop := &outbox.NoopPublisher{}
	poller := outbox.New(outboxStore, noop)
	poller.Handle(service.ChannelProofGenerationRequired, func(handlerCtx context.Context, e *domain.OutboxEntry) error {
		return service.HandleProofOutboxEntry(handlerCtx, proofSvc, e)
	})

	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		poller.Start(pollCtx)
	}()

	defer func() {
		cancel()
		<-done
	}()

	// Wait for the proof row to appear in the DB (up to 10s).
	deadline := time.Now().Add(10 * time.Second)

	var proof *domain.ContractProof

	for time.Now().Before(deadline) {
		proof, err = proofStore.GetByContract(ctx, contractID, domain.ContractKindBilateral)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	require.NoError(t, err, "proof row must be stored in DB after poller dispatches local handler")
	require.NotNil(t, proof)
	assert.Equal(t, contractID, proof.ContractID)
	assert.Equal(t, domain.ContractKindBilateral, proof.ContractKind)
	assert.NotEmpty(t, proof.SHA256)
	assert.NotEqual(t, uuid.Nil, proof.FileID)
}

// ---- test helpers ----

// alwaysErrProofStore is a ContractProofStore stub that always returns an error
// on write operations. Used to simulate transient DB failures in handler tests.
type alwaysErrProofStore struct {
	err error
}

func (s *alwaysErrProofStore) Create(_ context.Context, _ *domain.ContractProof) error {
	return s.err
}

func (s *alwaysErrProofStore) GetByContract(_ context.Context, _ uuid.UUID, _ domain.ContractKind) (*domain.ContractProof, error) {
	return nil, s.err
}

func (s *alwaysErrProofStore) Supersede(_ context.Context, _ *domain.ContractProof) error {
	return s.err
}
