package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

func TestAttachVSCodeTabUsesDaemonMintedStableID(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	inst := startedMockInstance(t, "af_vscode_response_id")

	tab, err := inst.AttachVSCodeTab("editor", "daemon-vscode-id")
	require.NoError(t, err)
	require.Equal(t, "daemon-vscode-id", tab.ID)

	_, err = inst.AttachVSCodeTab("editor", "same-name-replacement-id")
	require.Error(t, err,
		"a raced same-name projection must not replace the identity returned by CreateTab")
}

func TestAttachShellTabUsesDaemonMintedStableID(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	const agentName = "af_shell_response_id"
	inst := startedMockInstance(t, agentName, agentName+"__shell")

	tab, err := inst.AttachShellTab("shell", "", "daemon-shell-id")
	require.NoError(t, err)
	require.Equal(t, "daemon-shell-id", tab.ID)

	_, err = inst.AttachShellTab("shell", "", "same-name-replacement-id")
	require.Error(t, err,
		"a raced same-name projection must not replace the identity returned by CreateTab")
}

func TestAttachTabPreservesOlderDaemonEmptyIDFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	inst := startedMockInstance(t, "af_vscode_legacy_response")

	tab, err := inst.AttachVSCodeTab("editor", "")
	require.NoError(t, err)
	require.Empty(t, tab.ID,
		"a response from an older daemon has no authoritative id to invent locally")
}
