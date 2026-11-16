package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These pin #7381's follow-ups to the async project switch (#7377/#7379). Like
// switch_project_async_test.go they drive only switchProject, the cmds the TUI
// returns, and Update, so they compile against the code before the fix too.

// TestSwitchProjectWarmingDaemonCutShortByTheDeadlineIsStillWarming: an attempt
// whose daemon answered "starting" counts as warming, never as "no answer" —
// even when the attempt's deadline cuts its admission retry short (#7381). The
// production fetcher runs against a daemon socket that refuses admission for a
// while, then answers; the switch must wait it out and land.
func TestSwitchProjectWarmingDaemonCutShortByTheDeadlineIsStillWarming(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	// An attempt deadline shorter than the 5s admission retry stands in for a
	// slow ensure: either way the deadline fires while the daemon is still
	// answering "starting".
	prevTimeout, prevPoll := switchSnapshotFetchTimeout, coldStartWarmupPoll
	switchSnapshotFetchTimeout, coldStartWarmupPoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { switchSnapshotFetchTimeout, coldStartWarmupPoll = prevTimeout, prevPoll })

	answer, err := json.Marshal(snapshotWith("repoB-session"))
	require.NoError(t, err)
	sock := testguard.SocketPath(t, "daemon-http.sock")
	counter := serveDaemonHTTP(t, sock, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		if n <= 6 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"data":null,"error":{"message":"agent-factory daemon is starting (restoring sessions); retry shortly"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":` + string(answer) + `,"error":null}`))
	})
	pointDaemonHTTPAt(t, sock)
	h.snapshotFetcher = snapshotThroughDaemon

	cmd := startSwitch(t, h, repoB, nil)
	require.NotNil(t, cmd)
	h.Update(receive(t, runAsync(cmd)))

	notice, _ := h.errBox.RetainedNotice()
	assert.NotContains(t, notice, "no answer from the daemon", "a daemon that answered \"starting\" is warming, not silent")
	assert.Equal(t, repoB.ID, h.repoID, "the switch waits out the warm-up and lands")
	require.NotNil(t, findSidebarInstance(h, "repoB-session"))
	assert.Greater(t, counter.get("/v1/Snapshot"), 6, "the daemon was asked again once it had warmed")
}

// TestSwitchProjectLoadingLineShowsOverTheFullScreenViews: while a switch is
// pending its loading line is on every frame, including the full-screen
// "Cannot reach the daemon" outage view and a recovery screen, which replace
// the status bar (#7381). Keys are held meanwhile, and nothing else on screen
// says why or that Esc cancels.
func TestSwitchProjectLoadingLineShowsOverTheFullScreenViews(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	resizeHome(h, 100, 30)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return snapshotWith("repoB-session"), nil
	}
	cmd := startSwitch(t, h, repoB, nil)
	require.NotNil(t, cmd)
	loading := "Loading " + filepath.Base(repoB.Root) + "… · " + projectSwitchCancelHintForTest

	h.snapshotUnavailable = true
	view := h.View()
	requireViewSized(t, view, 100, 30)
	assert.Contains(t, ansi.Strip(view), "Cannot reach the daemon")
	assert.Contains(t, ansi.Strip(view), loading, "the outage view must keep the loading line")

	h.snapshotUnavailable = false
	h.recovery = &recoveryNotice{"Cannot kill session", "The session is retained.", "Press any key to return to the session."}
	view = h.View()
	requireViewSized(t, view, 100, 30)
	assert.Contains(t, ansi.Strip(view), "Cannot kill session")
	assert.Contains(t, ansi.Strip(view), loading, "a recovery screen must keep the loading line")
	h.recovery = nil

	h.Update(cmd())
	h.snapshotUnavailable = true
	assert.NotContains(t, ansi.Strip(h.View()), "Loading", "no loading line once the switch has landed")
}

// TestSwitchProjectCreateReplyAfterTheCommitTouchesOnlyItsProject: a create made
// in A whose reply lands after the switch to B committed is applied to A, never
// to B's store — not even to a B session that happens to share its title — and
// what it says names A (#7381).
func TestSwitchProjectCreateReplyAfterTheCommitTouchesOnlyItsProject(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"failed", neverSent()},
		{"started", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repoB, _ := asyncSwitchHome(t)
			// The submit's preflight resolves the project's config, so A is a
			// real checkout here.
			h.repoRoot = initTestGitRepo(t)
			repoAID, repoARoot := h.repoID, h.repoRoot
			const title = "shared-title"
			h.snapshotFetcher = func(_ context.Context, repoID string) (daemon.SnapshotResponse, error) {
				if repoID == repoB.ID {
					return snapshotWith(title), nil
				}
				return snapshotWith("repoA-session"), nil
			}
			t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, _ sessionStartRequest) (*session.Instance, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return inst, nil
			}))

			// Submit a create in A through the form, and hold its reply.
			inst := startNaming(t, h, title)
			inst.Path = repoARoot
			var reply tea.Msg
			for _, msg := range pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter}) {
				if _, ok := msg.(instanceStartedMsg); ok {
					reply = msg
				}
			}
			require.NotNil(t, reply, "the submit must start the create")

			// The user moves to B before A's create answers.
			switchProjectNow(t, h, repoB)
			require.Equal(t, repoB.ID, h.repoID)
			bRow := findSidebarInstance(h, title)
			require.NotNil(t, bRow, "fixture: B has its own session with the same title")

			h.Update(reply)

			assert.Same(t, bRow, findSidebarInstance(h, title), "B's session must not be removed or replaced by A's create")
			assert.Len(t, sidebarTitled(h, title), 1, "A's session must not be added to B's sidebar")
			assert.Equal(t, stateDefault, h.state, "nothing from A opens over B")
			assert.Nil(t, h.recovery)
			assert.Nil(t, h.textOverlay)
			assert.Nil(t, h.namingInstance)
			notice, _ := h.errBox.RetainedNotice()
			assert.Contains(t, notice, filepath.Base(repoARoot), "what the create says names the project it was made in")
			assert.Contains(t, notice, title, "and the session")

			if tc.err == nil {
				return
			}
			// Back in A, the next create reopens the failed draft there.
			switchProjectNow(t, h, &config.RepoContext{Root: repoARoot, ID: repoAID})
			h.startNewInstance()
			assert.Same(t, inst, h.namingInstance, "A's next create reopens A's failed draft")
		})
	}
}

// sidebarTitled is every row in the store with this title.
func sidebarTitled(h *home, title string) []*session.Instance {
	var out []*session.Instance
	for _, inst := range h.store.GetInstances() {
		if inst.Title == title {
			out = append(out, inst)
		}
	}
	return out
}

// TestSwitchProjectRedirectedKillOrArchiveNamesSessionAndProject: a kill or
// archive in the project being left that fails while the next one loads is
// reported on the bar rather than a modal (#7379), and that report names the
// session and its project — it can outlast the switch into the next project
// (#7381).
func TestSwitchProjectRedirectedKillOrArchiveNamesSessionAndProject(t *testing.T) {
	for _, verb := range []string{"kill", "archive"} {
		t.Run(verb, func(t *testing.T) {
			h, repoB, _ := asyncSwitchHome(t)
			h.errBox.SetSize(300, 1)
			h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
				return snapshotWith("repoB-session"), nil
			}
			inst := findSidebarInstance(h, "repoA-session")
			require.NotNil(t, inst)
			target := captureSessionActionTarget(inst, h.repoID)

			cmd := startSwitch(t, h, repoB, nil)
			require.NotNil(t, cmd)
			refused := errors.New("The daemon refused this " + verb + ".")
			if verb == "kill" {
				h.Update(instanceKilledMsg{target: target, err: refused})
			} else {
				h.Update(instanceArchivedMsg{target: target, err: refused})
			}

			require.Nil(t, h.recovery, "a failure for the project being left raises no modal (#7379)")
			notice, _ := h.errBox.RetainedNotice()
			assert.Contains(t, notice, "repoA-session", "the report names the session")
			assert.Contains(t, notice, filepath.Base(h.repoRoot), "and the project it is in")
			assert.Contains(t, notice, "The daemon refused this "+verb+".", "and why")
		})
	}
}

// TestSwitchProjectArchiveReplyForAnEarlierProjectNamesThatProject: an archive
// started in A whose failure lands after the switch to B committed names A —
// the project the action ran in, not whichever one is on screen or loading —
// and raises no modal over B (Codex on #7385). Both with a further switch from
// B pending and with none.
func TestSwitchProjectArchiveReplyForAnEarlierProjectNamesThatProject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
	}{
		{"after the commit", false},
		{"while a further switch loads", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repoB, repoC := asyncSwitchHome(t)
			h.errBox.SetSize(300, 1)
			repoARoot := h.repoRoot
			h.snapshotFetcher = func(_ context.Context, repoID string) (daemon.SnapshotResponse, error) {
				return snapshotWith("session-of-" + repoID), nil
			}

			// Archive a session in A through its confirmation, as the user does.
			inst := archiveActionInstance(t, "repoA-worker", session.Ready)
			h.store.AddInstance(inst)
			h.sidebar.SelectInstance(inst)
			model, _ := h.handleArchive()
			h = model.(*home)
			require.NotNil(t, h.confirmationOverlay)
			h.confirmationOverlay.OnConfirm()
			start, ok := h.pendingConfirmMsg.(startArchiveMsg)
			require.True(t, ok, "the confirmed archive must start")
			h.confirmationOverlay, h.pendingConfirmMsg, h.state = nil, nil, stateDefault

			switchProjectNow(t, h, repoB)
			require.Equal(t, repoB.ID, h.repoID)
			if tc.pending {
				require.NotNil(t, startSwitch(t, h, repoC, nil))
			}
			h.Update(instanceArchivedMsg{target: start.target, err: errors.New("The daemon refused this archive.")})

			assert.Nil(t, h.recovery, "no modal about A's session over another project")
			notice, _ := h.errBox.RetainedNotice()
			assert.Contains(t, notice, "repoA-worker", "the report names the session")
			assert.Contains(t, notice, filepath.Base(repoARoot), "and the project the archive ran in")
			assert.NotContains(t, notice, filepath.Base(repoB.Root), "not the project on screen")
		})
	}
}
