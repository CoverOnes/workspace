package postgres_test

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/store"
	"github.com/CoverOnes/workspace/internal/store/postgres"
	migrations "github.com/CoverOnes/workspace/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// sharedPool is the singleton pool shared across all integration subtests.
var sharedPool *pgxpool.Pool

// TestMain starts ONE Postgres container, runs migrations once, then runs all tests.
// This avoids the per-test container startup cost.
func TestMain(m *testing.M) {
	// flag.Parse must be called before testing.Short() is used.
	flag.Parse()
	os.Exit(runMain(m))
}

// runMain is extracted so deferred cleanup runs before os.Exit is called by TestMain.
func runMain(m *testing.M) int {
	if testing.Short() {
		return m.Run()
	}

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)

		return 1
	}

	defer func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", termErr)
		}
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get connection string: %v\n", err)

		return 1
	}

	sharedPool, err = postgres.NewPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create pool: %v\n", err)

		return 1
	}

	defer sharedPool.Close()

	if err := applyMigrations(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)

		return 1
	}

	return m.Run()
}

// applyMigrations runs all embedded *.up.sql files against the test database.
func applyMigrations(ctx context.Context, dsn string) error {
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("create migration pool: %w", err)
	}

	defer pool.Close()

	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", file, readErr)
		}

		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply %s: %w", file, execErr)
		}
	}

	return nil
}

// truncateTables clears all mutable rows between sub-tests that share the DB.
func truncateTables(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	_, err := sharedPool.Exec(
		ctx,
		"TRUNCATE TABLE worklogs, tasks, contract_signatures, contracts RESTART IDENTITY CASCADE",
	)
	require.NoError(t, err, "truncate tables")
}

func makeTestContract(clientID, freelancerID uuid.UUID) *domain.Contract {
	now := time.Now().UTC().Truncate(time.Millisecond)
	amount := decimal.NewFromInt(5000)
	cid := uuid.New()
	hash := domain.CanonicalContractDigest(cid.String(), "Test Contract", "Terms body", amount.StringFixed(2), "TWD", 1)

	return &domain.Contract{
		ID:               cid,
		ListingID:        uuid.New(),
		AcceptedBidID:    uuid.New(),
		ClientUserID:     clientID,
		FreelancerUserID: freelancerID,
		Title:            "Test Contract",
		Terms:            "Terms body",
		Amount:           amount,
		Currency:         "TWD",
		ContentHash:      hash,
		Version:          1,
		Status:           domain.ContractStatusDraft,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// TestContractStore_Integration tests ContractStore against real Postgres.
func TestContractStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	truncateTables(t)

	ctx := context.Background()
	contractStore := postgres.NewContractStore(sharedPool)

	clientID := uuid.New()
	freelancerID := uuid.New()

	t.Run("create and get contract", func(t *testing.T) {
		c := makeTestContract(clientID, freelancerID)

		require.NoError(t, contractStore.Create(ctx, c))

		got, err := contractStore.GetByID(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.ID)
		assert.Equal(t, c.ClientUserID, got.ClientUserID)
		assert.Equal(t, c.FreelancerUserID, got.FreelancerUserID)
		assert.Equal(t, domain.ContractStatusDraft, got.Status)
		assert.Equal(t, 1, got.Version)
		assert.True(t, c.Amount.Equal(got.Amount))
	})

	t.Run("get not found", func(t *testing.T) {
		_, err := contractStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrContractNotFound)
	})

	t.Run("duplicate accepted_bid_id returns conflict", func(t *testing.T) {
		c1 := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, c1))

		c2 := makeTestContract(clientID, freelancerID)
		c2.AcceptedBidID = c1.AcceptedBidID // same bid_id

		err := contractStore.Create(ctx, c2)
		require.ErrorIs(t, err, domain.ErrConflict)
	})

	t.Run("list by party returns self-scoped results", func(t *testing.T) {
		otherClientID := uuid.New()
		otherFreelancerID := uuid.New()
		c1 := makeTestContract(otherClientID, otherFreelancerID)
		require.NoError(t, contractStore.Create(ctx, c1))

		thirdClientID := uuid.New()
		thirdFreelancerID := uuid.New()
		c2 := makeTestContract(thirdClientID, thirdFreelancerID)
		require.NoError(t, contractStore.Create(ctx, c2))

		// List by party: should only see contracts where we're a party.
		result, err := contractStore.ListByParty(ctx, store.ContractFilter{
			PartyUserID: otherClientID,
		})
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, c1.ID, result[0].ID)
	})

	t.Run("update contract status", func(t *testing.T) {
		c := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, c))

		c.Status = domain.ContractStatusPendingSignature
		c.Version = 2

		require.NoError(t, contractStore.Update(ctx, c))

		got, err := contractStore.GetByID(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.ContractStatusPendingSignature, got.Status)
		assert.Equal(t, 2, got.Version)
	})
}

// TestSignatureStore_Integration tests SignatureStore against real Postgres.
func TestSignatureStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	truncateTables(t)

	ctx := context.Background()
	contractStore := postgres.NewContractStore(sharedPool)
	sigStore := postgres.NewSignatureStore(sharedPool)

	clientID := uuid.New()
	freelancerID := uuid.New()

	c := makeTestContract(clientID, freelancerID)
	require.NoError(t, contractStore.Create(ctx, c))

	t.Run("create signature and list by contract", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		sig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c.ContentHash,
			SignedAt:          now,
			CreatedAt:         now,
		}

		require.NoError(t, sigStore.Create(ctx, sig))

		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 1)
		assert.Equal(t, sig.ID, sigs[0].ID)
		assert.Equal(t, domain.SignerRoleClient, sigs[0].SignerRole)
	})

	t.Run("count valid signatures: dual-sign", func(t *testing.T) {
		c2 := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, c2))

		now := time.Now().UTC().Truncate(time.Millisecond)
		sig1 := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c2.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c2.ContentHash,
			SignedAt:          now,
			CreatedAt:         now,
		}
		sig2 := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c2.ID,
			SignerUserID:      freelancerID,
			SignerRole:        domain.SignerRoleFreelancer,
			ContractVersion:   1,
			SignedContentHash: c2.ContentHash,
			SignedAt:          now,
			CreatedAt:         now,
		}

		require.NoError(t, sigStore.Create(ctx, sig1))
		require.NoError(t, sigStore.Create(ctx, sig2))

		count, err := sigStore.CountValidSignatures(ctx, c2.ID, 1, c2.ContentHash)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	t.Run("duplicate sign (same signer, same version) returns ErrAlreadySigned", func(t *testing.T) {
		c3 := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, c3))

		now := time.Now().UTC().Truncate(time.Millisecond)
		sig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c3.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c3.ContentHash,
			SignedAt:          now,
			CreatedAt:         now,
		}
		require.NoError(t, sigStore.Create(ctx, sig))

		// Attempt to sign again (same signer, same version) -> should return ErrAlreadySigned.
		sig2 := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c3.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c3.ContentHash,
			SignedAt:          time.Now().UTC(),
			CreatedAt:         time.Now().UTC(),
		}
		err := sigStore.Create(ctx, sig2)
		require.ErrorIs(t, err, domain.ErrAlreadySigned)
	})
}

// TestTaskStore_Integration tests TaskStore against real Postgres.
func TestTaskStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	truncateTables(t)

	ctx := context.Background()
	taskStore := postgres.NewTaskStore(sharedPool)
	clientID := uuid.New()
	contractID := uuid.New()

	t.Run("create and get task", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		task := &domain.Task{
			ID:         uuid.New(),
			ContractID: contractID,
			Title:      "Test task",
			Status:     domain.TaskStatusTodo,
			CreatedAt:  now,
			UpdatedAt:  now,
		}

		require.NoError(t, taskStore.Create(ctx, task))

		got, err := taskStore.GetByID(ctx, task.ID)
		require.NoError(t, err)
		assert.Equal(t, task.ID, got.ID)
		assert.Equal(t, domain.TaskStatusTodo, got.Status)
	})

	t.Run("get not found", func(t *testing.T) {
		_, err := taskStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrTaskNotFound)
	})

	t.Run("update task status", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		task := &domain.Task{
			ID:         uuid.New(),
			ContractID: contractID,
			Title:      "Task to update",
			Status:     domain.TaskStatusTodo,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		require.NoError(t, taskStore.Create(ctx, task))

		task.Status = domain.TaskStatusDone
		task.AssigneeUserID = &clientID
		require.NoError(t, taskStore.Update(ctx, task))

		got, err := taskStore.GetByID(ctx, task.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.TaskStatusDone, got.Status)
		require.NotNil(t, got.AssigneeUserID)
		assert.Equal(t, clientID, *got.AssigneeUserID)
	})

	t.Run("soft delete task", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		task := &domain.Task{
			ID:         uuid.New(),
			ContractID: contractID,
			Title:      "To delete",
			Status:     domain.TaskStatusTodo,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		require.NoError(t, taskStore.Create(ctx, task))
		require.NoError(t, taskStore.SoftDelete(ctx, task.ID))

		_, err := taskStore.GetByID(ctx, task.ID)
		require.ErrorIs(t, err, domain.ErrTaskNotFound)
	})
}

// TestWorklogStore_Integration tests WorklogStore against real Postgres.
func TestWorklogStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	truncateTables(t)

	ctx := context.Background()
	worklogStore := postgres.NewWorklogStore(sharedPool)
	userID := uuid.New()
	contractID := uuid.New()

	t.Run("create and list worklog", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		wl := &domain.Worklog{
			ID:          uuid.New(),
			ContractID:  contractID,
			UserID:      userID,
			Description: "Worked on API design",
			Minutes:     120,
			LoggedAt:    now,
			CreatedAt:   now,
		}

		require.NoError(t, worklogStore.Create(ctx, wl))

		list, err := worklogStore.ListByContract(ctx, contractID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, wl.ID, list[0].ID)
		assert.Equal(t, 120, list[0].Minutes)
	})

	t.Run("get not found", func(t *testing.T) {
		_, err := worklogStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrWorklogNotFound)
	})

	t.Run("soft delete worklog", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		wl := &domain.Worklog{
			ID:          uuid.New(),
			ContractID:  contractID,
			UserID:      userID,
			Description: "To delete",
			Minutes:     30,
			LoggedAt:    now,
			CreatedAt:   now,
		}
		require.NoError(t, worklogStore.Create(ctx, wl))
		require.NoError(t, worklogStore.SoftDelete(ctx, wl.ID))

		_, err := worklogStore.GetByID(ctx, wl.ID)
		require.ErrorIs(t, err, domain.ErrWorklogNotFound)
	})
}

// TestTxManager_Integration tests atomic dual-sign flow.
func TestTxManager_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	truncateTables(t)

	ctx := context.Background()
	tx := postgres.NewTxManager(sharedPool)
	contractStore := postgres.NewContractStore(sharedPool)
	sigStore := postgres.NewSignatureStore(sharedPool)

	clientID := uuid.New()
	freelancerID := uuid.New()

	t.Run("transaction commits atomically: dual-sign activates contract", func(t *testing.T) {
		c := makeTestContract(clientID, freelancerID)
		c.Status = domain.ContractStatusPendingSignature
		require.NoError(t, contractStore.Create(ctx, c))

		now := time.Now().UTC().Truncate(time.Millisecond)

		txErr := tx.WithTx(ctx, func(ctx context.Context, txContracts store.ContractStore, txSigs store.SignatureStore) error {
			locked, err := txContracts.GetByIDForUpdate(ctx, c.ID)
			if err != nil {
				return err
			}

			sig1 := &domain.Signature{
				ID:                uuid.New(),
				ContractID:        locked.ID,
				SignerUserID:      clientID,
				SignerRole:        domain.SignerRoleClient,
				ContractVersion:   locked.Version,
				SignedContentHash: locked.ContentHash,
				SignedAt:          now,
				CreatedAt:         now,
			}

			if createErr := txSigs.Create(ctx, sig1); createErr != nil {
				return createErr
			}

			sig2 := &domain.Signature{
				ID:                uuid.New(),
				ContractID:        locked.ID,
				SignerUserID:      freelancerID,
				SignerRole:        domain.SignerRoleFreelancer,
				ContractVersion:   locked.Version,
				SignedContentHash: locked.ContentHash,
				SignedAt:          now,
				CreatedAt:         now,
			}

			if createErr := txSigs.Create(ctx, sig2); createErr != nil {
				return createErr
			}

			count, err := txSigs.CountValidSignatures(ctx, locked.ID, locked.Version, locked.ContentHash)
			if err != nil {
				return err
			}

			if count >= 2 {
				activatedAt := now
				locked.Status = domain.ContractStatusActive
				locked.ActivatedAt = &activatedAt

				return txContracts.Update(ctx, locked)
			}

			return nil
		})
		require.NoError(t, txErr)

		// Verify the contract is now ACTIVE.
		got, err := contractStore.GetByID(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.ContractStatusActive, got.Status)
		assert.NotNil(t, got.ActivatedAt)

		// Verify both signatures are persisted.
		sigs, err := sigStore.ListByContract(ctx, c.ID)
		require.NoError(t, err)
		assert.Len(t, sigs, 2)
	})
}
