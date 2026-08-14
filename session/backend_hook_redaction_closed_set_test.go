package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An input that is not exactly one JSON value has no trustworthy field
// boundaries. Redacting any smaller span would claim structure the JSON parser
// rejected, so the complete payload must be replaced regardless of which bytes
// happen to resemble a token field.
func TestRedactHookOutputTokensClosesOverParseOutcome(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "parsed value uses structural redaction",
			output: `{"message":"quota exceeded","token":"secret"}`,
			want:   `{"message":"quota exceeded","token":"[REDACTED]"}`,
		},
		{
			name:   "unparseable payload with token-like bytes is replaced",
			output: `{"message":"quota exceeded","token":"secret"} trailing bytes`,
			want:   "[REDACTED]",
		},
		{
			name:   "unparseable payload without token-like bytes is still replaced",
			output: "provisioning failed: quota exceeded",
			want:   "[REDACTED]",
		},
		{
			name:   "overwritten duplicate member cannot survive from raw input",
			output: `{"message":{"token":"overwritten-secret"},"message":"ok"}`,
			want:   `{"message":"ok"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, redactHookOutputTokens(test.output))
		})
	}
}

func TestRedactHookOutputTokensRecursesThroughSerializedJSON(t *testing.T) {
	document := `{"token":"nested-secret"}`
	for range 4 {
		encoded, err := json.Marshal(document)
		require.NoError(t, err)
		document = string(encoded)
	}

	assert.NotContains(t, redactHookOutputTokens(document), "nested-secret")
}

func TestRedactHookOutputTokensPreservesUnparseableNestedDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "bracket-prefixed diagnostic",
			output: `{"message":"[INFO] connection failed"}`,
			want:   `{"message":"[INFO] connection failed"}`,
		},
		{
			name:   "brace-prefixed diagnostic",
			output: `{"message":"{error}: bad config"}`,
			want:   `{"message":"{error}: bad config"}`,
		},
		{
			name:   "quote-prefixed diagnostic",
			output: `{"message":"\"quoted diagnostic"}`,
			want:   `{"message":"\"quoted diagnostic"}`,
		},
		{
			name:   "serialized document with token",
			output: `{"message":"{\"token\":\"nested-secret\"}"}`,
			want:   `{"message":"{\"token\":\"[REDACTED]\"}"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, redactHookOutputTokens(test.output))
		})
	}
}
