package service_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/CoverOnes/workspace/internal/fileclient"
	"github.com/google/uuid"
)

// fakeFileClient is an in-memory implementation of the service.proofFileClient interface
// for tests and local development. It stores bytes keyed by FileID and returns
// deterministic presign URLs. Thread-safe for use across goroutines in parallel tests.
type fakeFileClient struct {
	mu    sync.Mutex
	files map[uuid.UUID][]byte
	keys  map[uuid.UUID]string
}

// newFakeFileClient returns a fakeFileClient backed by an in-memory store.
// PresignDownload returns a deterministic fake URL using the default scheme.
func newFakeFileClient() *fakeFileClient {
	return &fakeFileClient{
		files: make(map[uuid.UUID][]byte),
		keys:  make(map[uuid.UUID]string),
	}
}

// StoreSystemFile stores data in memory, assigns a new UUID, and returns it.
// Satisfies the proofFileClient interface via ProofServiceConfig.FileClient.
func (f *fakeFileClient) StoreSystemFile(_ context.Context, in fileclient.StoreSystemFileInput) (*fileclient.StoreSystemFileResult, error) {
	id := uuid.New()
	key := fmt.Sprintf("fake/%s/%s", in.SystemContext, id)

	f.mu.Lock()
	f.files[id] = append([]byte(nil), in.Data...)
	f.keys[id] = key
	f.mu.Unlock()

	return &fileclient.StoreSystemFileResult{
		FileID:    id,
		ObjectKey: key,
	}, nil
}

// PresignDownload returns a deterministic fake presigned URL for the given file ID.
// Returns an error if the file was never stored.
func (f *fakeFileClient) PresignDownload(_ context.Context, fileID uuid.UUID) (presignURL string, ttlSeconds int, err error) {
	f.mu.Lock()
	_, ok := f.files[fileID]
	f.mu.Unlock()

	if !ok {
		return "", 0, fmt.Errorf("fake file client: file %s not found", fileID)
	}

	return fmt.Sprintf("https://fake-file-svc.test/download/%s?sig=deterministic", fileID), 300, nil
}

// get returns the stored bytes for a file ID (nil if not stored).
// Useful in tests for asserting PDF content without going through the presign flow.
func (f *fakeFileClient) get(id uuid.UUID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	data := f.files[id]
	if data == nil {
		return nil
	}

	out := make([]byte, len(data))
	copy(out, data)

	return out
}

// storedIDs returns all file IDs that have been stored so far.
// Useful in tests to iterate over stored files.
func (f *fakeFileClient) storedIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]uuid.UUID, 0, len(f.files))
	for id := range f.files {
		ids = append(ids, id)
	}

	return ids
}
