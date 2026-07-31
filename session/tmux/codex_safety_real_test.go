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
)

const codexSafetyRealFixtureEnv = "AF_CODEX_SAFETY_REAL_FIXTURE"

// socketExecutor forces every production tmux command issued by TmuxSession
// onto one test-owned server. It leaves the user's default tmux server entirely
// untouched while still exercising the real capture-pane/send-keys boundary.
type socketExecutor struct {
	label string
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
	command.Args = append([]string{command.Args[0], "-L", e.label}, command.Args[1:]...)
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
	time.Sleep(5 * time.Second)
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
	info, _, errorLogs := captureTrustPromptLogs(t)
	label := fmt.Sprintf("af2673-%d-%d", os.Getpid(), time.Now().UnixNano())
	name := "af_safety-render-2673"
	fixtureDir := t.TempDir()
	fixturePath := filepath.Join(fixtureDir, ProgramCodex)
	executable, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(executable, fixturePath))

	program := fixturePath + " -test.run=^TestCodexSafetyDelayedRenderFixtureProcess$"
	start := exec.Command(
		"tmux", "-L", label, "new-session", "-d",
		"-s", name, "-x", "100", "-y", "24", program,
	)
	start.Env = append(os.Environ(), codexSafetyRealFixtureEnv+"=1")
	require.NoError(t, start.Run())
	require.NoError(t, exec.Command("tmux", "-L", label, "set-option", "-g", "exit-empty", "on").Run())

	socketPath := tmuxSocketPath(t, label, name)
	serverPID := tmuxServerPID(t, label, name)
	t.Cleanup(func() {
		if killErr := exec.Command("tmux", "-L", label, "kill-session", "-t", name).Run(); killErr != nil {
			t.Errorf("kill isolated tmux session: %v", killErr)
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

	session := newTmuxSession(name, program, MakePtyFactory(), socketExecutor{label: label})
	waitForRealPane(t, session, codexSafetyRealNormalPane)
	require.False(t, session.CheckAndHandleTrustPrompt(), "normal pane establishes the pre-dialog model")

	require.NoError(t, exec.Command("tmux", "-L", label, "send-keys", "-t", exactTarget(name), "x").Run())
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

func tmuxSocketPath(t *testing.T, label, name string) string {
	t.Helper()
	output, err := exec.Command(
		"tmux", "-L", label, "display-message", "-p", "-t", exactTarget(name), "#{socket_path}",
	).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func tmuxServerPID(t *testing.T, label, name string) int {
	t.Helper()
	output, err := exec.Command(
		"tmux", "-L", label, "display-message", "-p", "-t", exactTarget(name), "#{pid}",
	).Output()
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
