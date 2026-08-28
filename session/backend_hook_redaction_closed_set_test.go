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

// A serialized child that parses keeps the precise half of the policy: only the
// token field is replaced, and the surrounding diagnostic stays readable.
func TestRedactHookOutputTokensRedactsParsedSerializedChildrenPrecisely(t *testing.T) {
	assert.Equal(t,
		`{"message":"{\"reason\":\"quota exceeded\",\"token\":\"[REDACTED]\"}"}`,
		redactHookOutputTokens(`{"message":"{\"reason\":\"quota exceeded\",\"token\":\"nested-secret\"}"}`))
}

// A string carrying no object opener — literal or escaped — cannot name a token
// field however often it is re-parsed, so it survives byte-exact. This is the
// half of the boundary that keeps a hook's diagnostics worth printing.
func TestRedactHookOutputTokensPreservesDiagnosticsWithoutObjectOpeners(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "bracket-prefixed diagnostic",
			output: `{"message":"[INFO] connection failed"}`,
		},
		{
			name:   "quote-prefixed diagnostic",
			output: `{"message":"\"quoted diagnostic"}`,
		},
		{
			name:   "opener-prefixed token prose",
			output: `{"message":"[INFO] diagnostic says \"token\": unavailable"}`,
		},
		{
			name:   "timestamp-prefixed quoted token prose",
			output: `{"message":"[2026-08-25] diagnostic says \"token\": unavailable"}`,
		},
		{
			name:   "array-prefixed comma-delimited quoted token prose",
			output: `{"message":"[INFO, \"token\": unavailable]"}`,
		},
		{
			name:   "unterminated array of diagnostics",
			output: `{"message":"[warn, retrying"}`,
		},
		{
			name:   "plain sentence naming a token",
			output: `{"message":"the token was rejected by the endpoint"}`,
		},
		{
			// Byte-exact means byte-exact: a string that cannot carry an object is not
			// parsed at all, so it cannot come back whitespace-normalized either.
			name:   "numeric-looking string keeps its spacing",
			output: `{"code":" 42 "}`,
		},
		{
			name:   "serialized array of scalars keeps its spacing",
			output: `{"list":"[1, 2, 3]"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.output, redactHookOutputTokens(test.output),
				"a string with no object opener must survive byte-exact")
		})
	}
}

// Every case here is a string the JSON parser rejected that still holds an
// object opener. The rule replaces the whole string rather than deciding which
// bytes were the secret, so each spelling below is closed by construction rather
// than by a scan that has to recognize it.
//
// The list is kept as regression evidence: every entry was reachable at some
// revision of the scanning fallback this rule replaced, and several leaked.
func TestRedactHookOutputTokensRedactsUnparseableStringsHoldingObjectOpeners(t *testing.T) {
	const want = `{"message":"[REDACTED]"}`
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "brace-prefixed diagnostic",
			output: `{"message":"{error}: bad config"}`,
		},
		{
			name:   "serialized document with trailing malformed bytes",
			output: `{"message":"{\"token\":\"trailing-secret\"} trailing"}`,
		},
		{
			name:   "truncated serialized document with an overwritten token member",
			output: `{"message":"{\"payload\":{\"token\":\"duplicate-secret\"},\"payload\":\"safe\""}`,
		},
		{
			name:   "malformed multiply serialized document with a token",
			output: `{"message":"\"{\\\"token\\\":\\\"deep-secret\\\"}\" trailing"}`,
		},
		{
			name:   "serialized document with a token after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"token\":\"post-error-secret\"}"}`,
		},
		{
			name:   "serialized child with a token after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"child\":\"{\\\"token\\\":\\\"nested-post-error-secret\\\"}\"}"}`,
		},
		{
			name:   "token after a raw newline in a malformed string",
			output: `{"message":"{\"message\":\"unterminated\n,\"token\":\"newline-secret\"}"}`,
		},
		{
			name:   "quoted token prose after a syntax error",
			output: `{"message":"{\"message\":\"diagnostic says \\\"token\\\": unavailable\""}`,
		},
		{
			name:   "serialized child followed by a colon after a syntax error",
			output: `{"message":"{\"level\":INVALID,\"child\":\"{\\\"token\\\":\\\"colon-secret\\\"}\": junk}"}`,
		},
		{
			name:   "truncated serialized child string with a token",
			output: `{"message":"{\"child\":\"{\\\"token\\\":\\\"truncated-string-secret\\\"}"}`,
		},
		{
			name:   "escaped serialized child after a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n{\\\"token\\\":\\\"invalid-string-child-secret\\\"}\"}"}`,
		},
		{
			name:   "escaped serialized child before a later invalid escape",
			output: `{"message":"{\"message\":\"unterminated\n{\\\"token\\\":\\\"invalid-escape-secret\\\"}\\q\"}"}`,
		},
		{
			name:   "serialized token key split by a raw newline",
			output: `{"message":"{\"to\nken\":\"split-key-secret\"}"}`,
		},
		{
			name:   "serialized token escape split by a raw newline",
			output: `{"message":"{\"\\u00\n74oken\":\"split-escape-secret\"}"}`,
		},
		{
			name:   "malformed object whose first item is invalid",
			output: `{"message":"{INVALID,\"token\":\"first-secret\"}"}`,
		},
		{
			name:   "malformed array whose first item is invalid",
			output: `{"message":"[INVALID,{\"token\":\"first-array-secret\"}]"}`,
		},
		{
			name:   "escaped serialized child after an invalid escape",
			output: `{"message":"{\"message\":\"unterminated\n{error}\\q{\\\"token\\\":\\\"later-secret\\\"}\"}"}`,
		},
		{
			name:   "token key after a stray backslash",
			output: `{"message":"{\"a\":INVALID,\\\"token\":\"slash-secret\"}"}`,
		},
		{
			name:   "unicode-escaped object opener after a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n\\u007b\\\"token\\\":\\\"unicode-open-secret\\\"}\"}"}`,
		},
		{
			name:   "serialized child opener immediately before a raw newline",
			output: `{"message":"{\"child\":\"{\n\\\"token\\\":\\\"brace-before-newline-secret\\\"}\"}"}`,
		},
		{
			name:   "token key after a malformed block comment",
			output: `{"message":"{/*comment*/\"token\":\"comment-secret\"}"}`,
		},
		{
			name:   "token key after a malformed line comment",
			output: `{"message":"{// comment\n\"token\":\"line-comment-secret\"}"}`,
		},
		{
			name:   "serialized token document after a byte order mark",
			output: `{"message":"\ufeff{\"token\":\"bom-secret\"}"}`,
		},
		{
			name:   "unicode-escaped object opener split by a raw newline",
			output: `{"message":"{\"message\":\"unterminated\n\\u00\n7b\\\"token\\\":\\\"split-opener-secret\\\"}\"}"}`,
		},
		{
			name:   "escaped token key quote split by a raw newline",
			output: `{"message":"{\"child\":\"{\\\n\"token\\\":\\\"split-quote-secret\\\"}\"}"}`,
		},
		{
			name:   "delimiter inside a quoted value before the token key",
			output: `{"message":"{\"a\":INVALID,\"b\":\"}\",\"token\":\"quoted-delimiter-secret\"}"}`,
		},
		{
			name:   "block comment between the token key and its colon",
			output: `{"message":"{INVALID,\"token\"/*comment*/:\"comment-colon-secret\"}"}`,
		},
		{
			name:   "line comment between the token key and its colon",
			output: `{"message":"{INVALID,\"token\"// comment\n:\"line-comment-colon-secret\"}"}`,
		},
		{
			name:   "serialized document behind a prose prefix",
			output: `{"message":"error: {\"token\":\"prose-prefix-secret\"}"}`,
		},
		{
			name:   "serialized document behind a prose line",
			output: `{"message":"provisioning failed\n{\"token\":\"prose-line-secret\"}"}`,
		},
		{
			name:   "doubly serialized object opener with no literal brace",
			output: `{"message":"\\u007b\\\"token\\\":\\\"double-serialized-secret\\\"\\u007d"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, want, redactHookOutputTokens(test.output))
		})
	}
}

// The rule reaches strings wherever they sit in the parsed document, not only
// under an object member.
func TestRedactHookOutputTokensRedactsSerializedObjectsInsideArrays(t *testing.T) {
	assert.Equal(t,
		`{"logs":["[INFO] starting","[REDACTED]"]}`,
		redactHookOutputTokens(`{"logs":["[INFO] starting","error: {\"token\":\"array-prose-secret\"}"]}`))
}
