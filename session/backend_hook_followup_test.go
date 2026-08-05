package session

import (
	"strings"
	"testing"
	"time"

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
	res, err := newHookProvisioner(h, "malformed stdout log").provisionOrReap()

	// The malformed record closes on its own line, so skipping that LINE is safe:
	// its nested endpoint never becomes a candidate, and later lines stay
	// trustworthy. The real record is still selected.
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
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
	_, err := newHookProvisioner(h, "mismatched closer").provisionOrReap()

	// encoding/json rejects the record; selection stops rather than re-framing its child.
	require.Error(t, err, "a malformed JSON record on stdout must not be salvaged past")
	// The logged URL may appear in the echoed output — that IS the diagnostic.
	// What must not happen is selecting it, so assert the provision reported no
	// endpoint rather than a failure to reach one, and that the token is redacted.
	assert.Contains(t, err.Error(), `printed no {"url","token"} JSON on stdout`,
		"selection must stop at the malformed record, not dial a logged endpoint")
	assert.NotContains(t, err.Error(), "logged-secret", "the logged token must not reach the error")
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

// P1 3716996280 / 3717054748: a log that breaks before its endpoint value hands
// that value its own line, indented or not. No positional rule separates it from
// a record, so selection stops at the malformed record instead of guessing.
func TestHookLaunchRejectsIndentedNestedEndpoint(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"level":INVALID,"endpoint":' '  {"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "multiline nested log").provisionOrReap()

	// Indented or not, a continuation of a malformed record is never promoted.
	require.Error(t, err, "a malformed JSON record on stdout must not be salvaged past")
	// The logged URL may appear in the echoed output — that IS the diagnostic.
	// What must not happen is selecting it, so assert the provision reported no
	// endpoint rather than a failure to reach one, and that the token is redacted.
	assert.Contains(t, err.Error(), `printed no {"url","token"} JSON on stdout`,
		"selection must stop at the malformed record, not dial a logged endpoint")
	assert.NotContains(t, err.Error(), "logged-secret", "the logged token must not reach the error")
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

	// Several values on one line is a log line, not a record: none of them is
	// promoted, and the real record on the next line is.
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

// Fourth review round on #2841.

// P1 3717054748: the continuation of a malformed record need not be indented, so
// column position cannot separate it from a record. Selection stops at the
// malformed record; the unindented variant must not dial the logged URL either.
func TestHookLaunchRejectsUnindentedNestedEndpoint(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"level":INVALID,"endpoint":' '{"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "unindented nested").provisionOrReap()

	require.Error(t, err)
	assert.Contains(t, err.Error(), `printed no {"url","token"} JSON on stdout`,
		"selection must stop at the malformed record, not dial a logged endpoint")
	assert.NotContains(t, err.Error(), "logged-secret")
}

// Prose on stdout is SUPPORTED: docs/remote-hooks.md lets a background tunnel
// inherit the stream and keep logging. It must not disturb selection — this is
// the case a fail-at-first-syntax-error rule broke.
func TestHookLaunchIgnoresTunnelProseOnStdout(t *testing.T) {
	h := newHookState(t, `
echo 'tunnel forwarding'
echo 'tunnel forwarding'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
echo 'tunnel still forwarding'
exit 0
`, "")
	res, err := newHookProvisioner(h, "tunnel prose").provisionOrReap()
	require.NoError(t, err, "tunnel chatter on stdout must not fail the provision")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.False(t, h.deleteRan(t))
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

// P2 3717054761: 20k column-0 JSON-prefix lines made the old scan build a
// decoder over the whole remaining suffix per line, ~4s of a launch budget spent
// selecting an endpoint. Selection stops at the first malformed record now, so
// the work is bounded by where that record begins.
func TestHookEndpointSelectionBoundedOnJSONPrefixFlood(t *testing.T) {
	// Bracket lines are prose — an endpoint is an object — so a flood of them is
	// skipped without parsing and the endpoint is still found.
	brackets := strings.Repeat("[\n", 20_000) + `{"url":"http://10.0.0.7:8080","token":"secret"}` + "\n"
	started := time.Now()
	endpoint, _ := selectHookEndpoint(brackets)
	assert.Less(t, time.Since(started), time.Second, "bracket lines must not be parsed per line")
	require.NotNil(t, endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", endpoint.URL)

	// Prose likewise.
	prose := strings.Repeat("tunnel forwarding\n", 20_000) + `{"url":"http://10.0.0.7:8080","token":"secret"}` + "\n"
	started = time.Now()
	endpoint, _ = selectHookEndpoint(prose)
	assert.Less(t, time.Since(started), time.Second)
	require.NotNil(t, endpoint)

	// An unterminated OBJECT is a record that broke, and stops selection — the
	// scan must reach that conclusion quickly rather than rescanning the suffix.
	objects := strings.Repeat("{\n", 20_000) + `{"url":"http://10.0.0.7:8080","token":"secret"}` + "\n"
	started = time.Now()
	endpoint, _ = selectHookEndpoint(objects)
	assert.Less(t, time.Since(started), 2*time.Second, "an open record must not rescan the suffix")
	assert.Nil(t, endpoint, "an unterminated record stops selection rather than promoting past it")
}

// Fifth review round on #2841.

// P2 3717118574: an endpoint line with leading JSON whitespace was classified as
// prose and skipped, failing a launch that printed a valid endpoint.
func TestHookLaunchAcceptsIndentedEndpointLine(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '  {"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "indented endpoint").provisionOrReap()
	require.NoError(t, err, "leading whitespace must not make an endpoint line prose")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
}

// P2 3717118585: bracketed prose at column 0 is the most common tunnel log
// prefix there is. It must not poison the rest of stdout.
func TestHookLaunchIgnoresBracketedProseOnStdout(t *testing.T) {
	h := newHookState(t, `
echo '[INFO] tunnel forwarding'
echo '[WARN] retrying'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "bracketed prose").provisionOrReap()
	require.NoError(t, err, "bracketed prose must not stop endpoint selection")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.False(t, h.deleteRan(t))
}

// P1 3717118579: a line whose valid JSON prefix is followed by a continuation
// (`{"level":"info"},"endpoint":`) leaves the record open, so later lines carry
// its contents and must not be promoted.
func TestHookLaunchRejectsEndpointAfterTrailingContinuation(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"level":"info"},"endpoint":' '{"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "trailing continuation").provisionOrReap()
	require.Error(t, err, "a record left open by trailing text must stop selection")
	// sawJSON is true here — the `{"level":"info"}` prefix decoded — so this is
	// the "printed JSON but none matched" variant. What matters is that neither
	// the logged endpoint nor the later real one was promoted past the open
	// record, and that the token is redacted from the reported output.
	assert.Contains(t, err.Error(), "none contained a non-empty url and token")
	assert.NotContains(t, err.Error(), "logged-secret")
}

// P2 3717118577: a hard-wrapped SERIALIZED token ends at its escaped closing
// quote; stepping over it ran the redaction through the failure detail.
func TestHookOutputSuffixStopsWrappedTokenAtEscapedQuote(t *testing.T) {
	suffix := hookOutputSuffix([]byte("INFO endpoint=\\\"token\\\":\\\"se\ncret\\\"error=quota-exceeded"))
	assert.NotContains(t, suffix, "cret\\\"error", "the wrapped token tail must be redacted")
	assert.Contains(t, suffix, "error=quota-exceeded", "the failure detail must survive")
}

// Sixth review round on #2841.

// P1 3717248242: a multi-line log record was decoded whole but the cursor
// advanced only one line, so the lines INSIDE it were rescanned and its
// endpoint-shaped child promoted as a record of its own.
func TestHookLaunchRejectsChildOfMultilineStdoutLog(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{' '  "level": "info",' '  "endpoint":' '  {"url":"http://wrong.invalid","token":"logged-secret"}' '}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "multiline log wrapper").provisionOrReap()

	require.NoError(t, err, "a complete multi-line log must be skipped whole, not rescanned")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t))
}

// P1 3717248248: an endpoint-shaped prefix followed by a continuation on the
// same line was accepted before the trailing-byte check could reject it.
func TestHookLaunchRejectsEndpointShapedPrefixWithContinuation(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"url":"http://wrong.invalid","token":"logged-secret"},"endpoint":'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "endpoint prefix continuation").provisionOrReap()

	require.Error(t, err, "an endpoint-shaped prefix of an open record is not a record")
	assert.NotContains(t, err.Error(), "logged-secret", "the logged token must not reach the error")
}

// Seventh review round on #2841.

// P1 3717302082: a depth counter called `{"level":]` balanced, so a record that
// was still open looked self-contained and the next line's logged endpoint was
// promoted. Bracket TYPES have to match.
func TestHookLaunchRejectsEndpointAfterMismatchedBracketLine(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{"level":]' '{"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "mismatched bracket line").provisionOrReap()

	require.Error(t, err, "a mismatched closer leaves the record open")
	assert.NotContains(t, err.Error(), "logged-secret", "the logged token must not reach the error")
}

// P2 3717302086: bracketed prose with a stray quote had already closed its
// brackets; treating the open string as a continuing record stopped selection
// and reaped a sandbox that launched correctly.
func TestHookLaunchIgnoresBracketedProseWithStrayQuote(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '[INFO] opening "config'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "prose stray quote").provisionOrReap()

	require.NoError(t, err, "a stray quote in closed bracketed prose must not stop selection")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.False(t, h.deleteRan(t))
}

// Eighth review round on #2841. Both findings are the prose-on-stdout path
// breaking again — the fourth and fifth time in this PR — so they are pinned
// together with the earlier prose cases as one set.
func TestHookLaunchIgnoresProseThatOnlyLooksLikeJSON(t *testing.T) {
	tests := []struct{ name, prose string }{
		{"bracket prefix with unmatched brace", `[INFO] opening {config`},
		{"numeric bracket prefix", `[2026] tunnel forwarding`},
		{"bracket prefix with stray quote", `[INFO] opening "config`},
		{"plain bracket prefix", `[INFO] tunnel forwarding`},
		{"plain prose", `tunnel forwarding`},
		{"brace prefix", `{config} loaded`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHookState(t, "printf '%s\\n' '"+test.prose+"'\n"+
				`echo '{"url":"http://10.0.0.7:8080","token":"secret"}'`+"\nexit 0\n", "")
			res, err := newHookProvisioner(h, "prose "+test.name).provisionOrReap()

			require.NoError(t, err, "prose on stdout must never stop endpoint selection")
			require.NotNil(t, res.Endpoint)
			assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
			assert.False(t, h.deleteRan(t), "a working sandbox must not be reaped")
		})
	}
}

// The security direction must still hold against every prose shape above: a
// record that genuinely broke still stops selection.
func TestHookLaunchStillRejectsBrokenRecordsAfterProseFix(t *testing.T) {
	h := newHookState(t, `
echo '[INFO] tunnel forwarding'
printf '%s\n' '{"level":INVALID,"endpoint":' '{"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "prose then broken record").provisionOrReap()

	require.Error(t, err, "a record that broke still stops selection")
	assert.NotContains(t, err.Error(), "logged-secret")
}

// Ninth review round on #2841.

// P1 3717439335: `{level:INVALID,…` dies on its first token exactly as `[INFO] …`
// does, so an offset-based prose test could not separate them. An endpoint is an
// OBJECT, so a `{` line can be a broken record and a `[` line never is.
func TestHookLaunchRejectsUnbalancedBraceLogLine(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{level:INVALID,"endpoint":' '{"url":"http://wrong.invalid","token":"logged-secret"}'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "unbalanced brace log").provisionOrReap()

	require.Error(t, err, "an unbalanced brace-started log is a record that broke")
	assert.NotContains(t, err.Error(), "logged-secret")
}

// P1 3717439336: the continuation separator moved onto its own line bypassed a
// trailing check that only looked at the value's own line.
func TestHookLaunchRejectsEndpointBeforeContinuationLine(t *testing.T) {
	for _, sep := range []string{`,"endpoint":`, `:`, `}`, `]`} {
		t.Run(sep, func(t *testing.T) {
			h := newHookState(t, "printf '%s\\n' '{\"url\":\"http://wrong.invalid\",\"token\":\"logged-secret\"}' '"+sep+"'\n"+
				`echo '{"url":"http://10.0.0.7:8080","token":"secret"}'`+"\nexit 0\n", "")
			_, err := newHookProvisioner(h, "continuation line "+sep).provisionOrReap()

			require.Error(t, err, "a value followed by JSON structure is not a record")
			assert.NotContains(t, err.Error(), "logged-secret")
		})
	}
}

// P2 3717439337: punctuated bracketed prose is still prose.
func TestHookLaunchIgnoresPunctuatedBracketProse(t *testing.T) {
	for _, prose := range []string{`[2026], tunnel forwarding`, `[2026]: tunnel forwarding`, `[2026]; forwarding`} {
		t.Run(prose, func(t *testing.T) {
			h := newHookState(t, "printf '%s\\n' '"+prose+"'\n"+
				`echo '{"url":"http://10.0.0.7:8080","token":"secret"}'`+"\nexit 0\n", "")
			res, err := newHookProvisioner(h, "punctuated prose").provisionOrReap()

			require.NoError(t, err, "punctuated bracketed prose must not stop selection")
			require.NotNil(t, res.Endpoint)
			assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
		})
	}
}
