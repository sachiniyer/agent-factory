package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests pin #7377: a project switch's snapshot fetch never runs on the
// bubbletea event loop. switchProject is called from a key handler, so it must
// return without waiting on the daemon; the fetch runs in the tea.Cmd it
// returns, and Update applies only the reply for the switch still pending.
//
// They drive the switch only through switchProject, the returned cmd, and
// Update — the same seams the program uses — so they compile, and fail, against
// the synchronous switch too.

// asyncSwitchHome is a home on project A with one session, and project B and C
// roots to switch to.
func asyncSwitchHome(t *testing.T) (h *home, repoB, repoC *config.RepoContext) {
	t.Helper()
	h = newTestHome(t)
	h.sidebar.SetSize(40, 20)
	h.errBox.SetSize(120, 1)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.repoRoot = t.TempDir()
	h.sidebar.SetProjectName(filepath.Base(h.repoRoot))
	h.store.AddInstance(newSnapshotTestInstance(t, "repoA-session"))
	rootB, rootC := t.TempDir(), t.TempDir()
	repoB = &config.RepoContext{Root: rootB, ID: config.RepoIDFromRoot(rootB)}
	repoC = &config.RepoContext{Root: rootC, ID: config.RepoIDFromRoot(rootC)}
	return h, repoB, repoC
}

// snapshotWith is a one-session snapshot.
func snapshotWith(title string) daemon.SnapshotResponse {
	return daemon.SnapshotResponse{Instances: []session.InstanceData{{Title: title, CreatedAt: time.Now()}}}
}

// heldReply is a fetch that answers only once released, whatever its ctx says:
// a reply that arrives late, after the switch it answers was cancelled or
// replaced.
type heldReply struct {
	once    sync.Once
	release chan struct{}
}

func newHeldReply(t *testing.T) *heldReply {
	r := &heldReply{release: make(chan struct{})}
	t.Cleanup(r.Release)
	return r
}

func (r *heldReply) Release() { r.once.Do(func() { close(r.release) }) }

func (r *heldReply) wait() { <-r.release }

// startSwitch calls switchProject the way a key handler does and returns its
// cmd. A switch that waits on the daemon before returning parks the event
// loop, so it fails here instead of hanging the test. held, if set, is
// released before the stuck call is waited out, so it cannot race the
// teardown of h.
func startSwitch(t *testing.T, h *home, repo *config.RepoContext, held *heldReply) tea.Cmd {
	t.Helper()
	done := make(chan tea.Cmd, 1)
	go func() {
		_, cmd := h.switchProject(repo)
		done <- cmd
	}()
	select {
	case cmd := <-done:
		return cmd
	case <-time.After(2 * time.Second):
		t.Cleanup(func() {
			if held != nil {
				held.Release()
			}
			<-done
		})
		t.Fatalf("switchProject(%s) did not return: the snapshot fetch ran on the event loop", filepath.Base(repo.Root))
		return nil
	}
}

// runAsync runs cmd off the test goroutine, as the program runs a tea.Cmd.
func runAsync(cmd tea.Cmd) <-chan tea.Msg {
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	return out
}

func receive(t *testing.T, msgs <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-msgs:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("the switch's fetch never replied")
		return nil
	}
}

// bar is the status bar as painted.
func bar(h *home) string { return h.errBox.String() }

// TestSwitchProjectFetchRunsInTheCmdNotOnTheEventLoop: switchProject returns
// before the daemon is asked anything, the bar says what is loading, and the
// switch lands when the cmd's reply goes through Update.
func TestSwitchProjectFetchRunsInTheCmdNotOnTheEventLoop(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	repoAID, repoARoot := h.repoID, h.repoRoot
	var calls atomic.Int32
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		calls.Add(1)
		return snapshotWith("repoB-session"), nil
	}

	cmd := startSwitch(t, h, repoB, nil)
	assert.Equal(t, int32(0), calls.Load(), "switchProject must not fetch on the event loop")
	assert.Equal(t, repoAID, h.repoID, "nothing switches before the reply lands")
	assert.Equal(t, repoARoot, h.repoRoot)
	assert.Contains(t, bar(h), "Loading "+filepath.Base(repoB.Root)+"…",
		"the bar must say which project is loading")
	require.NotNil(t, cmd)

	h.Update(cmd())
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, repoB.ID, h.repoID, "the reply switches the project")
	assert.Equal(t, repoB.Root, h.repoRoot)
	require.NotNil(t, findSidebarInstance(h, "repoB-session"))
	assert.Nil(t, findSidebarInstance(h, "repoA-session"))
	assert.NotContains(t, bar(h), "Loading", "the loading line goes once the switch lands")
}

// TestSwitchProjectWaitsOutAWarmingDaemonInsideTheCmd: a daemon still restoring
// its sessions is waited out in the cmd, never on the event loop, and nothing
// is written to stdout, where it would paint over the alt-screen.
func TestSwitchProjectWaitsOutAWarmingDaemonInsideTheCmd(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	prevPoll := coldStartWarmupPoll
	coldStartWarmupPoll = time.Millisecond
	t.Cleanup(func() { coldStartWarmupPoll = prevPoll })
	var calls atomic.Int32
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		if calls.Add(1) < 3 {
			return daemon.SnapshotResponse{}, errDaemonStarting()
		}
		return snapshotWith("repoB-session"), nil
	}

	var cmd tea.Cmd
	printed := captureStdout(t, func() {
		cmd = startSwitch(t, h, repoB, nil)
		assert.Equal(t, int32(0), calls.Load(), "a warming daemon must not be waited on from the event loop")
		require.NotNil(t, cmd)
		h.Update(cmd())
	})
	assert.Empty(t, printed, "nothing may write to stdout under the alt-screen")
	assert.Equal(t, int32(3), calls.Load(), "the cmd retries while the daemon warms")
	assert.Equal(t, repoB.ID, h.repoID, "the warmed daemon's answer switches the project")
	require.NotNil(t, findSidebarInstance(h, "repoB-session"))
}

// captureStdout returns what fn wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	prev := os.Stdout
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	func() {
		defer func() { os.Stdout = prev }()
		fn()
	}()
	require.NoError(t, w.Close())
	return <-out
}

// TestSwitchProjectAppliesOnlyTheLatestReply: a reply for a switch a newer one
// replaced is dropped, whether it lands before or after the newer reply.
func TestSwitchProjectAppliesOnlyTheLatestReply(t *testing.T) {
	for _, order := range []string{"late reply first", "late reply last"} {
		t.Run(order, func(t *testing.T) {
			h, repoB, repoC := asyncSwitchHome(t)
			repoAID := h.repoID
			held := newHeldReply(t)
			h.snapshotFetcher = func(_ context.Context, repoID string) (daemon.SnapshotResponse, error) {
				if repoID == repoB.ID {
					held.wait()
					return snapshotWith("repoB-session"), nil
				}
				return snapshotWith("repoC-session"), nil
			}

			lateB := runAsync(startSwitch(t, h, repoB, held))
			cmdC := startSwitch(t, h, repoC, held)
			require.NotNil(t, cmdC)
			assert.Contains(t, bar(h), "Loading "+filepath.Base(repoC.Root)+"…",
				"the newer switch owns the loading line")

			held.Release()
			if order == "late reply first" {
				h.Update(receive(t, lateB))
				assert.Equal(t, repoAID, h.repoID, "a replaced switch's reply must not switch the project")
				assert.Nil(t, findSidebarInstance(h, "repoB-session"))
				assert.Contains(t, bar(h), "Loading "+filepath.Base(repoC.Root)+"…",
					"a replaced switch's reply must not end the newer switch")
				h.Update(cmdC())
			} else {
				h.Update(cmdC())
				h.Update(receive(t, lateB))
			}

			assert.Equal(t, repoC.ID, h.repoID, "the latest switch wins")
			assert.Equal(t, repoC.Root, h.repoRoot)
			require.NotNil(t, findSidebarInstance(h, "repoC-session"))
			assert.Nil(t, findSidebarInstance(h, "repoB-session"), "the replaced switch's sessions must never appear")
			retained, _ := h.errBox.RetainedNotice()
			assert.Empty(t, retained, "a dropped reply raises nothing")
		})
	}
}

// TestSwitchProjectEscCancelsAndDropsTheLateReply: Esc ends a pending switch
// and keeps the current project, and the reply that arrives afterwards is
// dropped.
func TestSwitchProjectEscCancelsAndDropsTheLateReply(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	repoAID, repoARoot := h.repoID, h.repoRoot
	held := newHeldReply(t)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		held.wait()
		return snapshotWith("repoB-session"), nil
	}

	late := runAsync(startSwitch(t, h, repoB, held))
	require.Contains(t, bar(h), projectSwitchCancelHintForTest, "the loading line says how to cancel")
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	assert.NotContains(t, bar(h), "Loading", "Esc ends the loading state")

	held.Release()
	h.Update(receive(t, late))
	assert.Equal(t, repoAID, h.repoID, "a cancelled switch's reply must not switch the project")
	assert.Equal(t, repoARoot, h.repoRoot)
	require.NotNil(t, findSidebarInstance(h, "repoA-session"))
	assert.Nil(t, findSidebarInstance(h, "repoB-session"))
	retained, _ := h.errBox.RetainedNotice()
	assert.Empty(t, retained, "a cancelled switch raises nothing")
}

// projectSwitchCancelHintForTest is the loading line's cancel hint, spelled out
// so this file does not depend on the constant it pins.
const projectSwitchCancelHintForTest = "esc to cancel"

// TestSwitchProjectHoldsKeysWhileLoading: a key pressed while a switch loads
// must not act on the project the user is leaving.
func TestSwitchProjectHoldsKeysWhileLoading(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return snapshotWith("repoB-session"), nil
	}

	cmd := startSwitch(t, h, repoB, nil)
	require.NotNil(t, cmd)
	// n would open the new-session form on the outgoing project; ? the help.
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	assert.Equal(t, stateDefault, h.state, "no key may act while the switch loads")
	assert.Nil(t, h.namingInstance, "no session may be started on the outgoing project")

	h.Update(cmd())
	require.Equal(t, repoB.ID, h.repoID)
	h.startNewInstance()
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, repoB.Root, h.namingInstance.Path, "once the switch lands, new sessions target it")
}

// TestSwitchProjectDeadlineToastIsActionable: a daemon that accepts the request
// and never answers ends the switch with the wait and a next step, never the
// transport's text, and the current project is left as it was.
func TestSwitchProjectDeadlineToastIsActionable(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	repoAID := h.repoID
	prevTimeout := switchSnapshotFetchTimeout
	switchSnapshotFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { switchSnapshotFetchTimeout = prevTimeout })
	h.snapshotFetcher = func(ctx context.Context, _ string) (daemon.SnapshotResponse, error) {
		<-ctx.Done()
		return daemon.SnapshotResponse{}, fmt.Errorf("Post \"http://af/v1/Snapshot\": %w", ctx.Err())
	}

	cmd := startSwitch(t, h, repoB, nil)
	require.NotNil(t, cmd)
	h.Update(receive(t, runAsync(cmd)))

	notice, failure := h.errBox.RetainedNotice()
	assert.True(t, failure, "a switch that could not load is a failure")
	assert.Contains(t, notice, filepath.Base(repoB.Root), "the toast names the project")
	assert.Contains(t, notice, "no answer from the daemon within 50ms", "the toast says the daemon did not answer, and for how long")
	assert.Contains(t, notice, "af daemon status", "the toast gives a next step")
	assert.NotContains(t, notice, "context deadline exceeded", "transport text is for the log, not the toast")
	assert.NotContains(t, notice, "http://", "transport text is for the log, not the toast")
	assert.Equal(t, repoAID, h.repoID, "a switch that timed out leaves the current project")
	require.NotNil(t, findSidebarInstance(h, "repoA-session"))
}

// TestSnapshotThroughDaemonIsBoundedByItsCtx drives the production fetcher —
// snapshotThroughDaemon, through the real HTTP client — against a daemon socket
// that accepts the request and never answers (#7377, the line #7365 fixed). It
// must return at its ctx's deadline: a fetch that ignored its ctx would hold
// the switch's "Loading…" until the daemon answered, which may be never.
func TestSnapshotThroughDaemonIsBoundedByItsCtx(t *testing.T) {
	sock := testguard.SocketPath(t, "daemon-http.sock")
	unwedge := make(chan struct{})
	var unwedgeOnce sync.Once
	release := func() { unwedgeOnce.Do(func() { close(unwedge) }) }
	counter := serveDaemonHTTP(t, sock, func(_ http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-unwedge:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(release)
	pointDaemonHTTPAt(t, sock)

	const bound = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := snapshotThroughDaemon(ctx, "repo")
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		require.Error(t, r.err, "a daemon that never answered is not an empty snapshot")
		assert.ErrorIs(t, r.err, context.DeadlineExceeded, "the read ends at its ctx's deadline")
		assert.GreaterOrEqual(t, r.elapsed, bound/2, "the request reached the wedged handler and waited for the deadline")
	case <-time.After(5 * time.Second):
		// Let the stuck call finish before pointDaemonHTTPAt's cleanup swaps
		// back the seam it is still reading.
		t.Cleanup(func() {
			release()
			<-done
		})
		t.Fatalf("snapshotThroughDaemon outlived its %s deadline against a daemon that never answers", bound)
	}
	assert.GreaterOrEqual(t, counter.get("/v1/Snapshot"), 1, "the request must reach the daemon socket")
}

// outgoingCreate is a create in flight in the project on screen, its Loading
// row selected — what n, a name and Enter leave behind.
func outgoingCreate(t *testing.T, h *home, title string) (*session.Instance, *sessionStartRequest) {
	t.Helper()
	inst := newLoadingInstance(t, title)
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	return inst, &sessionStartRequest{Title: title, RepoPath: h.repoRoot, Program: "claude", Prompt: "p"}
}

// TestSwitchProjectFailedCreateMeanwhileOpensNothingForTheOutgoingProject pins
// #7379 R1: a switch commits only from the state it started in. A create in the
// project being left that fails while the next one loads raises no recovery
// screen and restores no form: the user has left that project. The failure is
// still reported, and its draft is kept for that project's next create — so
// no form for it is open once the switch commits, and its session never shows
// in the next project's sidebar.
func TestSwitchProjectFailedCreateMeanwhileOpensNothingForTheOutgoingProject(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return snapshotWith("repoB-session"), nil
	}
	inst, req := outgoingCreate(t, h, "repoA-create")

	cmd := startSwitch(t, h, repoB, nil)
	h.Update(instanceStartedMsg{instance: inst, draft: req, rawPrompt: "p", err: neverSent()})

	assert.Equal(t, stateDefault, h.state, "no form opens for the project being left")
	assert.Nil(t, h.namingInstance, "the failed draft is not restored while the switch loads")
	assert.Nil(t, h.recovery, "no modal while the switch loads: keys are held, so nothing could dismiss it")
	assert.Contains(t, bar(h), "Loading "+filepath.Base(repoB.Root)+"…", "the switch still leads the bar")
	assert.Contains(t, bar(h), "Cannot create session", "the failure is still reported")
	require.NotNil(t, h.failedCreate, "the draft is kept for the outgoing project's next create")

	h.Update(cmd())
	require.Equal(t, repoB.ID, h.repoID, "the switch commits")
	assert.Equal(t, stateDefault, h.state, "no form for the outgoing project is open after the commit")
	assert.Nil(t, h.namingInstance)
	assert.Nil(t, h.recovery)
	assert.Nil(t, findSidebarInstance(h, "repoA-create"), "the outgoing project's session never appears in the next one's sidebar")
	require.NotNil(t, findSidebarInstance(h, "repoB-session"))

	// A create in B opens a fresh form: the kept draft is the outgoing project's.
	h.startNewInstance()
	require.NotNil(t, h.namingInstance)
	assert.NotSame(t, inst, h.namingInstance, "the outgoing project's draft must not reopen in the next project")
	assert.Equal(t, repoB.Root, h.namingInstance.Path)
}

// TestSwitchProjectStartedCreateMeanwhileOpensNoStartHelp: the same property for
// a create that succeeds while the next project loads — no start-help modal and
// no pane for a session in the project being left.
func TestSwitchProjectStartedCreateMeanwhileOpensNoStartHelp(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return snapshotWith("repoB-session"), nil
	}
	inst, _ := outgoingCreate(t, h, "repoA-create")

	cmd := startSwitch(t, h, repoB, nil)
	h.Update(instanceStartedMsg{instance: inst})

	assert.Equal(t, stateDefault, h.state, "no start-help modal for a session in the project being left")
	assert.Nil(t, h.textOverlay)
	assert.Zero(t, h.store.NumOpenPanes(), "no pane opens for it either")

	h.Update(cmd())
	require.Equal(t, repoB.ID, h.repoID)
	assert.Equal(t, stateDefault, h.state, "nothing from the outgoing project is over the next one")
	assert.Nil(t, h.textOverlay)
	assert.Nil(t, findSidebarInstance(h, "repoA-create"))
}

// TestSwitchProjectFailedKillMeanwhileOpensNoModal: a kill in the project being
// left that fails while the next one loads raises neither the recovery screen
// nor the daemon-restart offer; the bar reports it.
func TestSwitchProjectFailedKillMeanwhileOpensNoModal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"refused", errors.New("The daemon refused this kill."), "Cannot kill session"},
		{"daemon unresponsive", errDaemonUnresponsive, "could not be confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repoB, _ := asyncSwitchHome(t)
			h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
				return snapshotWith("repoB-session"), nil
			}
			inst := findSidebarInstance(h, "repoA-session")
			require.NotNil(t, inst)
			target := captureSessionActionTarget(inst, h.repoID)

			cmd := startSwitch(t, h, repoB, nil)
			h.Update(instanceKilledMsg{target: target, err: tc.err})

			assert.Equal(t, stateDefault, h.state, "no confirmation opens for the project being left")
			assert.Nil(t, h.recovery, "no recovery screen opens for the project being left")
			assert.Contains(t, bar(h), tc.want, "the failure is still reported")

			h.Update(cmd())
			require.Equal(t, repoB.ID, h.repoID)
			assert.Equal(t, stateDefault, h.state)
			assert.Nil(t, h.recovery)
		})
	}
}

// TestSwitchProjectLoadingLineSurvivesANotice pins #7379 R2: while a switch is
// pending the bar always says so — keys are held, and it is the only thing that
// says why — even while a transient notice is up, such as the rebind
// follow-up's toast raised in the same Update that starts the switch.
func TestSwitchProjectLoadingLineSurvivesANotice(t *testing.T) {
	h, repoB, _ := asyncSwitchHome(t)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return snapshotWith("repoB-session"), nil
	}

	cmd := startSwitch(t, h, repoB, nil)
	h.showTransientMessage("Rebound project-a to /new/root")
	got := bar(h)
	assert.Contains(t, got, "Loading "+filepath.Base(repoB.Root)+"… · "+projectSwitchCancelHintForTest,
		"the loading line must not yield to a notice")
	assert.Contains(t, got, "Rebound project-a", "the notice still shows beside it")

	h.handleError(errors.New("an unrelated failure"))
	assert.Contains(t, bar(h), "Loading "+filepath.Base(repoB.Root)+"…", "nor to a failure")

	h.Update(cmd())
	assert.NotContains(t, bar(h), "Loading", "the loading line goes once the switch lands")
}
