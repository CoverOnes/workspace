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

	sharedPool, err = postgres.NewPool(ctx, dsn, "", postgres.PoolConfig{}) // empty schema = public (test default)
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
	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolConfig{}) // empty schema = public (test default)
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
	hash := domain.CanonicalContractDigest(
		cid.String(), clientID.String(), freelancerID.String(),
		"Test Contract", "Terms body", amount.StringFixed(2), "TWD", 1,
	)

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

	// Regression: signer_ip is Postgres inet (OID 869). pgx v5 binary mode cannot
	// scan inet directly into *string, so ListByContract returned 500 once any row
	// had a signer_ip. The store now casts signer_ip::text. This covers both a
	// non-NULL inet value and a NULL value through the same list call.
	t.Run("list signatures with signer_ip (inet) does not error and round-trips IP", func(t *testing.T) {
		cIP := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, cIP))

		now := time.Now().UTC().Truncate(time.Millisecond)
		ipv4 := "203.0.113.42"
		// Postgres inet::text renders a host-only address in CIDR form (host => /32).
		// This is the literal value the store returns after the inet->text cast.
		const ipv4Text = "203.0.113.42/32"

		sigWithIP := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        cIP.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: cIP.ContentHash,
			SignerIP:          &ipv4, // non-NULL inet — the exact value that triggered the 500.
			SignedAt:          now,
			CreatedAt:         now,
		}
		sigNullIP := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        cIP.ID,
			SignerUserID:      freelancerID,
			SignerRole:        domain.SignerRoleFreelancer,
			ContractVersion:   1,
			SignedContentHash: cIP.ContentHash,
			SignerIP:          nil, // NULL inet must still scan into *string.
			SignedAt:          now,
			CreatedAt:         now,
		}

		require.NoError(t, sigStore.Create(ctx, sigWithIP))
		require.NoError(t, sigStore.Create(ctx, sigNullIP))

		// This is the call that previously 500'd ("cannot scan inet (OID 869) ...").
		sigs, err := sigStore.ListByContract(ctx, cIP.ID)
		require.NoError(t, err)
		require.Len(t, sigs, 2)

		byID := make(map[uuid.UUID]*domain.Signature, len(sigs))
		for _, s := range sigs {
			byID[s.ID] = s
		}

		gotWithIP := byID[sigWithIP.ID]
		require.NotNil(t, gotWithIP)
		require.NotNil(t, gotWithIP.SignerIP, "non-NULL signer_ip must round-trip as *string")
		assert.Equal(t, ipv4Text, *gotWithIP.SignerIP)

		gotNullIP := byID[sigNullIP.ID]
		require.NotNil(t, gotNullIP)
		assert.Nil(t, gotNullIP.SignerIP, "NULL signer_ip must scan as nil *string, not error")
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

	// Migration 000005 adds UNIQUE(contract_id, signer_role, contract_version).
	// This backstops dual-sign integrity: even two DISTINCT users cannot both
	// occupy the SAME role for the same contract version, so CountValidSignatures
	// (which counts DISTINCT signer_role) can never be inflated to 2 by a single
	// role. A second row with the same role+version is rejected as a 23505 (which
	// the store surfaces as ErrAlreadySigned).
	t.Run("two distinct users cannot both sign as the same role/version (role-version unique index)", func(t *testing.T) {
		c4 := makeTestContract(clientID, freelancerID)
		require.NoError(t, contractStore.Create(ctx, c4))

		now := time.Now().UTC().Truncate(time.Millisecond)
		clientSig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c4.ID,
			SignerUserID:      clientID,
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c4.ContentHash,
			SignedAt:          now,
			CreatedAt:         now,
		}
		require.NoError(t, sigStore.Create(ctx, clientSig))

		// A DIFFERENT user attempts to also sign as CLIENT for the same version.
		// (signer_user_id differs, so the older signer_user_id-based index does NOT
		// catch this — only the new role-version index does.)
		impostorClientSig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c4.ID,
			SignerUserID:      uuid.New(), // distinct user
			SignerRole:        domain.SignerRoleClient,
			ContractVersion:   1,
			SignedContentHash: c4.ContentHash,
			SignedAt:          time.Now().UTC(),
			CreatedAt:         time.Now().UTC(),
		}
		err := sigStore.Create(ctx, impostorClientSig)
		require.ErrorIs(t, err, domain.ErrAlreadySigned,
			"a second CLIENT signature for the same version must be rejected by the role-version unique index")

		// Sanity: a FREELANCER signature for the same version is still allowed.
		freelancerSig := &domain.Signature{
			ID:                uuid.New(),
			ContractID:        c4.ID,
			SignerUserID:      freelancerID,
			SignerRole:        domain.SignerRoleFreelancer,
			ContractVersion:   1,
			SignedContentHash: c4.ContentHash,
			SignedAt:          time.Now().UTC(),
			CreatedAt:         time.Now().UTC(),
		}
		require.NoError(t, sigStore.Create(ctx, freelancerSig),
			"the opposite role must still be insertable for the same version")
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

		txErr := tx.WithTx(ctx,
			func(ctx context.Context, txContracts store.ContractStore, txSigs store.SignatureStore, _ store.OutboxStore) error {
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

// TestPool_SchemaIsolation_Integration verifies that when NewPool is called with a
// non-empty schema name, migrations land in that schema (not public) and all queries
// resolve against it via search_path.
func TestPool_SchemaIsolation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	const testSchema = "dev_test_schema"

	ctx := context.Background()

	// Spin up a dedicated container so this test is fully isolated from sharedPool.
	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Build pool with the custom schema — this should CREATE SCHEMA IF NOT EXISTS
	// and set search_path on every connection.
	pool, err := postgres.NewPool(ctx, dsn, testSchema, postgres.PoolConfig{})
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	// Apply migrations via the same pool so they land inside testSchema.
	var upFiles []string

	walkErr := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkEntryErr error) error {
		if walkEntryErr != nil {
			return walkEntryErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, walkErr, "walk embedded migrations FS")
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration file %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s inside schema %s", file, testSchema)
	}

	// Assert the main workspace table (contracts) exists in the custom schema,
	// not in public, using information_schema.tables.
	var tableSchema string

	row := pool.QueryRow(
		ctx,
		`SELECT table_schema FROM information_schema.tables
		 WHERE table_name = 'contracts' AND table_schema = $1`,
		testSchema,
	)

	err = row.Scan(&tableSchema)
	require.NoError(t, err, "contracts table must exist in schema %q", testSchema)
	assert.Equal(t, testSchema, tableSchema, "contracts must be in schema %q, not public", testSchema)

	// Also verify that public schema does NOT contain the contracts table
	// (confirming isolation — migrations did not fall through to public).
	var publicCount int

	countRow := pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_name = 'contracts' AND table_schema = 'public'`,
	)

	require.NoError(t, countRow.Scan(&publicCount))
	assert.Equal(t, 0, publicCount, "contracts must NOT exist in public schema when schema=%q", testSchema)
}

// TestPool_ReservedWordSchema_Integration verifies that schema names that are
// PostgreSQL reserved words (e.g. "user") work correctly when using
// pgx.Identifier.Sanitize() quoting.  Without quoting, PG returns error 42601
// (syntax error) on both CREATE SCHEMA and SET search_path statements.
func TestPool_ReservedWordSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// "user" is a PG reserved word — it passes [a-zA-Z_][a-zA-Z0-9_]* validation
	// but fails at runtime without identifier quoting.
	const reservedSchema = "user"

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// NewPool must succeed: CREATE SCHEMA IF NOT EXISTS "user" and
	// SET search_path = "user", public must both work without syntax error.
	pool, err := postgres.NewPool(ctx, dsn, reservedSchema, postgres.PoolConfig{MaxConns: 2, MinConns: 1})
	require.NoError(t, err, "NewPool with reserved-word schema %q must not return error 42601", reservedSchema)

	t.Cleanup(pool.Close)

	// Apply migrations so the contracts table is created inside the "user" schema.
	var upFiles []string

	walkErr := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkEntryErr error) error {
		if walkEntryErr != nil {
			return walkEntryErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, walkErr, "walk embedded migrations FS")
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration file %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s inside reserved-word schema %q", file, reservedSchema)
	}

	// Assert contracts table exists in the "user" schema (not public).
	var tableSchema string

	row := pool.QueryRow(
		ctx,
		`SELECT table_schema FROM information_schema.tables
		 WHERE table_name = 'contracts' AND table_schema = $1`,
		reservedSchema,
	)

	err = row.Scan(&tableSchema)
	require.NoError(t, err, "contracts table must exist in reserved-word schema %q", reservedSchema)
	assert.Equal(t, reservedSchema, tableSchema, "contracts must be in schema %q, not public", reservedSchema)

	// Confirm public schema has no contracts (isolation check).
	var publicCount int

	countRow := pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_name = 'contracts' AND table_schema = 'public'`,
	)

	require.NoError(t, countRow.Scan(&publicCount))
	assert.Equal(t, 0, publicCount, "contracts must NOT exist in public when schema=%q", reservedSchema)
}
