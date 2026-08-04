package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookLaunchIgnoresEndpointOnStderr is the structural claim behind #2637:
// stderr is the script's narrative, never a source of endpoints. Selecting by
// stream removes the whole family of "can a log record impersonate the endpoint"
// questions, which have no sound answer once the surrounding text is malformed.
//
// A launch_cmd whose ONLY endpoint-shaped output is on stderr must fail the
// provision rather than dial whatever that record named.
func TestHookLaunchIgnoresEndpointOnStderr(t *testing.T) {
	h := newHookState(t, `
echo '{"url":"http://stderr.invalid","token":"stderr-secret"}' >&2
exit 0
`, "")
	p := newHookProvisioner(h, "stderr only endpoint")

	_, err := p.provisionOrReap()
	require.Error(t, err, "an endpoint printed only to stderr must not be accepted")
	assert.Contains(t, err.Error(), "no {\"url\",\"token\"} JSON on stdout")
	assert.NotContains(t, err.Error(), "stderr-secret", "the reported output must redact the token")
}

// TestHookLaunchPrefersStdoutEndpointOverStderrLogs covers the reported #2637
// shape directly: a structured JSON logger on stderr around the real endpoint.
func TestHookLaunchPrefersStdoutEndpointOverStderrLogs(t *testing.T) {
	h := newHookState(t, `
echo '{"level":"info","msg":"connecting","url":"http://log.invalid","token":"log-secret"}' >&2
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
echo '{"level":"info","msg":"done","url":"http://after.invalid","token":"after-secret"}' >&2
exit 0
`, "")
	p := newHookProvisioner(h, "json logger around endpoint")

	res, err := p.provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "a working sandbox must not be reaped")
}

// TestHookScriptCapturesStreamsSeparately pins the capture itself, since every
// guarantee above rests on it. Both streams must still be reported for
// diagnostics — stderr is the only window onto a failed remote provision.
func TestHookScriptCapturesStreamsSeparately(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/streams.sh"
	writeHookScript(t, script, `
echo 'to-stdout'
echo 'to-stderr' >&2
`)

	out, _, err := runHookScriptWithResolvedEnvironment(
		hookDeleteTimeout, script, "", nil, nil, "--name", "x",
	)
	require.NoError(t, err)
	assert.Equal(t, "to-stdout", strings.TrimSpace(string(out.Stdout)))
	assert.Equal(t, "to-stderr", strings.TrimSpace(string(out.Stderr)))

	combined := string(out.Combined())
	assert.Contains(t, combined, "to-stdout")
	assert.Contains(t, combined, "to-stderr")
}

// TestHookOutputSuffixKeepsDiagnosticsIntact guards the cost side of redaction.
// This output is the only window onto a failed remote provision, so a redactor
// that eats the surrounding text is a worse outcome than the leak it prevents.
func TestHookOutputSuffixKeepsDiagnosticsIntact(t *testing.T) {
	output := strings.Join([]string{
		"provisioning vm i-0abc123",
		`{"level":"error","msg":"quota exceeded","region":"us-east-1","resource_id":9223372036854775807}`,
		"see https://example.invalid/docs for the quota request form",
	}, "\n")

	suffix := hookOutputSuffix([]byte(output))
	assert.Contains(t, suffix, "provisioning vm i-0abc123")
	assert.Contains(t, suffix, "quota exceeded")
	assert.Contains(t, suffix, "9223372036854775807", "unrelated identifiers must stay byte-exact")
	assert.Contains(t, suffix, "https://example.invalid/docs")
	assert.NotContains(t, suffix, "[REDACTED]", "output with no token must not be altered")
}

// FuzzHookOutputSuffix pins the two halves of the redaction contract that must
// never bend: reporting a hook's output must not panic, and a token in a
// well-formed endpoint record must not survive into the reported error. An
// earlier revision of this path crashed on overlapping replacements while
// formatting a provisioning error, which turns a diagnostic into an outage.
// The seed corpus runs on every `go test`.
func FuzzHookOutputSuffix(f *testing.F) {
	seeds := []struct{ prefix, token, suffix string }{
		{"", "s3cret", ""},
		{"provisioning\n", "s3cret", "\ndone"},
		{`INFO endpoint="`, "s3cret", `"`},
		{"progress { reading \"config ", "s3cret", ""},
		{strings.Repeat(`\"`, 64), "s3cret", strings.Repeat("{", 64)},
		{"{\"outer\":\"", "s3cret", "\"}"},
		{"\x00\r\n", "s3cret", "\\"},
	}
	for _, seed := range seeds {
		f.Add(seed.prefix, seed.token, seed.suffix)
	}

	f.Fuzz(func(t *testing.T, prefix, token, suffix string) {
		// A token has to be findable to be redactable: skip degenerate cases where
		// the "secret" also occurs in the surrounding diagnostic, or is so short
		// that its absence would prove nothing.
		if len(token) < 6 || strings.ContainsAny(token, "\"\\") ||
			strings.Contains(prefix, token) || strings.Contains(suffix, token) {
			return
		}
		record, err := json.Marshal(map[string]string{"url": "http://h", "token": token})
		if err != nil {
			return
		}
		output := prefix + "\n" + string(record) + "\n" + suffix

		result := hookOutputSuffix([]byte(output))
		if result == "" {
			t.Fatal("hookOutputSuffix must always say something about the hook's output")
		}
		if strings.Contains(result, token) {
			t.Fatalf("token survived redaction\n input: %q\noutput: %q", output, result)
		}
	})
}
