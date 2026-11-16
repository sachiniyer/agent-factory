package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// These pin #7386: what a session action leaves behind — a kept failed-create
// draft, a report about how it went — belongs to the project the action ran in.
// Like the other switch tests they drive the TUI through its handlers, the cmds
// they return and Update, so they compile against the code before the fix.

// twoCheckouts is asyncSwitchHome with A and B as real checkouts, so the naming
// form's submit preflight can resolve either project, and a fetcher that gives
// each project one session of its own.
func twoCheckouts(t *testing.T) (h *home, repoA, repoB *config.RepoContext) {
	t.Helper()
	h, _, _ = asyncSwitchHome(t)
	h.repoRoot = initTestGitRepo(t)
	repoA = &config.RepoContext{Root: h.repoRoot, ID: h.repoID}
	rootB := initTestGitRepo(t)
	repoB = &config.RepoContext{Root: rootB, ID: config.RepoIDFromRoot(rootB)}
	h.snapshotFetcher = func(_ context.Context, repoID string) (daemon.SnapshotResponse, error) {
		return snapshotWith("session-of-" + repoID), nil
	}
	return h, repoA, repoB
}

// submitHeldCreate submits a create titled title, with a prompt, through the
// naming form in the project on screen, and returns its placeholder and its
// reply — unapplied, because the daemon answers later.
func submitHeldCreate(t *testing.T, h *home, title, prompt string) (*session.Instance, tea.Msg) {
	t.Helper()
	inst := startNaming(t, h, title)
	inst.Path = h.repoRoot
	h.sidebar.SelectInstance(inst)
	h.pendingPrompt = prompt
	var reply tea.Msg
	for _, msg := range pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter}) {
		if _, ok := msg.(instanceStartedMsg); ok {
			reply = msg
		}
	}
	require.NotNil(t, reply, "the submit must start the create")
	return inst, reply
}

// TestFailedCreateDraftsAreKeptPerProject pins #7386 R1: a kept failed-create
// draft belongs to its project. A failure or a restore in one project never
// displaces or consumes the draft another project is keeping.
func TestFailedCreateDraftsAreKeptPerProject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bWatching bool
	}{
		{"a watched failure in B does not consume A's draft", true},
		{"a late failure from A does not displace B's draft", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repoA, repoB := twoCheckouts(t)
			t.Cleanup(SetSessionStarterForTest(func(*session.Instance, sessionStartRequest) (*session.Instance, error) {
				return nil, neverSent()
			}))

			// A slow create in A; the user moves to B and creates there too.
			xInst, xReply := submitHeldCreate(t, h, "x", "x prompt")
			switchProjectNow(t, h, repoB)
			yInst, yReply := submitHeldCreate(t, h, "y", "y prompt")

			if tc.bWatching {
				// A's x fails while B is on screen, so A keeps x's draft. Then y
				// fails while the user watches it: its form comes straight back,
				// and they leave it.
				h.Update(xReply)
				h.sidebar.SelectInstance(yInst)
				h.Update(yReply)
				require.Same(t, yInst, h.namingInstance, "fixture: a watched failure reopens its form")
				h.recovery = nil
				pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
				require.Nil(t, h.namingInstance)
			} else {
				// The user moves off y before it fails, so B keeps y's draft. Then
				// A's late x failure lands.
				h.sidebar.SelectInstance(findSidebarInstance(h, "session-of-"+repoB.ID))
				h.Update(yReply)
				require.Nil(t, h.namingInstance, "fixture: an unwatched failure keeps its draft")
				h.recovery = nil
				h.Update(xReply)

				h.startNewInstance()
				assert.Same(t, yInst, h.namingInstance, "B's next create reopens B's kept draft")
				assert.Equal(t, "y prompt", h.pendingPrompt)
				h.recovery = nil
				pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
			}

			switchProjectNow(t, h, repoA)
			h.startNewInstance()
			assert.Same(t, xInst, h.namingInstance, "A's next create reopens A's kept draft")
			assert.Equal(t, "x prompt", h.pendingPrompt)
		})
	}
}

// TestReportsAboutAnotherProjectsActionNameIt pins #7386 R2: a report about an
// action that ran in a project the user has left, or is leaving, names that
// project and points at its session list — the sidebar on screen is another
// project's — and no modal about it opens over the current project.
func TestReportsAboutAnotherProjectsActionNameIt(t *testing.T) {
	// Each case runs an action in A, moves to B (or starts to), and delivers
	// what the action reports. unknown marks an outcome-unknown report, which
	// must also say where to look.
	cases := []struct {
		name    string
		unknown bool
		run     func(t *testing.T) outcome
	}{
		{"kill, daemon unresponsive", true, func(t *testing.T) outcome {
			h, repoA, repoB := twoCheckouts(t)
			start := confirmedKill(t, h)
			switchProjectNow(t, h, repoB)
			h.Update(instanceKilledMsg{target: start.target, err: errDaemonUnresponsive})
			return outcome{h, repoA}
		}},
		{"kill, outcome unknown", true, func(t *testing.T) outcome {
			h, repoA, repoB := twoCheckouts(t)
			start := confirmedKill(t, h)
			switchProjectNow(t, h, repoB)
			h.Update(instanceKilledMsg{target: start.target, err: replyLost()})
			return outcome{h, repoA}
		}},
		{"archive, outcome unknown", true, func(t *testing.T) outcome {
			h, repoA, repoB := twoCheckouts(t)
			inst := archiveActionInstance(t, "repoA-worker", session.Ready)
			h.store.AddInstance(inst)
			h.sidebar.SelectInstance(inst)
			model, _ := h.handleArchive()
			h = model.(*home)
			require.NotNil(t, h.confirmationOverlay)
			h.confirmationOverlay.OnConfirm()
			start, ok := h.pendingConfirmMsg.(startArchiveMsg)
			require.True(t, ok)
			h.confirmationOverlay, h.pendingConfirmMsg, h.state = nil, nil, stateDefault
			switchProjectNow(t, h, repoB)
			h.Update(instanceArchivedMsg{target: start.target, err: replyLost()})
			return outcome{h, repoA}
		}},
		{"restore, outcome unknown", true, func(t *testing.T) outcome {
			return restoreInA(t, replyLost())
		}},
		{"restore, refused", false, func(t *testing.T) outcome {
			return restoreInA(t, errors.New("The daemon refused this restore."))
		}},
		{"create, outcome unknown while leaving", true, func(t *testing.T) outcome {
			h, repoA, repoB := twoCheckouts(t)
			inst, req := outgoingCreate(t, h, "repoA-worker")
			require.NotNil(t, startSwitch(t, h, repoB, nil))
			h.Update(instanceStartedMsg{repoID: h.repoID, repoRoot: h.repoRoot, instance: inst, draft: req, err: replyLost()})
			return outcome{h, repoA}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run(t)
			h, project := got.h, filepath.Base(got.repoA.Root)
			assert.Equal(t, stateDefault, h.state, "no modal about A's action opens over another project")
			assert.Nil(t, h.recovery)
			notice, _ := h.errBox.RetainedNotice()
			assert.Contains(t, notice, "repoA-worker", "the report names the session")
			assert.Contains(t, notice, project, "and the project the action ran in")
			assert.NotContains(t, notice, "the sidebar", "the sidebar on screen is another project's")
			if tc.unknown {
				assert.Contains(t, notice, fmt.Sprintf("%s's session list", project), "an unknown outcome points at that project's session list")
			}
		})
	}
}

// confirmedKill kills A's session through the real confirmation and returns the
// start message carrying the target it captured.
func confirmedKill(t *testing.T, h *home) startKillMsg {
	t.Helper()
	inst := newKillableInstance(t, "repoA-worker")
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	cmd := confirmKill(t, h)
	require.NotNil(t, cmd)
	start, ok := cmd().(startKillMsg)
	require.True(t, ok, "the confirmed kill must start")
	return start
}

// restoreInA restores a resting session in A, switches to B, and delivers the
// restore's reply, which answers err.
// outcome is where a report case left the TUI, and the project its action ran in.
type outcome struct {
	h     *home
	repoA *config.RepoContext
}

func restoreInA(t *testing.T, err error) outcome {
	t.Helper()
	h, repoA, repoB := twoCheckouts(t)
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(daemon.RestoreSessionRequest) (string, error) { return "", err }
	t.Cleanup(func() { restoreSessionThroughDaemon = prev })
	inst := archiveActionInstance(t, "repoA-worker", session.Lost)
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	_, cmd := h.handleRestore()
	require.NotNil(t, cmd, "restoring a resting session starts the restore")
	switchProjectNow(t, h, repoB)
	h.Update(cmd())
	return outcome{h, repoA}
}
