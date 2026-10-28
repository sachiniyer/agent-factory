package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The callback-absence reason is durable (#5571 W3.1): it is recorded the
// moment the sandbox provisions callback-less and must survive the storage
// round-trip, or a daemon restart would silently forget why the session's
// agent cannot call af — the exact silence the feature exists to prevent.
func TestCallbackUnavailableSurvivesStorageRoundTrip(t *testing.T) {
	const reason = "refusing to give this sandbox a callback credential: require_token is false"
	data := deadInstanceData(t, Archived, "af_cb_agent", "af_cb_shell")
	data.CallbackUnavailable = reason

	stored := data.ForStorage()
	require.Equal(t, reason, stored.CallbackUnavailable,
		"the reason must reach the durable record — unlike ArchiveWarning it is not a live-only projection")

	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	assert.Equal(t, reason, restored.CallbackUnavailable())
	assert.Equal(t, reason, restored.ToInstanceData().CallbackUnavailable,
		"a thin client rebuilding a live snapshot must not drop the callback notice either")
}

// ReconcileCallbackUnavailable is the client-projection axis (app/sync.go):
// the daemon's snapshot reason mirrors onto a live instance without touching
// liveness, and clears again when a healthy reprovision removes it.
func TestReconcileCallbackUnavailableMirrorsAndClearsProjection(t *testing.T) {
	inst := &Instance{}
	require.True(t, inst.ReconcileCallbackUnavailable("the listener moved"))
	require.Equal(t, "the listener moved", inst.CallbackUnavailable())
	require.False(t, inst.ReconcileCallbackUnavailable(inst.CallbackUnavailable()),
		"an unchanged projection must not dirty the row")
	require.True(t, inst.ReconcileCallbackUnavailable(""))
	require.Empty(t, inst.CallbackUnavailable())
}

// The provisioning path's reason wins over the generic in-sandbox one: it
// names the actual refusal, while InstanceOptions.CallbackUnavailable can only
// say "nothing arrived".
func TestCallbackUnavailableReasonForPrefersTheProvisionReason(t *testing.T) {
	assert.Equal(t, "the mint refused",
		callbackUnavailableReasonFor(
			InstanceOptions{CallbackUnavailable: true},
			ProvisionResult{CallbackUnavailable: "the mint refused"},
		))
	assert.Equal(t, callbackUndeliveredReason,
		callbackUnavailableReasonFor(InstanceOptions{CallbackUnavailable: true}, ProvisionResult{}))
	assert.Empty(t, callbackUnavailableReasonFor(InstanceOptions{}, ProvisionResult{}))
	assert.Empty(t, callbackUnavailableReasonFor(
		InstanceOptions{}, ProvisionResult{CallbackUnavailable: ""}))
}

// The callbackless guidance must exist on BOTH per-instance seams (#5571 W3.1):
// a session with no af callback must not be taught af verbs that all fail.
// Claude reads commands/af.md through --plugin-dir; aider reads the same
// document through --read.
//
// Each variant owns a DISTINCT path: sessions sharing a remote user's config
// dir would otherwise flap, the later launch rewriting the file the earlier
// session then reads (PR #5577 review).
func TestEnsurePluginDirIsolatesTheCallbacklessGuidance(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	lessDir, err := ensurePluginDir(noCallbackGuidanceCaps)
	require.NoError(t, err)
	assert.Contains(t, lessDir, "plugin-callbackless")

	body, err := os.ReadFile(filepath.Join(lessDir, "commands", "af.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "cannot reach the daemon",
		"the callbackless variant must say plainly why af verbs are omitted")
	assert.NotContains(t, string(body), "af tasks list",
		"a callbackless session must not be taught verbs that all fail")

	// A callback-enabled session on the same host gets its own dir — and the
	// callbackless file beside it must be left exactly as written.
	fullDir, err := ensurePluginDir(fullGuidanceCaps)
	require.NoError(t, err)
	assert.NotEqual(t, lessDir, fullDir)
	fullBody, err := os.ReadFile(filepath.Join(fullDir, "commands", "af.md"))
	require.NoError(t, err)
	assert.Contains(t, string(fullBody), "af tasks list")

	body, err = os.ReadFile(filepath.Join(lessDir, "commands", "af.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "cannot reach the daemon",
		"a later full-variant launch must not rewrite the callbackless plugin")
}

func TestEnsureAiderReadFileIsolatesTheCallbacklessGuidance(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	lessPath, err := ensureAiderReadFile(noCallbackGuidanceCaps)
	require.NoError(t, err)
	assert.Contains(t, lessPath, "af-skill-callbackless.md")

	body, err := os.ReadFile(lessPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "cannot reach the daemon")
	assert.NotContains(t, string(body), "af tasks list")

	fullPath, err := ensureAiderReadFile(fullGuidanceCaps)
	require.NoError(t, err)
	assert.NotEqual(t, lessPath, fullPath)
	fullBody, err := os.ReadFile(fullPath)
	require.NoError(t, err)
	assert.Contains(t, string(fullBody), "af tasks list")

	body, err = os.ReadFile(lessPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "cannot reach the daemon",
		"a later full-variant launch must not rewrite the callbackless file")
}

// The doc the two seams share must hold both halves of the contract itself:
// the inside framing stays (the agent IS an af instance), while every verb is
// withheld in favour of "do your work and report".
func TestCallbacklessReferenceExplainsTheAbsence(t *testing.T) {
	assert.Contains(t, afUsageReferenceCallbackless, "you are one of them")
	assert.Contains(t, afUsageReferenceCallbackless, "cannot reach the daemon")
	assert.NotContains(t, afUsageReferenceCallbackless, "af sessions",
		"listing verbs an agent cannot run is the failure this variant exists to fix")
	assert.NotContains(t, afUsageReferenceCallbackless, "af tasks",
		"listing verbs an agent cannot run is the failure this variant exists to fix")
}
