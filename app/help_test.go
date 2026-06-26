package app

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestGeneralHelpNavigationMatchesBindings guards against regressing #764, where
// the help screen documented "↑/j, ↓/k" while the canonical bindings in
// keys/keys.go map k=up and j=down (standard vim convention).
func TestGeneralHelpNavigationMatchesBindings(t *testing.T) {
	content := helpTypeGeneral{}.toContent()

	if !strings.Contains(content, "↑/k, ↓/j") {
		t.Errorf("help content missing canonical navigation label \"↑/k, ↓/j\"; got:\n%s", content)
	}
	if strings.Contains(content, "↑/j, ↓/k") {
		t.Errorf("help content contains reversed navigation label \"↑/j, ↓/k\" (see #764); got:\n%s", content)
	}
}

// TestInstanceStartHelpRemoteOmitsUnsupportedTabKeys guards against regressing
// #988: remote instances block `t` (new tab) and `w` (close tab) — those
// handlers reject IsRemote() with an error — so the instance-start help must
// only advertise the tab keys that actually work (cycle / 1-9 jump). Local
// instances keep the full hint.
func TestInstanceStartHelpRemoteOmitsUnsupportedTabKeys(t *testing.T) {
	remote := newStartedInstance(t, "remote")
	remote.SetBackend(&session.HookBackend{})
	require.True(t, remote.IsRemote(), "sanity: instance should report as remote")

	remoteContent := helpStart(remote).toContent()
	if strings.Contains(remoteContent, "t new tab") || strings.Contains(remoteContent, "w close") {
		t.Errorf("remote instance-start help must not advertise unsupported t/w tab keys; got:\n%s", remoteContent)
	}
	if !strings.Contains(remoteContent, "1-9 jump") {
		t.Errorf("remote instance-start help should still advertise the supported 1-9 jump; got:\n%s", remoteContent)
	}

	local := newStartedInstance(t, "local")
	require.False(t, local.IsRemote(), "sanity: instance should report as local")

	localContent := helpStart(local).toContent()
	if !strings.Contains(localContent, "t new tab") || !strings.Contains(localContent, "w close") {
		t.Errorf("local instance-start help should advertise the full t/w/1-9 tab hint; got:\n%s", localContent)
	}
}
