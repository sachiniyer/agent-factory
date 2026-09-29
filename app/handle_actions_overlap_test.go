package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebTabAttachGuardRedactsOverlapPercentEscape is the end-to-end emitter
// smoke for app/handle_actions.go:832's
// `agentproto.RedactAccessTokenURL(tabs[tabIdx].URL)` — the "this is a web tab"
// CLI error string. It mirrors the existing
// TestWebTabAttachGuardDoesNotExposeAccessToken but drives the URL shape that
// defeated the decode-based redactor: a valid %HH escape (here %ac) whose hex
// digits overlap the leading characters of an otherwise-literal
// `access_token<...>` substring. Without the raw-bytes scan fallback in
// redactAccessTokenQueryPair, the redacted URL — and so the error string this
// guard returns — would contain the literal `access_token=<value>` text from
// the raw query bytes that net/url re-emits verbatim from RawQuery.
func TestWebTabAttachGuardRedactsOverlapPercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-guard-overlap"
	inst := startedLocalInstance(t, "web-host-overlap")
	_, err := inst.AddWebTab("http://localhost:3000/app?%access_token="+secret+"&view=2", "")
	require.NoError(t, err)

	err = webTabAttachGuard(inst, len(inst.GetTabs())-1)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret,
		"the overlap %HH shape must not let the literal token value survive the guard")
	assert.Contains(t, err.Error(), "access_token=REDACTED",
		"the overlap %HH shape must still be redacted, not merely dropped")
}
