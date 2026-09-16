package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #4400 review-round-5 carry helpers: ambientSafeCarriedTabs filters the
// roster a rejected pin must not restore, and reconcilePendingSwapConversation
// keeps the committed swap's recorded conversation equal to the one the create
// actually launches.

// TestAmbientSafeCarriedTabsKeepsAgentPlaceholder pins the two defects Codex
// found in the filter: it must not compact the parked roster's shared backing
// array in place (the next retry reads it), and it must keep index 0 as the
// placeholder restoreCarriedTabs and countNonAgentTabs skip unconditionally —
// dropping it silently consumes the first surviving web/editor tab.
func TestAmbientSafeCarriedTabsKeepsAgentPlaceholder(t *testing.T) {
	parked := []session.TabData{
		{ID: "tab-agent", Kind: session.TabKindAgent, TmuxName: "af_x"},
		{ID: "tab-shell", Kind: session.TabKindShell, TmuxName: "af_x_shell"},
		{ID: "tab-web", Kind: session.TabKindWeb, URL: "http://localhost:9/"},
		{ID: "tab-logs", Kind: session.TabKindProcess, Command: "tail -f x", TmuxName: "af_x_logs"},
		{ID: "tab-vs", Kind: session.TabKindVSCode},
	}
	snapshot := append([]session.TabData(nil), parked...)

	kept := ambientSafeCarriedTabs(parked)

	var ids []string
	for _, td := range kept {
		ids = append(ids, td.ID)
	}
	require.Equal(t, []string{"tab-agent", "tab-web", "tab-vs"}, ids,
		"index 0 stays the placeholder; every tmux-backed row after it is dropped")
	require.Equal(t, snapshot, parked,
		"filtering must not mutate the roster the parked carry still owns")
	require.Empty(t, ambientSafeCarriedTabs(nil))
}

// TestReconcilePendingSwapConversationFollowsLaunchedConversation pins the
// create-side half of the finding: the attached swap's ConversationID tracks
// the conversation this attempt launches — substituted, fresh, or
// non-resumable — and the parked carry's shared pointer is never written
// through.
func TestReconcilePendingSwapConversationFollowsLaunchedConversation(t *testing.T) {
	parked := &session.AccountSwapData{To: "work", ConversationID: "old-conv"}
	req := CreateSessionRequest{pendingAccountSwap: parked}

	// A substituted conversation becomes the swap's recorded id.
	req.resumeConversation = session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "new-conv"}
	reconcilePendingSwapConversation(&req)
	require.Equal(t, "new-conv", req.pendingAccountSwap.ConversationID)
	require.Equal(t, "old-conv", parked.ConversationID,
		"the parked carry's pointer is shared — reconcile must clone, not mutate")

	// A fresh start clears it: the pane has no resumable id to stamp.
	req.resumeConversation = session.AgentConversationData{}
	reconcilePendingSwapConversation(&req)
	require.Empty(t, req.pendingAccountSwap.ConversationID)

	// A swap that never recorded a conversation stays untouched.
	plain := &session.AccountSwapData{To: "work"}
	req.pendingAccountSwap = plain
	req.resumeConversation = session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "x"}
	reconcilePendingSwapConversation(&req)
	require.Same(t, plain, req.pendingAccountSwap)
	require.Empty(t, plain.ConversationID)

	// No swap at all is a no-op.
	req.pendingAccountSwap = nil
	reconcilePendingSwapConversation(&req)
	require.Nil(t, req.pendingAccountSwap)
}

// TestLoadReapedRootCarryRefusesSymlink pins the read half of the managed-file
// contract (#4400 review): the write and remove ends already refuse links, and
// a read that silently followed one would treat foreign content as the carry af
// parked. Both a resolving link and a dangling one must refuse — the refusal is
// about the link's presence, not about what it points at.
func TestLoadReapedRootCarryRefusesSymlink(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m := &Manager{}
	const repoID = "deadbeefcafe01"

	path, err := reapedRootCarryPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	payload, err := json.Marshal(reapedRootCarryDisk{Workspace: "/repos/ws", Account: "work"})
	require.NoError(t, err)
	foreign := filepath.Join(t.TempDir(), "foreign.json")
	require.NoError(t, os.WriteFile(foreign, payload, 0o600))

	require.NoError(t, os.Symlink(foreign, path))
	_, present, err := m.loadReapedRootCarry(repoID)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrManagedFileSymlink)
	require.False(t, present, "a refused read reports no carry — it must not consume foreign content")

	// A dangling link refuses the same way: nothing it points at exists, and
	// reading through it must still not stand in for af's own file.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone.json"), path))
	_, present, err = m.loadReapedRootCarry(repoID)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrManagedFileSymlink)
	require.False(t, present)

	// A real file at the path still loads normally.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, payload, 0o600))
	state, present, err := m.loadReapedRootCarry(repoID)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "/repos/ws", state.workspace)
	require.Equal(t, "work", state.account)
}

// TestReapedRootStateForWorkspace pins the consumption gate (#4400 review): the
// carry is keyed by repository ID so every spelling of the repo finds it, but
// its contents belong to the checkout that parked it. A create under a
// different workspace must leave it parked; a carry with no workspace — one
// written before the field existed — remains consumable anywhere.
func TestReapedRootStateForWorkspace(t *testing.T) {
	bound := reapedRootState{workspace: "/repos/ws"}
	require.True(t, bound.forWorkspace("/repos/ws"))
	require.True(t, bound.forWorkspace("/repos/ws/"),
		"path spelling differences — trailing separators, dot segments — clean equal")
	require.True(t, bound.forWorkspace("/repos/./ws"))
	require.False(t, bound.forWorkspace("/repos/other"),
		"a linked worktree shares the repo ID but not the checkout the carry belongs to")
	require.False(t, bound.forWorkspace(""))

	unbound := reapedRootState{}
	require.True(t, unbound.forWorkspace("/repos/anything"),
		"a pre-binding carry consumes exactly as it did before the field existed")
	require.True(t, unbound.forWorkspace(""))

	// The binding survives the disk round-trip: what the reap captured is what
	// a post-restart load enforces.
	state := reapedRootState{workspace: "/repos/ws", account: "work"}
	require.Equal(t, state.workspace, state.carryDisk().state().workspace)
}

// TestRepoHasEnabledRootCandidate pins the sibling check a disabled pass runs
// before retiring the parked carry (#4400 review): the carry's only consumer is
// the next enabled spelling of the repo, so the pass may retire it only when no
// enabled spelling remains — never while an enabled sibling sits in backoff,
// and never on UNKNOWABLE evidence, where every candidate resolves disabled
// fail-closed and the carry must outlive the transient.
func TestRepoHasEnabledRootCandidate(t *testing.T) {
	const repoID = "deadbeefcafe02"
	enabledLayer := &config.RootAgentLayer{Value: config.RootAgent{Enabled: true}, EnabledSet: true}
	disabledLayer := &config.RootAgentLayer{Value: config.RootAgent{Enabled: false}, EnabledSet: true}

	newManager := func(rootAgents map[string]config.RootAgentConfig, snap rootAgentSnapshot) *Manager {
		m := &Manager{cfg: &config.Config{RootAgents: rootAgents}}
		m.rootAgentLayers.Store(&snap)
		return m
	}

	// A second legacy path proven to this repo is a live sibling: the disabled
	// pass may not retire the carry while it remains.
	m := newManager(
		map[string]config.RootAgentConfig{"/a": {}, "/b": {}},
		rootAgentSnapshot{legacy: legacyRepoDedup{
			byPath: map[string]string{"/a": repoID, "/b": repoID},
		}},
	)
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/a"),
		"an enabled sibling spelling keeps the carry parked for it")
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/b"),
		"symmetric: with /b excluded, /a is still enabled — the answer stays true")
	// Both present but the excluded key is /a: /b remains → true; and excluding
	// both is not a shape the sweep produces — the caller names one key.
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/a"))

	// A path proven to ANOTHER repo does not count; with it the only sibling,
	// nothing enabled remains.
	m = newManager(
		map[string]config.RootAgentConfig{"/a": {}, "/other": {}},
		rootAgentSnapshot{legacy: legacyRepoDedup{
			byPath: map[string]string{"/a": repoID, "/other": "0123456789ab"},
		}},
	)
	require.False(t, m.repoHasEnabledRootCandidate(repoID, "/a"))

	// A personal enabled=false vetoes every spelling for this repo: no enabled
	// consumer remains even with a second legacy path present.
	m = newManager(
		map[string]config.RootAgentConfig{"/a": {}, "/b": {}},
		rootAgentSnapshot{
			personal: map[string]*config.RootAgentLayer{repoID: disabledLayer},
			legacy:   legacyRepoDedup{byPath: map[string]string{"/a": repoID, "/b": repoID}},
		},
	)
	require.False(t, m.repoHasEnabledRootCandidate(repoID, "/a"),
		"a repo-wide disable retires the carry — no spelling can run")

	// A path whose probe never answered may still resolve to this repo: it
	// counts as a candidate, because retiring on unproven evidence is the
	// defect the check exists to prevent.
	m = newManager(
		map[string]config.RootAgentConfig{"/a": {}, "/b": {}},
		rootAgentSnapshot{
			personal: map[string]*config.RootAgentLayer{repoID: enabledLayer},
			legacy: legacyRepoDedup{
				byPath:       map[string]string{"/a": repoID},
				unknownPaths: map[string]bool{"/b": true},
			},
		},
	)
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/a"),
		"an unanswered sibling probe may still ensure this repo")

	// A project binding survives the same check: when the legacy map covers the
	// repo, the binding is ensured through the legacy path — it does not count
	// as a second candidate.
	m = newManager(
		map[string]config.RootAgentConfig{"/a": {}},
		rootAgentSnapshot{
			personal:     map[string]*config.RootAgentLayer{repoID: enabledLayer},
			projectRoots: map[string]resolvedProjectRoot{repoID: {root: "/proj"}},
			legacy: legacyRepoDedup{
				ids:    map[string]bool{repoID: true},
				byPath: map[string]string{"/a": repoID},
			},
		},
	)
	require.False(t, m.repoHasEnabledRootCandidate(repoID, "/a"),
		"a legacy-covered binding is ensured through the legacy path, not twice")

	m = newManager(
		map[string]config.RootAgentConfig{},
		rootAgentSnapshot{
			personal:     map[string]*config.RootAgentLayer{repoID: enabledLayer},
			projectRoots: map[string]resolvedProjectRoot{repoID: {root: "/proj"}},
			legacy:       legacyRepoDedup{},
		},
	)
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/other-key"),
		"an uncovered enabled project binding is the sibling that keeps the carry")
	require.False(t, m.repoHasEnabledRootCandidate(repoID, "/proj"),
		"the disabled binding itself is excluded — no other candidate remains")

	// An UNKNOWABLE decision — an unreadable personal layer — must keep the
	// carry parked: it is a transient the durable file exists to survive, not
	// proof no consumer remains.
	m = newManager(
		map[string]config.RootAgentConfig{"/a": {}},
		rootAgentSnapshot{
			personalUnreadable: map[string]string{repoID: "proj-1"},
			legacy:             legacyRepoDedup{byPath: map[string]string{"/a": repoID}},
		},
	)
	require.True(t, m.repoHasEnabledRootCandidate(repoID, "/a"),
		"an unreadable personal layer holds the carry, never retires it")
}
