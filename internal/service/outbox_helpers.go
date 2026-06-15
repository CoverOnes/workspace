package service

import (
	"encoding/json"
	"fmt"
)

// marshalEvent serializes any event struct to JSON bytes for the outbox payload.
// Events that carry signed content must be byte-for-byte reproducible; JSON
// serialization is deterministic for these fixed-schema structs.
func marshalEvent(evt any) ([]byte, error) {
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal event: %w", err)
	}

	return b, nil
}
