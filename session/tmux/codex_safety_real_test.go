package tmux

import (
	"bufio"
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

const codexSafetyRealFixtureEnv = "AF_CODEX_SAFETY_REAL_FIXTURE"

// socketExecutor forces every production tmux command issued by TmuxSession
// onto one test-owned server. It leaves the user's default tmux server entirely
// untouched while still exercising the real capture-pane/send-keys boundary.
type socketExecutor struct {
	socket string
}

func (e socketExecutor) Run(command *exec.Cmd) error {
	e.scope(command)
	return command.Run()
}

func (e socketExecutor) Output(command *exec.Cmd) ([]byte, error) {
	e.scope(command)
	return command.Output()
}

func (e socketExecutor) scope(command *exec.Cmd) {
	command.Args = append([]string{command.Args[0], "-S", e.socket}, command.Args[1:]...)
}

// tmuxAt builds a tmux command against this test's private server. `-S` names
// the socket outright rather than deriving it from TMUX_TMPDIR, because the
// derived path is what breaks: the package sandbox roots TMUX_TMPDIR under
// os.TempDir(), which on macOS is /var/folders/<hash>/T (~49 bytes), and a
// label appended to that overruns darwin's 104-byte sun_path — tmux then
// refuses with a bare "exit status 1" that names nothing. testguard.SocketPath
// hands back a short path and fails up front if it ever stops being short.
func tmuxAt(socket string, args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-S", socket}, args...)...)
}

func TestCodexSafetyDelayedRenderFixtureProcess(t *testing.T) {
	if os.Getenv(codexSafetyRealFixtureEnv) != "1" {
		t.Skip("fixture process")
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	require.NoError(t, err)
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	renderFixturePane(codexSafetyRealNormalPane, true)
	reader := bufio.NewReader(os.Stdin)
	require.NoError(t, waitForFixtureByte(reader, 'x'))
	renderFixturePane(codexCurrentSafetyBufferingDialog, false)
	require.NoError(t, waitForFixtureByte(reader, 'B'))

	// This is the production rendering race from #2673: tmux has delivered the
	// navigation key, but Codex has not painted the new selected row by af's
	// immediate capture.
	time.Sleep(500 * time.Millisecond)
	renderFixturePane(codexCurrentSafetyBufferingWaitSelected, false)
	require.NoError(t, waitForFixtureByte(reader, '\r'))
	renderFixturePane(codexSafetyRealNormalPane, true)

	// Hold the pane open until the test's cleanup kills the session, and no
	// longer. A timed exit races the runner: exit-empty would tear the server
	// down mid-assertion on a loaded box, and cleanup would then report the
	// session it wanted gone as a failure to remove it. Reads end at EOF when
	// the server does go away, so nothing outlives it either.
	_, _ = reader.ReadByte()
}

const codexSafetyRealNormalPane = `• Working

  gpt-5.6-sol max · ~/agent-factory`

func renderFixturePane(content string, cursorVisible bool) {
	cursorMode := "\x1b[?25l"
	if cursorVisible {
		cursorMode = "\x1b[?25h"
	}
	_, _ = fmt.Fprintf(os.Stdout, "\x1b[2J\x1b[H%s%s\r\n", cursorMode, strings.ReplaceAll(content, "\n", "\r\n"))
}

func waitForFixtureByte(reader *bufio.Reader, wanted byte) error {
	for {
		got, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if got == wanted {
			return nil
		}
	}
}

func TestCheckAndHandleTrustPrompt_CodexSafetyWaitsForRealTmuxRender(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux is not installed: %v", err)
	}
	info, _, errorLogs := captureTrustPromptLogs(t)
	socketPath := testguard.SocketPath(t, "af2673.sock")
	name := "af_safety-render-2673"
	fixtureDir := t.TempDir()
	fixturePath := filepath.Join(fixtureDir, ProgramCodex)
	executable, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(executable, fixturePath))

	program := fixturePath + " -test.run=^TestCodexSafetyDelayedRenderFixtureProcess$"
	start := tmuxAt(socketPath, "new-session", "-d", "-s", name, "-x", "100", "-y", "24", program)
	start.Env = append(os.Environ(), codexSafetyRealFixtureEnv+"=1")
	// CombinedOutput, not Run: tmux explains every refusal on stderr, and
	// discarding it leaves a bare "exit status 1" to diagnose from CI alone.
	out, err := start.CombinedOutput()
	require.NoError(t, err, "start isolated tmux fixture session: %s", out)
	out, err = tmuxAt(socketPath, "set-option", "-g", "exit-empty", "on").CombinedOutput()
	require.NoError(t, err, "set exit-empty on the isolated server: %s", out)

	serverPID := tmuxServerPID(t, socketPath, name)
	t.Cleanup(func() {
		if killOut, killErr := tmuxAt(socketPath, "kill-session", "-t", exactTarget(name)).CombinedOutput(); killErr != nil {
			// A session that is already gone is cleanup's goal reached early,
			// not a failure — kill-session and has-session both exit nonzero
			// once the server is down, so ask before reporting.
			if probeErr := tmuxAt(socketPath, "has-session", "-t", exactTarget(name)).Run(); probeErr == nil {
				t.Errorf("kill isolated tmux session: %v: %s", killErr, killOut)
			}
		}
		if !eventually(2*time.Second, 20*time.Millisecond, func() bool {
			return stderrors.Is(syscall.Kill(serverPID, 0), syscall.ESRCH)
		}) {
			t.Errorf("isolated tmux server %d did not exit", serverPID)
			return
		}
		if removeErr := os.Remove(socketPath); removeErr != nil && !os.IsNotExist(removeErr) {
			t.Errorf("remove isolated tmux socket %s: %v", socketPath, removeErr)
		}
	})

	session := newTmuxSession(name, program, MakePtyFactory(), socketExecutor{socket: socketPath})
	waitForRealPane(t, session, codexSafetyRealNormalPane)
	require.False(t, session.CheckAndHandleTrustPrompt(), "normal pane establishes the pre-dialog model")

	out, err = tmuxAt(socketPath, "send-keys", "-t", exactTarget(name), "x").CombinedOutput()
	require.NoError(t, err, "raise the fixture's safety picker: %s", out)
	waitForRealPane(t, session, "Dismiss and keep waiting")
	require.True(t, session.CheckAndHandleTrustPrompt(), "the live safety picker blocks prompt delivery")
	require.Empty(t, errorLogs.String(), "one stale frame after a real navigation key is pending, not an error")

	waitForRealPane(t, session, codexCurrentSafetyBufferingWaitSelected)
	require.True(t, session.CheckAndHandleTrustPrompt(), "af accepts only after the target row is visibly selected")
	waitForRealPane(t, session, codexSafetyRealNormalPane)
	require.False(t, session.CheckAndHandleTrustPrompt(), "the next live poll verifies the unchanged model")

	require.Empty(t, errorLogs.String())
	require.Contains(t, info.String(), "verified model unchanged: gpt-5.6-sol max")
}

func tmuxServerPID(t *testing.T, socket, name string) int {
	t.Helper()
	output, err := tmuxAt(socket, "display-message", "-p", "-t", exactTarget(name), "#{pid}").Output()
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	return pid
}

func waitForRealPane(t *testing.T, session *TmuxSession, wanted string) {
	t.Helper()
	var last string
	require.Eventually(t, func() bool {
		content, err := session.CapturePaneContent()
		if err != nil {
			last = err.Error()
			return false
		}
		last = content
		return strings.Contains(content, wanted)
	}, 3*time.Second, 20*time.Millisecond, "pane never rendered %q; last capture:\n%s", wanted, last)
}

func eventually(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return check()
}
