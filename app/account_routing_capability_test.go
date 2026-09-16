package app

import (
	"errors"
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// #4404 review: account_auto rides a create only to a daemon the form SAW
// report pool routing. The TUI reaches the daemon over HTTP, and the picker
// already labels the untouched row with the legacy contract when the answer
// lacks pool_routing — so an opt-in sent anyway is a request the label never
// described, and one that never answered is not evidence of a router at all.
func TestNamingFormOptsInOnlyAfterObservingPoolRouting(t *testing.T) {
	preRouter := twoAgentsWithAccounts()
	preRouter.PoolRouting = false
	for _, tc := range []struct {
		name     string
		deliver  bool
		resp     daemon.ListAccountsResponse
		err      error
		wantAuto bool
	}{
		{name: "routing daemon answered", deliver: true, resp: twoAgentsWithAccounts(), wantAuto: true},
		{name: "no answer landed yet", deliver: false},
		{name: "pre-router daemon answered", deliver: true, resp: preRouter},
		{name: "the answer failed", deliver: true, err: errors.New("daemon unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			got := recordStartRequest(t)
			inst := startNaming(t, h, "untouched")
			if tc.deliver {
				h.handleAccountDefault(accountDefaultMsg{naming: inst, agent: "claude", resp: tc.resp, err: tc.err})
			}

			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			assert.Empty(t, got.Account)
			assert.False(t, got.AccountAmbient)
			assert.Equal(t, tc.wantAuto, got.AccountAuto)
		})
	}
}

// The canary for the gate above: a user who deliberately picks the routable
// row — before or after the default answer lands — still opts in once the
// capability is known, and a pick of a named account never does.
func TestNamingFormRoutableRowStillOptsIn(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	inst := startNaming(t, h, "routable-pick")

	openAccountField(t, h)
	require.Equal(t, "Automatic — af picks a healthy account", h.accountPickerChoices[0].label)
	pickAccount(t, h, h.accountPickerChoices[0].label)
	// The default answer lands after the pick; it must not un-learn the
	// capability the picker's own answer already taught.
	h.handleAccountDefault(accountDefaultMsg{naming: inst, agent: "claude", resp: twoAgentsWithAccounts()})

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Account)
	assert.True(t, got.AccountAuto, "the routable row is this client's opt-in")
}

// #4404 review: the router leaves ssh/sandbox/hook creates on the legacy
// contract however many accounts are logged in, so the picker must not call
// the untouched row "Automatic" there.
func TestAccountPickerLabelFollowsTheBackend(t *testing.T) {
	resp := twoAgentsWithAccounts()
	routed := accountChoicesFrom(resp, "claude", true)
	require.Equal(t, "Automatic — af picks a healthy account", routed[0].label)
	require.True(t, slices.ContainsFunc(routed, func(c accountChoice) bool { return c.pinsAmbient }))

	offBox := accountChoicesFrom(resp, "claude", false)
	require.Equal(t, "Use the agent's own login (this backend runs no account)", offBox[0].label)
	require.True(t, slices.ContainsFunc(offBox, func(c accountChoice) bool { return c.pinsAmbient }),
		"the pin stays on offer: a pick made before the backend moved must not vanish with it")

	withDefault := resp
	withDefault.Defaults = map[string]string{"claude": "work"}
	require.Equal(t, "Use configured default (work)", accountChoicesFrom(withDefault, "claude", false)[0].label,
		"a configured default is still what the daemon applies — and refuses by name — on that backend")

	for _, backend := range []string{"ssh", "hook", "sandbox"} {
		t.Run(backend, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			stubAccounts(t, twoAgentsWithAccounts(), nil)
			startNaming(t, h, "off-box")
			h.pendingBackend = backend
			openAccountField(t, h)
			require.NotEmpty(t, h.accountPickerChoices)
			assert.NotContains(t, h.accountPickerChoices[0].label, "Automatic")
		})
	}
	t.Run("remote ssh-default repo", func(t *testing.T) {
		h := newTestHome(t)
		h.errBox.SetSize(200, 1)
		sshDefault := twoAgentsWithAccounts()
		sshDefault.RepoBackendAccountScoped = false
		stubAccounts(t, sshDefault, nil)
		startNaming(t, h, "remote-default")
		openAccountField(t, h)
		require.NotEmpty(t, h.accountPickerChoices)
		assert.Equal(t, "Use the agent's own login (this backend runs no account)", h.accountPickerChoices[0].label,
			"the daemon's answer for the repo default wins over anything this client could read")
	})
	for _, backend := range []string{"", "local", "docker"} {
		t.Run("routable "+backend, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			stubAccounts(t, twoAgentsWithAccounts(), nil)
			startNaming(t, h, "on-box")
			h.pendingBackend = backend
			openAccountField(t, h)
			require.NotEmpty(t, h.accountPickerChoices)
			assert.Equal(t, "Automatic — af picks a healthy account", h.accountPickerChoices[0].label)
		})
	}
}

// The wire follows the backend the form SUBMITTED, not the repo default the
// cleared form falls back to: a local pick in an ssh-default repo is routed,
// and an untouched field there is not. The repo default is the daemon's
// answer (RepoBackendAccountScoped), because a TUI attached to a remote daemon
// cannot read that repo's config.
func TestNamingFormOptInFollowsTheSubmittedBackend(t *testing.T) {
	sshDefault := twoAgentsWithAccounts()
	sshDefault.RepoBackendAccountScoped = false
	for _, tc := range []struct {
		backend  string
		wantAuto bool
	}{
		{backend: "", wantAuto: false},
		{backend: "local", wantAuto: true},
		{backend: "docker", wantAuto: true},
		{backend: "hook", wantAuto: false},
		{backend: "not-a-backend-this-build-knows", wantAuto: false},
	} {
		t.Run("backend="+tc.backend, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			got := recordStartRequest(t)
			inst := startNaming(t, h, "backend-pick")
			h.handleAccountDefault(accountDefaultMsg{naming: inst, agent: "claude", resp: sshDefault})
			h.pendingBackend = tc.backend

			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			assert.Equal(t, tc.backend, got.Backend)
			assert.Equal(t, tc.wantAuto, got.AccountAuto)
		})
	}
}

// #4404 review: a routed session's account is af's pick, not a pin — the
// daemon releases it in an agent-only handoff — so the handoff picker keeps
// every target agent on its ambient identity instead of demanding a target
// account, and still offers a target agent with no registered account at all.
func TestHandoffAutomaticAccountKeepsAgentOnlyTargets(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.ReconcileAccountHandoffSnapshot("work", true, nil)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	defer SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "work", LoggedIn: true},
			{Agent: "claude", Name: "personal", LoggedIn: true},
		}}, nil
	})()

	_, cmd := h.handleHandoff()
	require.NotEmpty(t, h.handoffChoices, "an automatic account must not blank the agent picker while accounts load")
	require.NotNil(t, cmd)
	h.Update(cmd())

	codex := slices.Index(h.handoffChoices, "codex")
	require.GreaterOrEqual(t, codex, 0, "a target agent with no registered account is still offered")
	assert.Equal(t, "", h.handoffAccounts[codex], "on its ambient identity")
	assert.Contains(t, h.selectionOverlay.Render(), "codex (ambient)")
	assert.Contains(t, h.handoffAccounts, "personal", "a same-agent account switch is still on offer")
	assert.NotContains(t, h.handoffAccounts, "work", "switching to the account it already runs is no switch")
}

// The pinned half is unchanged: a user's own account still demands a target
// account, so this change cannot silently drop a pin.
func TestHandoffPinnedAccountStillRequiresATargetAccount(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.ReconcileAccountHandoffSnapshot("work", false, nil)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	defer SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "personal", LoggedIn: true},
		}}, nil
	})()

	_, cmd := h.handleHandoff()
	require.Empty(t, h.handoffChoices)
	h.Update(cmd())
	assert.NotContains(t, h.handoffChoices, "codex", "a pinned session cannot hand off to an agent with no account")
	assert.NotContains(t, h.selectionOverlay.Render(), "(ambient)")
}
