package handler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSanitizeUserAgentHeader verifies control-char stripping and ANSI-escape removal
// to prevent log/record injection via crafted User-Agent values (WS2-M2).
func TestSanitizeUserAgentHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  *string // nil means expect nil return
	}{
		{
			name:  "empty string returns nil",
			input: "",
			want:  nil,
		},
		{
			name:  "clean user-agent passes through unchanged",
			input: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
			want:  ptr("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"),
		},
		{
			name:  "null byte is stripped",
			input: "Mozilla\x00/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "carriage return is stripped",
			input: "Mozilla\r/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "newline (LF) is stripped",
			input: "Mozilla\n/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "embedded CRLF header injection is stripped",
			input: "Mozilla/5.0\r\nX-Injected: evil",
			want:  ptr("Mozilla/5.0X-Injected: evil"),
		},
		{
			name:  "ANSI CSI reset sequence is stripped",
			input: "Mozilla\x1b[0m/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "ANSI color sequence is stripped",
			input: "\x1b[31mMozilla\x1b[0m/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "multiple control chars and ANSI stripped together",
			input: "\x1b[1;32mHello\x00\r\n\x7fWorld\x1b[0m",
			want:  ptr("HelloWorld"),
		},
		{
			name:  "DEL (0x7f) is stripped",
			input: "Mozilla\x7f/5.0",
			want:  ptr("Mozilla/5.0"),
		},
		{
			name:  "tab is preserved",
			input: "Mozilla\t/5.0",
			want:  ptr("Mozilla\t/5.0"),
		},
		{
			name:  "string capped at 500 runes",
			input: strings.Repeat("A", 600),
			want:  ptr(strings.Repeat("A", 500)),
		},
		{
			name:  "only control chars produces nil",
			input: "\x00\r\n\x01\x1f",
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sanitizeUserAgentHeader(tc.input)

			if tc.want == nil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, *tc.want, *got)
			}
		})
	}
}

// ptr is a helper that returns a pointer to s.
func ptr(s string) *string { return &s }
