package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skip list is the user-visible half of the daemon-side warning
// (sandbox_agent_config.go): every file the placement refused is named, with
// the reason, so the session row can say "af agent config skipped: <file>:
// <why>" on every surface instead of only in a daemon log the user never
// reads (#6337).

// A missing declared file still does not fail the create — but it must be
// REPORTED, and the report must name the entry the operator wrote.
func TestAgentConfigSkippedNamesAMissingFile(t *testing.T) {
	operatorHome(t)
	require.NoError(t, os.Remove(filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")))
	w := configWorkspace(t, "claude", "box1", "claude:CLAUDE.md", "claude:settings.json")

	skipped, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	require.Len(t, skipped, 1)
	assert.Contains(t, skipped[0], "claude:CLAUDE.md", "the entry names the declared agent:file")
	assert.Contains(t, skipped[0], "no such file", "and why it was not placed")
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "CLAUDE.md"))
	assert.FileExists(t, filepath.Join(w.AccountDir(), "settings.json"), "the placeable file still landed")
}

// Every skip shape lands on the row, not only the missing file: an unreadable
// entry, a non-regular entry, and one that would overflow the archive bound.
func TestAgentConfigSkippedNamesEveryRefusal(t *testing.T) {
	operatorHome(t)
	home := os.Getenv("HOME")
	// A directory where a regular file is required.
	require.NoError(t, os.Remove(filepath.Join(home, ".claude", "CLAUDE.md")))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "CLAUDE.md"), 0o700))
	w := configWorkspace(t, "claude", "box1",
		"claude:CLAUDE.md",      // not a regular file
		"claude:.claude.json",   // placed
		"claude:settings.json",  // placed
		"claude:does-not-exist", // never declared by the closed set — but if it were, the entry is named
	)
	// The closed set is normalized upstream; the placement loop still guards
	// each name it is handed, so feed it a name that does not exist.
	w.spec.AgentConfigFiles = []string{"claude:CLAUDE.md", "claude:.claude.json", "claude:settings.json", "claude:CLAUDE.local.md"}

	skipped, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	require.Len(t, skipped, 2, "the non-regular entry and the missing entry are both reported")
	assert.Contains(t, skipped[0], "claude:CLAUDE.md")
	assert.Contains(t, skipped[0], "not a regular file")
	assert.Contains(t, skipped[1], "claude:CLAUDE.local.md")
}

// A file over the bound is reported with the size fact, like the archive-bound
// skip beside it.
func TestAgentConfigSkippedNamesAnOversizedFile(t *testing.T) {
	operatorHome(t)
	big := make([]byte, agentConfigMaxFileBytes+1)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), big, 0o600))
	w := configWorkspace(t, "claude", "box1", "claude:settings.json")

	skipped, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	require.Len(t, skipped, 1)
	assert.Contains(t, skipped[0], "claude:settings.json")
	assert.Contains(t, skipped[0], "bound")
}

// When every declared file places, there is nothing to warn about — an empty
// list, not a nil-shaped ambiguity the surfaces would render as a warning.
func TestAgentConfigSkippedIsEmptyWhenEveryFilePlaces(t *testing.T) {
	operatorHome(t)
	w := configWorkspace(t, "claude", "box1", "claude:settings.json", "claude:.claude.json")
	skipped, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.Empty(t, skipped)
}

// A session that cannot carry agent config at all — unlisted host, no account
// scope — produces no list, so a file that would have been missing does not
// warn on a session it never applied to.
func TestAgentConfigSkippedIsEmptyWhenNothingApplies(t *testing.T) {
	operatorHome(t)
	unlisted := configWorkspace(t, "claude", "", "claude:settings.json")
	skipped, err := unlisted.syncAgentConfigIn(time.Second)
	require.NoError(t, err)
	assert.Empty(t, skipped)
}

// The skip list rides the session record like callback_unavailable does —
// persisted through instances.json and restored through FromInstanceData — so a
// daemon restart cannot silently drop the fact that the file never landed.
func TestAgentConfigSkippedRoundTripsThroughTheSessionRow(t *testing.T) {
	want := []string{
		"claude:CLAUDE.md: stat /home/op/.claude/CLAUDE.md: no such file or directory",
		"claude:.claude.json: /home/op/.claude.json is 999999999 bytes, over the 67108864-byte bound",
	}
	inst := &Instance{Title: "x"}
	require.NoError(t, inst.bindProvisionResult(ProvisionResult{
		Backend:            newInertSandboxBackend("ssh"),
		AgentConfigSkipped: want,
	}))

	payload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	var data InstanceData
	require.NoError(t, json.Unmarshal(payload, &data))
	restored, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)

	assert.Equal(t, want, restored.AgentConfigSkipped())
	assert.Equal(t, want, restored.ToInstanceData().AgentConfigSkipped, "the projection keeps the list for snapshots and the HTTP API")
}

// The same field the JSON carries is the one the storage tag names: exactly
// `agent_config_skipped`, additive and omitted when empty.
func TestAgentConfigSkippedSerializesUnderItsContractName(t *testing.T) {
	inst := &Instance{Title: "x"}
	payload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "agent_config_skipped", "empty list is omitted")

	require.NoError(t, inst.bindProvisionResult(ProvisionResult{
		Backend:            newInertSandboxBackend("ssh"),
		AgentConfigSkipped: []string{"claude:CLAUDE.md: missing"},
	}))
	payload, err = json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"agent_config_skipped":`)
}

// A fresh provision that placed everything clears a skip list the previous
// runtime carried — bindProvisionResult installs the result's evidence, never
// ORs it.
func TestBindProvisionResultClearsAgentConfigSkippedOnACleanPlacement(t *testing.T) {
	inst := &Instance{Title: "x", agentConfigSkipped: []string{"claude:CLAUDE.md: missing"}}
	require.NoError(t, inst.bindProvisionResult(ProvisionResult{Backend: newInertSandboxBackend("ssh")}))
	assert.Empty(t, inst.AgentConfigSkipped())

	// And the reverse: a fresh skip list replaces the old one wholesale.
	require.NoError(t, inst.bindProvisionResult(ProvisionResult{
		Backend:            newInertSandboxBackend("ssh"),
		AgentConfigSkipped: []string{"claude:.claude.json: over the bound"},
	}))
	assert.Equal(t, []string{"claude:.claude.json: over the bound"}, inst.AgentConfigSkipped())
}

// The reconcile is the client-side mirror app/sync.go runs: it must clone the
// transport slice in and out, so a caller mutating the payload it sent — or a
// renderer mutating what it read — cannot rewrite the projection underneath
// the row.
func TestReconcileAgentConfigSkippedIsCopySafe(t *testing.T) {
	inst := &Instance{Title: "x"}
	in := []string{"claude:CLAUDE.md: a", "claude:settings.json: b"}
	require.True(t, inst.ReconcileAgentConfigSkipped(in))
	in[0] = "mutated input"
	assert.Equal(t, []string{"claude:CLAUDE.md: a", "claude:settings.json: b"}, inst.AgentConfigSkipped(),
		"the stored list never aliases the transport slice")

	out := inst.AgentConfigSkipped()
	out[1] = "mutated read"
	assert.Equal(t, []string{"claude:CLAUDE.md: a", "claude:settings.json: b"}, inst.AgentConfigSkipped(),
		"the accessor hands out a copy, not the row's own slice")

	require.False(t, inst.ReconcileAgentConfigSkipped([]string{"claude:CLAUDE.md: a", "claude:settings.json: b"}),
		"the same list is no change")
	require.True(t, inst.ReconcileAgentConfigSkipped(nil), "an empty list clears it")
	assert.Empty(t, inst.AgentConfigSkipped())
}

// Reattach adopts the same runtime — it provisions nothing, so the recorded
// skip list travels with it exactly like the recorded callback reason does
// (#5974): installing an empty one would read as a fresh clean provision.
func TestReattachPreservesAgentConfigSkipped(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	want := []string{"claude:CLAUDE.md: stat /home/op/.claude/CLAUDE.md: no such file or directory"}
	inst.agentConfigSkipped = want
	require.NoError(t, inst.ReattachSandbox())
	assert.Equal(t, want, inst.AgentConfigSkipped())
}

// An off-box swap stages the incoming scope through the same placement: the
// result reports what did not land, and the row carries it once the swap
// commits.
func TestSwapOffBoxAccountReportsSkippedConfig(t *testing.T) {
	f := newSwapFixture(t)
	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{"model":"opus"}`), 0o600))
	// CLAUDE.md deliberately absent from the operator home.
	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	f.w.spec.AgentConfigFiles = []string{"claude:settings.json", "claude:CLAUDE.md"}
	f.inst.agentSrv = hostEnvServer{f.srv, t, hostScope}

	res, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	require.Len(t, res.AgentConfigSkipped, 1)
	assert.Contains(t, res.AgentConfigSkipped[0], "claude:CLAUDE.md")
	assert.Equal(t, res.AgentConfigSkipped, f.inst.AgentConfigSkipped(),
		"the row carries the staged scope's skip list")
	assert.Equal(t, `{"model":"opus"}`, readOrEmpty(filepath.Join(f.dir, "account", "settings.json")),
		"the placeable file still landed in the new scope")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the new account is live")
}

// A swap whose staged scope placed everything clears whatever the previous
// scope missed — the row must not warn about a file the incoming scope now
// holds.
func TestSwapOffBoxAccountClearsSkippedConfigWhenEverythingPlaces(t *testing.T) {
	f := newSwapFixture(t)
	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{"model":"opus"}`), 0o600))
	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	f.w.spec.AgentConfigFiles = []string{"claude:settings.json"}
	f.inst.agentSrv = hostEnvServer{f.srv, t, hostScope}
	f.inst.agentConfigSkipped = []string{"claude:settings.json: earlier placement skipped it"}

	res, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.Empty(t, res.AgentConfigSkipped)
	assert.Empty(t, f.inst.AgentConfigSkipped(), "a clean staged scope clears the row's skip list")
}
