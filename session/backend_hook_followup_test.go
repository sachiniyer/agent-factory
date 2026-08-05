package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review findings from #2718 and #2841 on hook stdout handling.
//
// The endpoint-SELECTION findings in that series are gone from this file, not
// dropped: stdout is the endpoint record's alone since #2845, so every input
// that once needed a rule about where a record ends and a log begins is now one
// refusal, and all of them are pinned by value in
// TestHookLaunchRefusesEveryHistoricalSharedStdout.
//
// What stays here is REDACTION, which is unaffected by that change — a hook's
// output still lands in a persisted error, and still must not carry a token
// there — plus the bound on how much work parsing a flooded stdout may cost.

// Codex P2 3716111944 on #2718. extractJSONAt reported the enclosing wrapper's
// end alongside a recovered child, so redactCompleteHookJSON rewrote the wrong
// byte span and mangled the diagnostic it was meant to preserve.
func TestHookOutputSuffixKeepsTextAroundRecoveredJSON(t *testing.T) {
	output := `noise { bad {"token":"nested-secret"} tail-marker-must-survive }`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, "nested-secret")
	assert.Contains(t, suffix, "[REDACTED]")
	assert.Contains(t, suffix, "tail-marker-must-survive", "surrounding diagnostic text must survive redaction")
	assert.Contains(t, suffix, "noise", "text before the recovered value must survive too")
}

// Codex P2 3716111947 on #2718. A token value truncated at a line break made the
// joined-line scan swallow every later line up to the next quote, replacing the
// actual failure with [REDACTED].
func TestHookOutputSuffixKeepsLaterLinesAfterTruncatedToken(t *testing.T) {
	output := strings.Join([]string{
		`{"url":"http://h","token":"truncated-secret`,
		`provisioner error: quota exceeded in region us-east-1`,
		`see the "help" page for the quota request form`,
	}, "\n")

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, "truncated-secret", "the truncated token must still be redacted")
	assert.Contains(t, suffix, "quota exceeded in region us-east-1",
		"the real failure must not be redacted away with the token")
	assert.Contains(t, suffix, "quota request form")
}

// Codex P2 3716111952 + 3716388272 on #2718: a serialized endpoint whose token
// key is Unicode-escaped, and one escaped two levels deep. Both exceeded the old
// key bound or kept a backslash in the key body, so neither matched.
func TestHookOutputSuffixRedactsDeeplyEscapedTokenKeys(t *testing.T) {
	tests := []struct {
		name   string
		output string
		secret string
	}{
		{
			name:   "serialized unicode-escaped key",
			output: `INFO endpoint="{\"\\u0074\\u006f\\u006b\\u0065\\u006e\":\"unicode-key-secret\"}"`,
			secret: "unicode-key-secret",
		},
		{
			name:   "twice-escaped token field",
			output: `INFO endpoint="{\\\"token\\\":\\\"twice-escaped-secret\\\"}"`,
			secret: "twice-escaped-secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suffix := hookOutputSuffix([]byte(test.output))
			assert.NotContains(t, suffix, test.secret)
			assert.Contains(t, suffix, "[REDACTED]")
		})
	}
}

// P2 3716726118 / 3716726119 / 3716833785: a token hard-wrapped by a logger —
// once, several times, with CRLF, or wrapped and then truncated — left its tail
// in the error. The continuation is bounded by whitespace, which a bearer token
// never contains.
func TestHookOutputSuffixRedactsWrappedTokenTails(t *testing.T) {
	tests := []struct{ name, output, secret string }{
		{"crlf wrap", "{\"token\":\"prefix-\r\nsecret-tail\"}", "secret-tail"},
		{"multiple wraps", "{\"token\":\"a-\nsecret-middle-\nsecret-tail\"}", "secret-middle-"},
		{"wrapped and truncated", "{\"token\":\"prefix-\nsecret-tail", "secret-tail"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suffix := hookOutputSuffix([]byte(test.output))
			assert.NotContains(t, suffix, test.secret)
			assert.Contains(t, suffix, "[REDACTED]")
		})
	}
}

// P2 3716833781: the advertised depth-3 Unicode-escaped key must actually be
// redacted end to end, not merely fit the widened scan window.
func TestHookOutputSuffixRedactsDepthThreeUnicodeTokenKey(t *testing.T) {
	output := `INFO endpoint="{\\\"\\\\u0074\\\\u006f\\\\u006b\\\\u0065\\\\u006e\\\":\\\"depth3-secret\\\"}"`
	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, "depth3-secret")
	assert.Contains(t, suffix, "[REDACTED]")
}

// P2 3716996290: the wrap continuation ran past a value that was already
// complete, eating the diagnostic that followed it.
func TestHookOutputSuffixKeepsTextAfterCompleteEscapedToken(t *testing.T) {
	output := `INFO endpoint=\"token\":\"secret\"error=quota-exceeded`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, "secret")
	assert.Contains(t, suffix, "[REDACTED]")
	assert.Contains(t, suffix, "error=quota-exceeded", "a complete value must not swallow the diagnostic after it")
}

// P2 3717054757: a wrapped value whose continuation is indented.
func TestHookOutputSuffixRedactsIndentedWrappedToken(t *testing.T) {
	suffix := hookOutputSuffix([]byte("{\"token\":\"prefix-\n  secret-tail\"}"))
	assert.NotContains(t, suffix, "secret-tail")
	assert.Contains(t, suffix, "[REDACTED]")
}

// P2 3717054755: a complete escaped field must keep its end even when the output
// has other lines, which is what put it through the joined pass.
func TestHookOutputSuffixKeepsTextAfterCompleteTokenWithOtherLines(t *testing.T) {
	suffix := hookOutputSuffix([]byte(`INFO endpoint=\"token\":\"secret\"error=quota-exceeded` + "\nnext line"))
	assert.NotContains(t, suffix, "secret")
	assert.Contains(t, suffix, "error=quota-exceeded")
	assert.Contains(t, suffix, "next line")
}

// P2 3717118577: a hard-wrapped SERIALIZED token ends at its escaped closing
// quote; stepping over it ran the redaction through the failure detail.
func TestHookOutputSuffixStopsWrappedTokenAtEscapedQuote(t *testing.T) {
	suffix := hookOutputSuffix([]byte("INFO endpoint=\\\"token\\\":\\\"se\ncret\\\"error=quota-exceeded"))
	assert.NotContains(t, suffix, "cret\\\"error", "the wrapped token tail must be redacted")
	assert.Contains(t, suffix, "error=quota-exceeded", "the failure detail must survive")
}

// P2 3717054761: 20k column-0 JSON-prefix lines made the old scan build a
// decoder over the whole remaining suffix per line, ~4s of a launch budget spent
// selecting an endpoint. stdout is now parsed once as a single value (#2845), so
// a flood costs one pass over it whatever the lines look like — and every one of
// these floods is a contract violation, because none of them is endpoint-only.
func TestHookEndpointParseBoundedOnJSONPrefixFlood(t *testing.T) {
	floods := map[string]string{
		"bracket lines": strings.Repeat("[\n", 20_000),
		"prose lines":   strings.Repeat("tunnel forwarding\n", 20_000),
		"object lines":  strings.Repeat("{\n", 20_000),
	}
	for name, prefix := range floods {
		t.Run(name, func(t *testing.T) {
			flood := prefix + `{"url":"http://10.0.0.7:8080","token":"secret"}` + "\n"
			started := time.Now()
			endpoint, _, violation := parseHookEndpoint(flood)
			assert.Less(t, time.Since(started), time.Second, "a flood must cost one pass, not one per line")
			assert.Nil(t, endpoint, "stdout carrying more than the endpoint yields none")
			require.NotNil(t, violation, "and reports the contract violation instead")
		})
	}
}
