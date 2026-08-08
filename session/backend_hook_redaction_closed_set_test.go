package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, redactHookOutputTokens(test.output))
		})
	}
}
