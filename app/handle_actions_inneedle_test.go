package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebTabAttachGuardRedactsInNeedlePercentEscape is the in-needle-escape
// sibling of TestWebTabAttachGuardRedactsOverlapPercentEscape. It drives
// app/handle_actions.go:832's `agentproto.RedactAccessTokenURL(tabs[tabIdx].URL)`
// — the "this is a web tab" CLI error string — with the combined shape the
// #4663 raw-bytes fallback could not catch: a leading-overlap `%ac` (collapses
// the 'a' in the decoded view) PLUS an in-needle `%5F` (spells the '_'
// inside the needle, so the raw bytes carry "access%5Ftoken=" not the literal
// "access_token="). Without the percent-tolerant raw matcher, the error
// string would contain the literal `access_token=<value>` text verbatim
// from the raw query bytes that net/url re-emits. With the fix, the matched
// key form is preserved and only the value is rewritten.
func TestWebTabAttachGuardRedactsInNeedlePercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-guard-inneedle"
	inst := startedLocalInstance(t, "web-host-inneedle")
	_, err := inst.AddWebTab("http://localhost:3000/app?%access%5Ftoken="+secret+"&view=2", "")
	require.NoError(t, err)

	err = webTabAttachGuard(inst, len(inst.GetTabs())-1)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret,
		"the in-needle %HH shape must not let the literal token value survive the guard")
	// The matched key form is preserved verbatim, so the redaction marker
	// carries the in-needle escape (`access%5Ftoken=REDACTED`, not the literal
	// `access_token=REDACTED`).
	assert.Contains(t, err.Error(), "access%5Ftoken=REDACTED",
		"the in-needle %HH shape must still be redacted, not merely dropped")
}

// TestWebTabAttachGuardRedactsInNeedlePercentEscapeSecondNibble is the
// non-`%5F` sanity check for the in-needle family. Per the bug report, "%5F
// does not need to be rare: any nibble works (`%5F`→`_`, `%73`→`s`, `%65`→
// `e`, `%6F`→`o` etc.)". Drive the same guard with `%acces%73_token=` to pin
// that the fix is not nibble-specific.
func TestWebTabAttachGuardRedactsInNeedlePercentEscapeSecondNibble(t *testing.T) {
	const secret = "af-sentinel-webtab-guard-inneedle-s"
	inst := startedLocalInstance(t, "web-host-inneedle-s")
	_, err := inst.AddWebTab("http://localhost:3000/app?%acces%73_token="+secret, "")
	require.NoError(t, err)

	err = webTabAttachGuard(inst, len(inst.GetTabs())-1)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret,
		"the in-needle %73 shape must not let the literal token value survive the guard")
	assert.Contains(t, err.Error(), "acces%73_token=REDACTED",
		"the in-needle %73 shape must still be redacted, not merely dropped")
}

// TestWebTabAttachGuardRedactsInNeedlePercentEscapeComponent exercises the
// component sweep (path) variant: the matched key form is preserved in the
// path component, not the query. scanned thinking: the existing overlap test
// only covers the query; the path component emitter at handle_actions.go:832
// prints the redacted URL verbatim, so the path-only shape is a fair e2e
// regression even though the original bug report cites the query shape.
func TestWebTabAttachGuardRedactsInNeedlePercentEscapeComponent(t *testing.T) {
	const secret = "af-sentinel-webtab-guard-inneedle-path"
	inst := startedLocalInstance(t, "web-host-inneedle-path")
	_, err := inst.AddWebTab("http://localhost:3000/path/%access%5Ftoken="+secret, "")
	require.NoError(t, err)

	err = webTabAttachGuard(inst, len(inst.GetTabs())-1)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret,
		"the in-needle path-component shape must not let the literal token value survive the guard")
	if !strings.Contains(err.Error(), "access%5Ftoken=REDACTED") {
		t.Errorf("guard error redacts/normalizes the key form: %q\n"+
			"want it to contain access%%5Ftoken=REDACTED with the key form preserved",
			err.Error())
	}
}
