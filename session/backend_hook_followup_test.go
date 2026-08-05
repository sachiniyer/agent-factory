package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Codex P1 3716111938 on #2718. selectHookEndpoint used the diagnostic
// extractor, which recovers values nested in malformed wrappers, so a malformed
// structured log on STDOUT could still promote its logged endpoint — the #2637
// failure mode, reachable again through the stream that was supposed to fix it.
func TestHookLaunchRejectsNestedEndpointInMalformedStdoutLog(t *testing.T) {
	h := newHookState(t, `
echo '{"level":INVALID,"endpoint":{"url":"http://wrong.invalid","token":"logged-secret"}}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	p := newHookProvisioner(h, "malformed stdout log")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "a malformed stdout log must not hide the launch record")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "the working sandbox must not be reaped")
}

// A malformed wrapper with no real endpoint after it must fail the provision
// rather than dial whatever the log named.
func TestHookLaunchRejectsNestedEndpointWithNoRealRecord(t *testing.T) {
	h := newHookState(t, `
echo '{"level":INVALID,"endpoint":{"url":"http://wrong.invalid","token":"logged-secret"}}'
exit 0
`, "")
	p := newHookProvisioner(h, "nested only")

	_, err := p.provisionOrReap()
	require.Error(t, err, "a nested endpoint is not a launch record")
	assert.NotContains(t, err.Error(), "logged-secret", "the reported output must redact the token")
}

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

// Second review round on #2841.

// P1 3716833778: a mismatched closer let the hand-rolled scan pop the record's
// opener, re-framing the log's nested endpoint as a top-level value.
func TestHookLaunchRejectsNestedEndpointAfterMismatchedCloser(t *testing.T) {
	h := newHookState(t, `
echo '{"level":],"endpoint":{"url":"http://wrong.invalid","token":"logged-secret"}}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "mismatched closer").provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
}

// P2 3716726113: an unterminated JSON-ish log on stdout left openers on the
// stack to EOF, so a launch that DID echo an endpoint was reported as printing
// none — and its working sandbox reaped.
func TestHookLaunchRecoversAfterUnterminatedStdoutLog(t *testing.T) {
	for _, prefix := range []string{"progress {{", `partial {"level":`, "noise ["} {
		t.Run(prefix, func(t *testing.T) {
			h := newHookState(t, "echo '"+prefix+"'\n"+
				`echo '{"url":"http://10.0.0.7:8080","token":"secret"}'`+"\nexit 0\n", "")
			res, err := newHookProvisioner(h, "unterminated stdout "+prefix).provisionOrReap()
			require.NoError(t, err, "an unterminated stdout log must not hide the endpoint")
			require.NotNil(t, res.Endpoint)
			assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
			assert.False(t, h.deleteRan(t), "a working sandbox must not be reaped")
		})
	}
}

// A pretty-printed endpoint still parses: the line-anchored rule requires the
// value to BEGIN a line, not to fit on one.
func TestHookLaunchAcceptsPrettyPrintedEndpoint(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{' '  "url": "http://10.0.0.7:8080",' '  "token": "secret"' '}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "pretty endpoint").provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
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

// Third review round on #2841.

// P1 3716996280: a log that breaks before its endpoint value hands that value
// its own line, so a line-start rule alone read it as a record. Column 0 is the
// discriminator — a logger indents a continued value; echo does not.
func TestHookLaunchRejectsIndentedNestedEndpoint(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"level":INVALID,"endpoint":' '  {"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "multiline nested log").provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
}

// P1 3716996283: after skipping a non-endpoint value the scan resumed at the
// caller's cursor, so a second object on the SAME physical line looked like a
// fresh record.
func TestHookLaunchRejectsSecondObjectOnSameLine(t *testing.T) {
	h := newHookState(t, `
echo '{"level":"info"} {"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "same line pair").provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
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
