package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/fileclient"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFileServer is a test HTTP server that simulates the file service S2S API.
type stubFileServer struct {
	server         *httptest.Server
	registerCalled int
	presignCalled  int
	// presignURL is the URL the stub returns from POST /internal/v1/attachments/presign.
	presignURL string
	// registerStatusCode lets tests inject errors.
	registerStatusCode int
}

func newStubFileServer(presignURL string) *stubFileServer {
	s := &stubFileServer{presignURL: presignURL, registerStatusCode: http.StatusNoContent}

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/attachments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.registerCalled++
			w.WriteHeader(s.registerStatusCode)
		}
	})
	mux.HandleFunc("/internal/v1/attachments/presign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.presignCalled++
			w.Header().Set("Content-Type", "application/json")

			resp := map[string]any{"url": s.presignURL, "ttlSeconds": 300}
			if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
				http.Error(w, "encode error", http.StatusInternalServerError)
			}
		}
	})

	s.server = httptest.NewServer(mux)

	return s
}

func (s *stubFileServer) close() {
	s.server.Close()
}

// TestSignatureAttachment_Integration_RegisterAndPresign verifies end-to-end:
// 1. A signature with an attached fileId calls fileclient.Register after commit.
// 2. file_id is persisted on the signature row (SetFileID).
// 3. A party can request a presigned download URL.
// 4. A non-party gets ErrNotFound (403/404 at handler level).
func TestSignatureAttachment_Integration_RegisterAndPresign(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	const expectedPresignURL = "https://file.example.com/presign/token"

	stub := newStubFileServer(expectedPresignURL)
	defer stub.close()

	fc := fileclient.New(fileclient.Config{
		BaseURL:   stub.server.URL,
		ServiceID: "workspace",
		Token:     "test-token-that-is-long-enough-32c",
	})

	ctx := context.Background()

	contractStore := postgres.NewContractStore(sharedServicePool)
	sigStore := postgres.NewSignatureStore(sharedServicePool)
	txMgr := postgres.NewTxManager(sharedServicePool)
	pub := &fakePublisher{}

	contractSvc := service.NewContractService(contractStore, sigStore, txMgr, pub, fc)
	sigSvc := service.NewSignatureService(contractStore, sigStore, fc)

	// Truncate tables for test isolation.
	truncateServiceTables(t)

	clientID := uuid.New()
	freelancerID := uuid.New()
	thirdPartyID := uuid.New()
	fileID := uuid.New()

	// Create and prepare a PENDING_SIGNATURE contract.
	c := makeServiceContract(clientID, freelancerID, domain.ContractStatusDraft)
	require.NoError(t, contractStore.Create(ctx, c))

	c.Status = domain.ContractStatusPendingSignature
	require.NoError(t, contractStore.Update(ctx, c))

	t.Run("sign with attachment registers file and persists file_id", func(t *testing.T) {
		_, err := contractSvc.SignContract(ctx, service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          clientID,
			SignedContentHash: c.ContentHash,
			FileID:            &fileID,
		})
		require.NoError(t, err)

		// registerAttachmentBestEffort is called synchronously; no sleep needed.
		assert.Equal(t, 1, stub.registerCalled, "file service Register must be called once")

		// Verify file_id is persisted on the signature row.
		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 1, "exactly one signature must exist")
		require.NotNil(t, sigs[0].FileID, "file_id must be persisted after successful registration")
		assert.Equal(t, fileID, *sigs[0].FileID)
	})

	t.Run("party can presign the attachment", func(t *testing.T) {
		// Find the created signature.
		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 1)

		result, err := sigSvc.GetAttachmentDownloadURL(ctx, service.DownloadURLInput{
			ContractID:  c.ID,
			SignatureID: sigs[0].ID,
			CallerID:    clientID,
		})
		require.NoError(t, err)
		assert.Equal(t, expectedPresignURL, result.URL)
		assert.Equal(t, 300, result.TTLSeconds)
		assert.Equal(t, 1, stub.presignCalled)
	})

	t.Run("non-party gets ErrNotFound from GetAttachmentDownloadURL", func(t *testing.T) {
		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 1)

		_, err = sigSvc.GetAttachmentDownloadURL(ctx, service.DownloadURLInput{
			ContractID:  c.ID,
			SignatureID: sigs[0].ID,
			CallerID:    thirdPartyID,
		})
		require.ErrorIs(t, err, domain.ErrNotFound, "non-party must get ErrNotFound (IDOR-safe 404)")
	})

	t.Run("signature without file_id returns ErrNotFound", func(t *testing.T) {
		// Sign as freelancer WITHOUT a fileID.
		_, err := contractSvc.SignContract(ctx, service.SignContractInput{
			ContractID:        c.ID,
			CallerID:          freelancerID,
			SignedContentHash: c.ContentHash,
			FileID:            nil, // no attachment
		})
		require.NoError(t, err)

		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 2)

		// Find the freelancer signature (the one without file_id).
		var freelancerSig *domain.Signature

		for _, s := range sigs {
			if s.SignerUserID == freelancerID {
				freelancerSig = s

				break
			}
		}

		require.NotNil(t, freelancerSig)
		assert.Nil(t, freelancerSig.FileID, "freelancer signed without attachment")

		_, err = sigSvc.GetAttachmentDownloadURL(ctx, service.DownloadURLInput{
			ContractID:  c.ID,
			SignatureID: freelancerSig.ID,
			CallerID:    clientID,
		})
		require.ErrorIs(t, err, domain.ErrNotFound, "signature without file_id must return ErrNotFound")
	})
}

// truncateServiceTables clears all rows in the mutable tables for test isolation.
func truncateServiceTables(t *testing.T) {
	t.Helper()

	_, err := sharedServicePool.Exec(
		context.Background(),
		"TRUNCATE TABLE worklogs, tasks, contract_signatures, contracts RESTART IDENTITY CASCADE",
	)
	require.NoError(t, err, "truncate tables")
}

// makeServiceContract creates a test Contract domain object.
func makeServiceContract(clientID, freelancerID uuid.UUID, status domain.ContractStatus) *domain.Contract {
	now := time.Now().UTC().Truncate(time.Millisecond)

	cid := uuid.New()
	hash := domain.CanonicalContractDigest(
		cid.String(), clientID.String(), freelancerID.String(),
		"Test Contract", "Terms body", "1000.00", "TWD", 1,
	)

	return &domain.Contract{
		ID:               cid,
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Title:            "Test Contract",
		Terms:            "Terms body",
		Amount:           decimal.NewFromInt(1000),
		Currency:         "TWD",
		ContentHash:      hash,
		Version:          1,
		Status:           status,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}
