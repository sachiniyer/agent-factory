package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Finding 3687787045 (backend_hook_remote.go:266): an unquoted property name in a
// malformed structured log leaves the parent state invalid, so the nested
// endpoint-shaped value is promoted over the real launch record on stdout.
func TestHookProvisionRejectsUnquotedEndpointPropertyInMalformedLog(t *testing.T) {
	h := newHookState(t, `
echo '{"level":INVALID, endpoint:{"url":"http://property.invalid","token":"property-secret"}}' >&2
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	p := newHookProvisioner(h, "unquoted property logger")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "an unquoted log property must not hide the launch record")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "valid endpoint output must not reap the working sandbox")
}

// Finding 3687787040 (backend_hook_redaction.go:73): a raw line break splitting an
// outer escaped quote between its backslash and quote drops the pending escape,
// so neither continuation tracking nor the raw fallback redacts the token.
func TestHookOutputSuffixRedactsTokenSplitAtOuterEscape(t *testing.T) {
	const secret = "split-outer-escape-token-must-not-leak"
	output := "INFO endpoint=\"{\\\n\"token\\\":\\\"split-outer-escape-token-must-not-leak\\\"}\""

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a line break inside the outer escape pair must not expose the token")
	assert.Contains(t, suffix, "[REDACTED]")
}

// CodeQL go/unsafe-quoting (critical) flagged rebuilding a JSON document around
// a key body taken from arbitrary hook output. Decoding happens in place now, so
// a body carrying its own quote is rejected rather than reparsed. These are the
// shapes that would match if a decoder tolerated trailing bytes after the first
// value — the latent hazard behind that alert.
func TestHookTokenKeyRejectsQuoteInjection(t *testing.T) {
	for _, body := range []string{
		`token","x`,
		`token"`,
		`to"ken`,
		`token":"leaked`,
		"token\x00",
	} {
		assert.False(t, hookTokenKeyMatches(body), "%q must not be accepted as a token key", body)
	}
	for _, body := range []string{
		`token`,
		`TOKEN`,
		`\u0074oken`,
		`\\u0074oken`,
	} {
		assert.True(t, hookTokenKeyMatches(body), "%q spells the token key", body)
	}
}

// A quote in the value must not let the surrounding diagnostic be swallowed or
// the redaction range invert — the class that previously panicked while
// formatting a provisioning error.
func TestHookOutputSuffixSurvivesQuotesInTokenValue(t *testing.T) {
	for _, output := range []string{
		`{"token":"a"b"} trailing diagnostic`,
		`"token":"` + `"""""`,
		`\"token\":\"a\"b\"`,
	} {
		require.NotPanics(t, func() { _ = hookOutputSuffix([]byte(output)) }, "output %q", output)
	}
}
