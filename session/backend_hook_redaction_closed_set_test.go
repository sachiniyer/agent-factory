package session

import (
	"encoding/json"
	"strings"
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
		{
			name:   "serialized document with trailing malformed bytes",
			output: `{"message":"{\"token\":\"trailing-secret\"} trailing"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "truncated serialized document with an overwritten token member",
			output: `{"message":"{\"payload\":{\"token\":\"duplicate-secret\"},\"payload\":\"safe\""}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "malformed multiply serialized document with a token",
			output: `{"message":"\"{\\\"token\\\":\\\"deep-secret\\\"}\" trailing"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "serialized document with a token after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"token\":\"post-error-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "serialized child with a token after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"child\":\"{\\\"token\\\":\\\"nested-post-error-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "token after a raw newline in a malformed string",
			output: `{"message":"{\"message\":\"unterminated\n,\"token\":\"newline-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "quoted token prose after a syntax error",
			output: `{"message":"{\"message\":\"diagnostic says \\\"token\\\": unavailable\""}`,
			want:   `{"message":"{\"message\":\"diagnostic says \\\"token\\\": unavailable\""}`,
		},
		{
			name:   "serialized child followed by a colon after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"child\":\"{\\\"token\\\":\\\"colon-secret\\\"}\": junk}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "truncated serialized child string with a token",
			output: `{"message":"{\"child\":\"{\\\"token\\\":\\\"truncated-string-secret\\\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "escaped serialized child after a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n{\\\"token\\\":\\\"invalid-string-child-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "escaped serialized child before a later invalid escape",
			output: `{"message":"{\"message\":\"unterminated\n{\\\"token\\\":\\\"invalid-escape-secret\\\"}\\q\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "opener-prefixed token prose",
			output: `{"message":"[INFO] diagnostic says \"token\": unavailable"}`,
			want:   `{"message":"[INFO] diagnostic says \"token\": unavailable"}`,
		},
		{
			name:   "serialized token key split by a raw newline",
			output: `{"message":"{\"to\nken\":\"split-key-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "serialized token escape split by a raw newline",
			output: `{"message":"{\"\\u00\n74oken\":\"split-escape-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "malformed object whose first item is invalid",
			output: `{"message":"{INVALID,\"token\":\"first-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "malformed array whose first item is invalid",
			output: `{"message":"[INVALID,{\"token\":\"first-array-secret\"}]"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "escaped serialized child after an invalid escape",
			output: `{"message":"{\"message\":\"unterminated\n{error}\\q{\\\"token\\\":\\\"later-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "token key after a stray backslash",
			output: `{"message":"{\"a\":INVALID,\\\"token\":\"slash-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "unicode-escaped object opener after a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n\\u007b\\\"token\\\":\\\"unicode-open-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "timestamp-prefixed quoted token prose",
			output: `{"message":"[2026-08-25] diagnostic says \"token\": unavailable"}`,
			want:   `{"message":"[2026-08-25] diagnostic says \"token\": unavailable"}`,
		},
		{
			name:   "serialized child opener immediately before a raw newline",
			output: `{"message":"{\"child\":\"{\n\\\"token\\\":\\\"brace-before-newline-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "token key after a malformed block comment",
			output: `{"message":"{/*comment*/\"token\":\"comment-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "token key after a malformed line comment",
			output: `{"message":"{// comment\n\"token\":\"line-comment-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "serialized token document after a byte order mark",
			output: `{"message":"\ufeff{\"token\":\"bom-secret\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "unicode-escaped object opener split by a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n\\u00\n7b\\\"token\\\":\\\"split-opener-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
		{
			name:   "array-prefixed comma-delimited quoted token prose",
			output: `{"message":"[INFO, \"token\": unavailable]"}`,
			want:   `{"message":"[INFO, \"token\": unavailable]"}`,
		},
		{
			name:   "escaped token key quote split by a raw newline",
			output: `{"message":"{\"child\":\"{\\\n\"token\\\":\\\"split-quote-secret\\\"}\"}"}`,
			want:   `{"message":"[REDACTED]"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, redactHookOutputTokens(test.output))
		})
	}
}

func TestMalformedHookJSONStringRecoveryScansOnce(t *testing.T) {
	contents := strings.Repeat("{", 256)
	var containsToken bool
	allocations := testing.AllocsPerRun(1, func() {
		containsToken = malformedHookJSONStringContentsContainToken(contents)
	})

	require.False(t, containsToken)
	assert.Less(t, allocations, 100.0)
}

func TestMalformedHookJSONQuoteRecoveryScansOnce(t *testing.T) {
	var containsToken bool
	small := "{" + strings.Repeat(`\"{`, 128)
	smallAllocations := testing.AllocsPerRun(1, func() {
		containsToken = malformedHookJSONDocumentContainsTokenKey(small)
	})
	require.False(t, containsToken)

	large := "{" + strings.Repeat(`\"{`, 256)
	largeAllocations := testing.AllocsPerRun(1, func() {
		containsToken = malformedHookJSONDocumentContainsTokenKey(large)
	})
	require.False(t, containsToken)
	assert.Less(t, largeAllocations, smallAllocations*3)
}

func TestMalformedHookJSONQuoteRecoveryHandlesEvenEscapeLayer(t *testing.T) {
	assert.True(t, malformedHookJSONDocumentContainsToken(`{\\"token\\":\\"secret\\"}`))
	assert.True(t, malformedHookRecoveredDocumentContainsToken(`prefix\{\\"token\\":\\"secret\\"}`))
}
