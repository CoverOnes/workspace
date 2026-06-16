package client

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// FakeFileClient is an in-memory FileClient for tests and local development.
// It stores bytes keyed by FileID and returns deterministic presign URLs.
// Thread-safe for use across goroutines in table-driven parallel tests.
type FakeFileClient struct {
	mu      sync.Mutex
	files   map[uuid.UUID][]byte
	keys    map[uuid.UUID]string
	presign func(id uuid.UUID) (string, int)
}

// NewFakeFileClient returns a FakeFileClient with an optional custom presign function.
//
//   - presign: optional; if non-nil, called by PresignDownload to produce (url, ttlSeconds).
//     If nil, a deterministic default URL scheme is used.
func NewFakeFileClient(presign func(id uuid.UUID) (string, int)) *FakeFileClient {
	return &FakeFileClient{
		files:   make(map[uuid.UUID][]byte),
		keys:    make(map[uuid.UUID]string),
		presign: presign,
	}
}

// StoreSystemFile stores data in memory, assigns a new UUID, and returns it.
// Satisfies the FileClient interface.
func (f *FakeFileClient) StoreSystemFile(_ context.Context, in StoreSystemFileInput) (*StoreSystemFileResult, error) {
	id := uuid.New()
	key := fmt.Sprintf("fake/%s/%s", in.SystemContext, id)

	f.mu.Lock()
	f.files[id] = append([]byte(nil), in.Data...)
	f.keys[id] = key
	f.mu.Unlock()

	return &StoreSystemFileResult{
		FileID:    id,
		ObjectKey: key,
	}, nil
}

// PresignDownload returns a deterministic fake presigned URL for the given file ID.
// Returns an error if the file was never stored.
func (f *FakeFileClient) PresignDownload(_ context.Context, fileID uuid.UUID) (presignURL string, ttlSeconds int, err error) {
	f.mu.Lock()
	_, ok := f.files[fileID]
	f.mu.Unlock()

	if !ok {
		return "", 0, fmt.Errorf("fake file client: file %s not found", fileID)
	}

	if f.presign != nil {
		u, ttl := f.presign(fileID)
		return u, ttl, nil
	}

	return fmt.Sprintf("https://fake-file-svc.test/download/%s?sig=deterministic", fileID), 300, nil
}

// Get returns the stored bytes for a file ID (nil if not stored).
// Useful in tests for asserting PDF content without going through the presign flow.
func (f *FakeFileClient) Get(id uuid.UUID) []byte {
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

// StoredIDs returns all file IDs that have been stored so far.
// Useful in tests to iterate over stored files.
func (f *FakeFileClient) StoredIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]uuid.UUID, 0, len(f.files))
	for id := range f.files {
		ids = append(ids, id)
	}

	return ids
}
