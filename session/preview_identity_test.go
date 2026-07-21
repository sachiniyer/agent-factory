package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// previewIdentityInstance returns each captured pane's exact tmux target as its
// content. That makes an ordinal misroute observable: b and c cannot both answer
// the fixture-wide "content" string used by the general tab lifecycle helper.
func previewIdentityInstance(t *testing.T, agentName string) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cmdExec, _ := raceHookExec(map[string]bool{agentName: true}, nil)
	cmdExec.OutputFunc = func(cmd *exec.Cmd) ([]byte, error) {
		for i, arg := range cmd.Args {
			if arg == "-t" && i+1 < len(cmd.Args) {
				target := strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
				return []byte(target), nil
			}
		}
		return nil, nil
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	repoPath := "/tmp/preview-tab-identity-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTmux := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, cmdExec)
	return &Instance{
		Title:       agentName,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTmux)},
	}
}

func tabTmuxName(t *testing.T, inst *Instance, id string) string {
	t.Helper()
	for _, tab := range inst.ToInstanceData().Tabs {
		if tab.ID == id {
			return tab.TmuxName
		}
	}
	t.Fatalf("tab id %q is absent from instance data", id)
	return ""
}

// TestPreview_TabIDSurvivesConcurrentOrdinalShift is the #2200 fail-first
// scenario. The caller resolved b from [agent,a,b,c], then a concurrent close
// shifted the live roster to [agent,b,c]. A capture addressed by b's stable id
// may return b or an honest error if b itself vanished; it must never return c.
func TestPreview_TabIDSurvivesConcurrentOrdinalShift(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, tc := range []struct {
		name string
		full bool
	}{
		{name: "visible", full: false},
		{name: "full history", full: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := previewIdentityInstance(t, "af_preview_identity_"+strings.ReplaceAll(tc.name, " ", "_"))
			_, err := inst.AddProcessTab("a", "a")
			require.NoError(t, err)
			b, err := inst.AddProcessTab("b", "b")
			require.NoError(t, err)
			c, err := inst.AddProcessTab("c", "c")
			require.NoError(t, err)

			snapshot := inst.GetTabs()
			resolved, err := ResolveTabIndex(snapshot, b.ID, "", 0)
			require.NoError(t, err)
			require.Equal(t, 2, resolved)

			closed := make(chan error, 1)
			go func() { closed <- inst.CloseTab(1) }()
			require.NoError(t, <-closed)
			requireIndex(t, inst, b.ID, 1)
			requireIndex(t, inst, c.ID, 2)

			content, captureErr := inst.AgentServer().PreviewByID(b.ID, tc.full)
			if captureErr == nil {
				require.Equal(t, tabTmuxName(t, inst, b.ID), content,
					"a preview resolved by b's stable id must never capture c after a shifts the ordinals")
			}
		})
	}
}

func TestPreview_InvalidOrdinalAndStaleIDReturnErrors(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := previewIdentityInstance(t, "af_preview_refusals")
	a, err := inst.AddProcessTab("a", "a")
	require.NoError(t, err)

	for _, full := range []bool{false, true} {
		for _, idx := range []int{-1, 2, 99} {
			_, err := inst.AgentServer().Preview(idx, full)
			require.ErrorIs(t, err, ErrTabIndexOutOfRange,
				"an out-of-range capture must not fabricate a blank pane")
		}
	}

	require.NoError(t, inst.CloseTabByID(a.ID))
	_, err = inst.AgentServer().PreviewByID(a.ID, false)
	require.ErrorIs(t, err, ErrTabGone, "a closed stable id must be refused")
}

// TestPreviewByIDAsOrdinalHoldsRosterAcrossCapture covers the remote-runtime
// compatibility bridge. Its private wire is ordinal-shaped, so the daemon must
// retain the roster read lock from ID resolution through the bounded capture.
func TestPreviewByIDAsOrdinalHoldsRosterAcrossCapture(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := previewIdentityInstance(t, "af_preview_remote_bridge")
	_, err := inst.AddProcessTab("a", "a")
	require.NoError(t, err)
	b, err := inst.AddProcessTab("b", "b")
	require.NoError(t, err)

	closeDone := make(chan error, 1)
	content, err := inst.previewByIDAsOrdinal(b.ID, func(idx int) (string, error) {
		require.Equal(t, 2, idx)
		go func() { closeDone <- inst.CloseTab(1) }()
		select {
		case err := <-closeDone:
			t.Fatalf("tab close completed during the capture critical section: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		return "remote-b", nil
	})
	require.NoError(t, err)
	require.Equal(t, "remote-b", content)
	require.NoError(t, <-closeDone)
	requireIndex(t, inst, b.ID, 1)
}
