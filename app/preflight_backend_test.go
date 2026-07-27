package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoDeclaringBackend makes a git repo whose in-repo config selects backend,
// the way #2194 documents opting a repo into container sessions.
func repoDeclaringBackend(t *testing.T, backend string) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))
	out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	if backend == "" {
		return repoRoot
	}
	cfgDir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, config.TomlConfigFileName),
		[]byte("backend = \""+backend+"\"\n"), 0o644))
	return repoRoot
}

// TestNamingPreflightFollowsTheRepoDeclaredBackend closes the half of #2592 the
// TUI's own gate was missing. #2600 skipped the local agent preflight for a
// backend PICKED in the naming form, which leaves the other way a session
// becomes non-local — the repo's `backend` config key, with the field never
// opened — still gated on a local `claude` that will never run the session.
//
// Both surfaces now ask session.LocalPrereqsRequired, so the precedence between
// the two is decided once rather than per-surface.
func TestNamingPreflightFollowsTheRepoDeclaredBackend(t *testing.T) {
	for _, tc := range []struct {
		name        string
		repoBackend string
		wantRefused bool
	}{
		{name: "local repo still gates", wantRefused: true},
		{name: "docker repo", repoBackend: "docker"},
		{name: "ssh repo", repoBackend: "ssh"},
		{name: "hook repo", repoBackend: "hook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(200, 1)
			got := recordStartRequest(t)
			t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error {
				return errors.New("claude is not installed on this machine")
			}))
			h.repoRoot = repoDeclaringBackend(t, tc.repoBackend)
			startNaming(t, h, "repo-declared-backend")

			// A refusal ends in a transient notice whose cmd is a timer, so the
			// refused leg drains one hop (pressExpectingNotice) rather than
			// following the re-emitted key's cmd to completion.
			if tc.wantRefused {
				pressExpectingNotice(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			} else {
				pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			}

			if tc.wantRefused {
				require.Equal(t, stateNew, h.state,
					"a local create must still be refused when the agent binary is missing")
				assert.Contains(t, h.errBox.FullError(), "not installed")
				return
			}
			require.Equal(t, stateDefault, h.state,
				"a repo that declares a sandbox backend must not be refused for a missing local agent")
			assert.Empty(t, h.errBox.FullError(),
				"nothing about the local agent applies to a session that runs off-box")
			assert.Empty(t, got.Backend,
				"an untouched backend field still sends nothing — the repo key decides, as it always did")
		})
	}
}

// TestNamingPreflightDoesNotRefuseABackendItCannotResolve keeps the gate from
// becoming a second, stale copy of the backend catalog. The picker offers
// whatever the daemon lists (#2600), so a name this process's enum has never
// heard of is normal — it means the daemon knows more, not that the create is
// wrong. Unresolvable is not local, and it is certainly not "the local agent is
// missing": the gate stands aside and lets the daemon answer.
func TestNamingPreflightDoesNotRefuseABackendItCannotResolve(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error {
		return errors.New("claude is not installed on this machine")
	}))
	h.repoRoot = repoDeclaringBackend(t, "")
	startNaming(t, h, "future-backend")
	h.pendingBackend = "moonbase"

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateDefault, h.state,
		"a backend only the daemon knows about must still submit")
	assert.Equal(t, "moonbase", got.Backend,
		"and reach the daemon verbatim")
}
