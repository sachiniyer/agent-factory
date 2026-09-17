package app

import (
	"encoding/base64"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffOffersAgentAccounts(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "work"}, {Agent: "codex", Name: "foreign"}, {Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd, "handoff must load all target agents' account choices")
	h.Update(cmd())
	require.Contains(t, h.selectionOverlay.Render(), "personal")
	require.Contains(t, h.selectionOverlay.Render(), "codex: foreign")
	require.Contains(t, h.selectionOverlay.Render(), "project default")
}

// 80x24 before/after evidence uses the same deterministic renderer and clock
// as the design stills. Capture only inside the test container.
func TestHandoffAccountDesignScenes(t *testing.T) {
	configureDesignStillsOutput(t)
	for _, mode := range []string{"dark", "light"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := newDesignDriverSceneHome(t, mode, nil)
			inst := handoffActionInstance(t, "Continue migration", "claude")
			inst.CreatedAt = designStillsNow(t)
			h.store.AddInstance(inst)
			h.sidebar.SelectInstance(inst)
			h.termWidth, h.termHeight = 80, 24
			h.relayout()
			restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
				return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "personal"}, {Agent: "codex", Name: "work", LoggedIn: true}}, Defaults: map[string]string{"claude": "personal", "codex": "work"}}, nil
			})
			defer restore()
			_, cmd := h.handleHandoff()
			require.NotNil(t, cmd)
			for _, phase := range []string{"before", "after"} {
				if phase == "after" {
					h.Update(cmd())
				}
				frame := h.View()
				svg := recoverySVG(frame, mode, 80, 24)
				name := "handoff-account-" + phase + "-" + mode + ".svg"
				if out := os.Getenv("AF_TUI_DESIGN_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, name), []byte(svg), 0644))
					continue
				}
				golden, err := os.ReadFile(filepath.Join("testdata", "design", name))
				if err != nil || string(golden) != svg {
					t.Logf("CAPTURE %s %s", name, base64.StdEncoding.EncodeToString([]byte(svg)))
				}
				require.NoError(t, err)
				require.Equal(t, string(golden), svg)
			}
		})
	}
}

func TestHandoffPinnedAccountsAndCredentialWarning(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(agent, _ string) (daemon.ListAccountsResponse, error) {
		require.Empty(t, agent, "pinned handoff needs the target agents' registry too")
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "claude", Name: "personal", LoggedIn: false},
				{Agent: "codex", Name: "spare", LoggedIn: true},
			}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	require.Empty(t, h.handoffChoices, "pinned sessions cannot submit ambient rows while loading")
	h.Update(cmd())
	// #4428: targets that cannot carry a scope stay offered — the swap drops it
	// — while a scopable target with no registered account stays hidden.
	require.Equal(t, []string{"claude", "codex", "aider", "amp", "opencode", "devin"}, h.handoffChoices)
	require.Equal(t, []string{"personal", "spare", "", "", "", ""}, h.handoffAccounts)
	require.Contains(t, h.selectionOverlay.Render(), "not logged in")
	require.Contains(t, h.selectionOverlay.Render(), "aider (ambient)")
	require.NotContains(t, h.selectionOverlay.Render(), "gemini")
	require.Equal(t, 1, h.selectionOverlay.GetSelectedIndex(), "unauthenticated default must not be preselected")
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	require.Contains(t, h.confirmationOverlay.Render(), "no claude credential yet")
}

// A scoped session picking a target with no account support must SEE that the
// scope is what is being handed over — the ambient row's warning names the drop
// before the picker submits (#4428).
func TestHandoffScopedOffersAmbientDropRows(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents:  []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{{Agent: "claude", Name: "work", LoggedIn: true}},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"aider", "amp", "opencode", "devin"}, h.handoffChoices,
		"only targets that cannot carry the scope remain — no account exists to name for the rest")
	require.Equal(t, []string{"", "", "", ""}, h.handoffAccounts)
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "aider cannot carry an account")
	require.Contains(t, rendered, `"work" scope is dropped`)
}

// A scoped session must classify targets by the command they launch, not the
// requested enum (#4430 review): resolved_agents is the daemon's report of the
// resolved command's agent over this repo's program_overrides.
//   - codex → aider: the target gets the ambient drop row its resolved launch
//     warrants, the warning names the resolution, and codex's account rows are
//     NOT offered — a resolved aider has no use for them.
//   - aider → codex: the daemon registers --account against the RESOLVED
//     namespace, so the target is honestly served by codex's registry — the
//     picker offers "aider: spare" rather than hiding a target the daemon
//     accepts (#4430 review round 3).
func TestHandoffScopedClassifiesTargetsByResolvedAgent(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "codex", Name: "spare", LoggedIn: true},
			},
			ResolvedAgents: map[string]string{"codex": "aider", "aider": "codex"},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"codex", "aider", "amp", "opencode", "devin"}, h.handoffChoices,
		"aider redirected to codex is reachable through codex's registry, not hidden")
	require.Equal(t, []string{"", "spare", "", "", ""}, h.handoffAccounts,
		"the aider row names a codex account — the namespace --account resolves in")
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "codex launches aider")
	require.Contains(t, rendered, `"work" scope is dropped`)

	// The redirected aider row explains its namespace up front: "spare" is a
	// codex account, which is the registry the daemon's --account transaction
	// will consult for this target. The picker tears down on Enter, so the
	// second row's confirmation takes a fresh load cycle.
	_, cmd = h.handleHandoff()
	h.Update(cmd())
	h.selectionOverlay.SetSelectedIndex(1)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	rendered = flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "aider launches codex")
	require.Contains(t, rendered, `"spare" is a codex account`)
}

// The account picker applies the same fallback (#4430 review): a scoped claude
// session behind an opaque claude override is still claude, so its own enum is
// neither offered as a self-handoff nor as an ambient "scope dropped" row —
// the daemon refuses both.
func TestHandoffScopedOpaqueCurrentAgentIsNotOffered(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents:         []string{"claude", "codex", "gemini"},
			Entries:        []daemon.AccountEntry{{Agent: "claude", Name: "personal", LoggedIn: true}},
			ResolvedAgents: map[string]string{"claude": "", "aider": ""},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"aider", "amp", "opencode", "devin"}, h.handoffChoices,
		"only targets that drop the scope remain; claude is the running agent")
	require.Equal(t, []string{"", "", "", ""}, h.handoffAccounts)
}

// A scoped session's ambient scope-drop rows must not steal the preselection
// from a scopable target's logged-in account row just because the drop row
// comes first in SupportedPrograms order (#4430 review): with claude
// redirected to a non-agent command, its ambient row precedes codex's account
// section, and the codex account must still win the default.
func TestHandoffScopedPreselectsAccountOverAmbientDrop(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "gemini")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents:         []string{"claude", "codex", "gemini"},
			Entries:        []daemon.AccountEntry{{Agent: "codex", Name: "spare", LoggedIn: true}},
			ResolvedAgents: map[string]string{"claude": ""},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"claude", "codex", "aider", "amp", "opencode", "devin"}, h.handoffChoices)
	require.Equal(t, []string{"", "spare", "", "", "", ""}, h.handoffAccounts)
	require.Equal(t, 1, h.selectionOverlay.GetSelectedIndex(),
		"the logged-in codex account is the default; the leading claude ambient row is only a fallback")
}

func TestHandoffCredentialWarning(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Contains(t, h.selectionOverlay.Render(), "not logged in")
	require.NotEqual(t, 0, h.selectionOverlay.GetSelectedIndex())
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	require.Contains(t, h.confirmationOverlay.Render(), "no claude credential yet")
}
