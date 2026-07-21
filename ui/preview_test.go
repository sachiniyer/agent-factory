package ui

import (
	"bytes"
	"fmt"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"
)

// testSetup holds common test setup data
type testSetup struct {
	workdir     string
	instance    *session.Instance
	sessionName string
	cleanupFn   func()
}

// setupTestEnvironment creates a common test environment with git repo and instance
func setupTestEnvironment(t *testing.T, cmdExec cmd_test.MockCmdExec) *testSetup {
	t.Helper()

	// Initialize logging
	log.Initialize(false)

	// Isolate config reads from the developer's real ~/.agent-factory:
	// instance.Start -> NewGitWorktree -> LoadConfig would otherwise pick up
	// the host's config and trip the strict default_program enum check
	// introduced in #658.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Set up a temp working directory
	workdir := t.TempDir()

	// Initialize git repository
	setupGitRepo(t, workdir)

	// Create unique session name
	random := time.Now().UnixNano() % 10000000
	sessionName := fmt.Sprintf("test-preview-%s-%d-%d", t.Name(), time.Now().UnixNano(), random)

	// Clean up any existing tmux session
	cleanupCmd := exec.Command("tmux", "kill-session", "-t", "af_"+sessionName)
	_ = cleanupCmd.Run() // Ignore errors if session doesn't exist

	// Create instance
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   sessionName,
		Path:    workdir,
		Program: "bash",
		AutoYes: false,
	})
	require.NoError(t, err)

	// Create MockPtyFactory
	ptyFactory := &MockPtyFactory{
		t:       t,
		cmdExec: cmdExec,
	}

	// Set up tmux session with mocks
	tmuxSession := tmux.NewTmuxSessionWithDeps(sessionName, "bash", ptyFactory, cmdExec)
	instance.SetTmuxSession(tmuxSession)

	// Start the tmux session
	err = instance.Start(true)
	require.NoError(t, err)

	// Create cleanup function
	cleanupFn := func() {
		if instance != nil {
			_ = instance.Kill() // Ignore errors during cleanup
		}
		log.Close()
	}

	return &testSetup{
		workdir:     workdir,
		instance:    instance,
		sessionName: sessionName,
		cleanupFn:   cleanupFn,
	}
}

// setupGitRepo initializes a git repository in the given directory
func setupGitRepo(t *testing.T, workdir string) {
	t.Helper()

	// Initialize git repository
	initCmd := exec.Command("git", "init")
	initCmd.Dir = workdir
	err := initCmd.Run()
	require.NoError(t, err)

	// Create basic git config (local to this repo only)
	configCmd := exec.Command("git", "config", "--local", "user.email", "test@example.com")
	configCmd.Dir = workdir
	err = configCmd.Run()
	require.NoError(t, err)

	configCmd = exec.Command("git", "config", "--local", "user.name", "Test User")
	configCmd.Dir = workdir
	err = configCmd.Run()
	require.NoError(t, err)

	// Create and commit a test file
	testFile := filepath.Join(workdir, "test.txt")
	err = os.WriteFile(testFile, []byte("test content"), 0644)
	require.NoError(t, err)

	addCmd := exec.Command("git", "add", "test.txt")
	addCmd.Dir = workdir
	err = addCmd.Run()
	require.NoError(t, err)

	commitCmd := exec.Command("git", "commit", "-m", "initial commit")
	commitCmd.Dir = workdir
	err = commitCmd.Run()
	require.NoError(t, err)
}

// TestPreviewScrolling tests the scrolling functionality in the preview pane
func TestPreviewScrolling(t *testing.T) {
	// Track what commands were executed and their order
	var executedCommands []string
	inCopyMode := false
	scrollPosition := 0 // 0 = bottom, positive = scrolled up
	sessionCreated := false

	// Create test content with line numbers for scrolling
	const numLines = 100
	lines := make([]string, numLines+1)
	lines[0] = "$ seq 100" // Command that was run
	for i := 1; i <= numLines; i++ {
		lines[i] = fmt.Sprintf("%d", i)
	}
	fullContent := strings.Join(lines, "\n")

	// Mock command execution
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()
			executedCommands = append(executedCommands, cmdStr)

			// Handle tmux session creation and existence checking
			if strings.Contains(cmdStr, "has-session") {
				if sessionCreated {
					return nil // Session exists
				} else {
					return fmt.Errorf("session does not exist")
				}
			}

			// Handle session creation
			if strings.Contains(cmdStr, "new-session") {
				sessionCreated = true
				return nil
			}

			// Handle attach-session
			if strings.Contains(cmdStr, "attach-session") {
				return nil
			}

			// Handle copy mode commands
			if strings.Contains(cmdStr, "copy-mode") {
				inCopyMode = true
			}
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "q") {
				inCopyMode = false
				scrollPosition = 0 // Reset position when exiting copy mode
			}
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "Up") {
				if inCopyMode {
					scrollPosition++
				}
			}
			if strings.Contains(cmdStr, "send-keys") && strings.Contains(cmdStr, "Down") {
				if inCopyMode && scrollPosition > 0 {
					scrollPosition--
				}
			}

			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()

			// Handle capture-pane commands
			if strings.Contains(cmdStr, "capture-pane") {
				// Check if this is a request for cursor position
				if strings.Contains(cmdStr, "display-message") && strings.Contains(cmdStr, "copy_cursor_y") {
					var buf []byte
					buf = fmt.Appendf(buf, "%d", scrollPosition)
					return buf, nil
				}

				// Check if this is a copy mode capture with full history (-S -)
				if strings.Contains(cmdStr, "-S -") {
					// Always return the full content for PreviewFullHistory
					return []byte(fullContent), nil
				}

				// Regular capture for normal preview mode - show the last 20 lines
				const visibleLines = 20
				startLine := max(0, numLines+1-visibleLines)
				visibleContent := strings.Join(lines[startLine:], "\n")
				return []byte(visibleContent), nil
			}

			return []byte(""), nil
		},
	}

	// Setup test environment
	setup := setupTestEnvironment(t, cmdExec)
	defer setup.cleanupFn()

	// The pane's output is driven by the mock capture-pane (OutputFunc) above —
	// raw key injection (the old SendKeys) was deleted in #1592 Phase 2 PR7, and
	// the preview never depended on it here (content comes from the mock capture).

	// Create the preview pane
	previewPane := NewTabPane(previewFromInstance)
	previewPane.SetSize(80, 30) // Set reasonable size for testing

	// Step 1: Check initial content - should show normal preview mode
	err := previewPane.UpdateContent(setup.instance, 0)
	require.NoError(t, err)

	// Verify we're not in scrolling mode initially
	require.False(t, previewPane.scroll.Active(), "Should not be in scrolling mode initially")

	// Step 2: Check that PreviewFullHistory returns all content
	fullHistory, err := setup.instance.AgentServer().Preview(0, true)
	require.NoError(t, err)

	// Verify that the full history contains both the command and early output
	require.Contains(t, fullHistory.Content, "$ seq 100", "Full history should contain the command")
	require.Contains(t, fullHistory.Content, "1", "Full history should contain earliest output")

	// Step 3: Enter scroll mode
	err = previewPane.ScrollUp(setup.instance, 0)
	require.NoError(t, err)

	// Verify we entered scrolling mode
	require.True(t, previewPane.scroll.Active(), "Should be in scrolling mode after ScrollUp")

	// Step 4: Get the content directly from the viewport
	viewportContent := previewPane.viewport.View()
	t.Logf("Viewport content: %q", viewportContent)

	// With proper implementation, the viewport should have the full history content
	// Note: The viewport will be positioned at the bottom initially, so we need to scroll up

	// Step 5: Scroll up multiple times to get to the top
	for range 50 {
		err = previewPane.ScrollUp(setup.instance, 0)
		require.NoError(t, err)
	}

	// Now get the viewport content after scrolling up
	viewportAfterScrollUp := previewPane.viewport.View()
	t.Logf("Viewport after scrolling up: %q", viewportAfterScrollUp)

	// Step 6: Scroll down multiple times
	for range 25 {
		err = previewPane.ScrollDown(setup.instance, 0)
		require.NoError(t, err)
	}

	// Get updated viewport content after scrolling down
	viewportAfterScrollDown := previewPane.viewport.View()
	t.Logf("Viewport after scrolling down: %q", viewportAfterScrollDown)

	// Step 7: Reset to normal mode
	err = previewPane.ResetToNormalMode(setup.instance, 0)
	require.NoError(t, err)

	// Verify we exited scrolling mode
	require.False(t, previewPane.scroll.Active(), "Should not be in scrolling mode after reset")
}

// MockPtyFactory for testing tmux sessions
type MockPtyFactory struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec

	// Array of commands and the corresponding file handles representing PTYs.
	cmds  []*exec.Cmd
	files []*os.File
}

func (pt *MockPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	filePath := filepath.Join(pt.t.TempDir(), fmt.Sprintf("pty-%s-%d", pt.t.Name(), len(pt.cmds)))
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		pt.cmds = append(pt.cmds, cmd)
		pt.files = append(pt.files, f)

		// Execute the command through our mock to trigger session creation logic
		_ = pt.cmdExec.Run(cmd)
	}
	return f, err
}

func (pt *MockPtyFactory) Close() {}

// TestPreviewContentWithoutScrolling tests that the preview pane correctly displays content
// for a new instance without requiring scrolling
func TestPreviewContentWithoutScrolling(t *testing.T) {
	// Create test content
	expectedContent := "$ echo test\ntest"

	// Track session creation state
	sessionCreated := false

	// Mock command execution
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()

			// Handle tmux session creation and existence checking
			if strings.Contains(cmdStr, "has-session") {
				if sessionCreated {
					return nil // Session exists
				} else {
					return fmt.Errorf("session does not exist")
				}
			}

			// Handle session creation
			if strings.Contains(cmdStr, "new-session") {
				sessionCreated = true
				return nil
			}

			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()

			// Handle capture-pane commands for normal preview
			if strings.Contains(cmdStr, "capture-pane") {
				// Return our test content for normal preview
				return []byte(expectedContent), nil
			}

			return []byte(""), nil
		},
	}

	// Setup test environment
	setup := setupTestEnvironment(t, cmdExec)
	defer setup.cleanupFn()

	// Create the preview pane
	previewPane := NewTabPane(previewFromInstance)
	previewPane.SetSize(80, 30) // Set reasonable size for testing

	// Update the preview content (this should display the content without scrolling)
	err := previewPane.UpdateContent(setup.instance, 0)
	require.NoError(t, err)

	// Verify we're not in scrolling mode
	require.False(t, previewPane.scroll.Active(), "Should not be in scrolling mode")

	// Verify that the preview state is not in fallback mode
	require.False(t, previewPane.content.fallback, "Preview should not be in fallback mode")

	// Verify that the preview state contains the expected content
	require.Equal(t, expectedContent, previewPane.content.text, "Preview state should contain the expected content")

	// Verify the rendered string contains the content
	renderedString := previewPane.String()
	require.Contains(t, renderedString, "test", "Rendered preview should contain the test content")
}

func TestPreviewExactFitDoesNotTruncate(t *testing.T) {
	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 3)
	p.content = tabContentState{
		fallback: false,
		text:     "one\ntwo\nthree",
	}

	rendered := p.String()
	require.Contains(t, rendered, "three")
	require.NotContains(t, rendered, "...")
}

// TestPreviewBottomTruncateShowsNewestLines is the regression test for #649:
// when content exceeds the pane height, PreviewPane must keep the BOTTOM
// p.height lines (newest output) — matching TerminalPane — rather than the
// top p.height-1 lines plus a "..." marker. Showing the start of the output
// while the agent has moved on is confusing UX.
func TestPreviewBottomTruncateShowsNewestLines(t *testing.T) {
	const totalLines = 100
	const paneHeight = 10

	lines := make([]string, totalLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%03d", i+1)
	}
	text := strings.Join(lines, "\n")

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, paneHeight)
	p.content = tabContentState{fallback: false, text: text}

	rendered := p.String()

	// The newest p.height lines must be present; the oldest lines must not.
	for i := totalLines - paneHeight; i < totalLines; i++ {
		require.Contains(t, rendered, lines[i],
			"newest line %q must be visible after truncation", lines[i])
	}
	for i := 0; i < totalLines-paneHeight; i++ {
		require.NotContains(t, rendered, lines[i],
			"older line %q must NOT be visible after truncation", lines[i])
	}

	// Match TerminalPane: no "..." indicator.
	require.NotContains(t, rendered, "...",
		"bottom-truncation should not append a '...' marker")
}

// TestPreviewTrailingNewlineDoesNotTriggerTruncation guards the off-by-one
// fix in #649: tmux capture-pane output frequently ends in "\n", which made
// strings.Split produce len(lines) == p.height+1 even when the visible
// content fits the pane. Before the fix, this took the truncation branch
// and dropped a line.
func TestPreviewTrailingNewlineDoesNotTriggerTruncation(t *testing.T) {
	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 10)
	// Five lines of content + trailing newline. Total visible content is 5
	// lines, well below the 10-line budget; the trailing "\n" must not cause
	// the renderer to truncate.
	p.content = tabContentState{
		fallback: false,
		text:     "one\ntwo\nthree\nfour\nfive\n",
	}

	rendered := p.String()
	require.Contains(t, rendered, "one",
		"first line must remain visible — trailing newline must not trigger truncation")
	require.Contains(t, rendered, "five",
		"last content line must remain visible")
	require.NotContains(t, rendered, "...",
		"no truncation marker expected when content fits")
}

// TestPreviewTrailingNewlineAtExactHeight checks the boundary: content that
// fills exactly p.height lines but ends in "\n" must not be truncated. The
// trailing-empty strip exists to handle this case.
func TestPreviewTrailingNewlineAtExactHeight(t *testing.T) {
	const paneHeight = 5
	contentLines := []string{"a", "b", "c", "d", "e"}
	text := strings.Join(contentLines, "\n") + "\n"

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, paneHeight)
	p.content = tabContentState{fallback: false, text: text}

	rendered := p.String()
	for _, line := range contentLines {
		require.Contains(t, rendered, line,
			"line %q must remain visible at exact-fit boundary", line)
	}
	require.NotContains(t, rendered, "...",
		"no truncation marker expected at exact-fit boundary")
}

// Helper function for max
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestPreviewResetToNormalModeNilInstance is a regression test for issue #338.
// When ResetToNormalMode is called with a nil instance (e.g., the user pressed
// ESC while the sidebar header was selected), the pane previously returned
// early without clearing isScrolling, leaving it stuck on stale viewport
// content. After the fix, the scroll state must be cleared regardless of
// whether an instance is provided.
func TestPreviewResetToNormalModeNilInstance(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enableHostHistory(p, nil, 0)

	// Simulate being in scroll mode with stale viewport content.
	const staleContent = "stale-scroll-content-line\nanother-line"
	p.scroll.Scroll(&p.viewport, scrollOneLineUp)
	p.viewport.SetContent(staleContent)

	require.True(t, p.scroll.Active(), "precondition: should be in scrolling mode")
	require.Contains(t, p.viewport.View(), "stale-scroll-content-line",
		"precondition: viewport should hold stale content")

	// Calling ResetToNormalMode with nil must still clear scroll state.
	err := p.ResetToNormalMode(nil, 0)
	require.NoError(t, err)

	require.False(t, p.scroll.Active(),
		"isScrolling must be false after ResetToNormalMode(nil)")

	// The rendered output must no longer be the stale scroll-mode viewport.
	rendered := p.String()
	require.NotContains(t, rendered, "stale-scroll-content-line",
		"rendered content must not be the stale scroll-mode viewport after reset")
}

// TestPreviewResetToNormalModeLoadingShowsFallback is a regression test for
// issue #823. Exiting scroll mode on a Loading instance wrote
// {fallback:false, text:""} into previewState — Preview() returns empty
// content while the workspace is still being set up — so the pane rendered
// completely blank for at least one frame until the next async UpdateContent
// tick. ResetToNormalMode must keep the same "Setting up workspace…"
// fallback that UpdateContent shows for Loading instances.
func TestPreviewResetToNormalModeLoadingShowsFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "loading", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Loading)

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	require.NoError(t, p.UpdateContent(inst, 0))
	require.True(t, p.content.fallback,
		"precondition: Loading instance shows the fallback in normal mode")

	enableHostHistory(p, inst, 0)
	require.NoError(t, p.ScrollUp(inst, 0))
	require.True(t, p.scroll.Active(), "precondition: ScrollUp enters scroll mode")

	require.NoError(t, p.ResetToNormalMode(inst, 0))
	require.False(t, p.scroll.Active(),
		"isScrolling must be false after ResetToNormalMode")
	require.True(t, p.content.fallback,
		"exiting scroll mode on a Loading instance must keep the fallback state")
	require.Contains(t, p.content.text, "Setting up workspace…",
		"fallback text must be the Loading message")
	require.Contains(t, p.String(), "Setting up workspace…",
		"rendered frame must show the Loading fallback, not a blank pane")
}

// TestPreviewUpdateContentDeletingShowsTeardownFallback is the regression test
// for issue #920 (regression of #847). The UpdateContent switch handled the
// Loading transient status but not Deleting, so a Deleting instance — whose
// backend Kill() drops the PTY before the (possibly slow) delete_cmd, leaving
// Preview() == ("", nil) and Started() == false — fell through to the generic
// "Please enter a name for the instance." fallback. That message is misleading
// during teardown and, for remote sessions, can persist for the whole 10-60s
// delete. UpdateContent must show the "Tearing down session…" fallback for a
// Deleting instance instead.
func TestPreviewUpdateContentDeletingShowsTeardownFallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "deleting", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Deleting)

	// Precondition: this instance has empty preview content and is not started,
	// exactly the state that previously produced the wrong "Please enter a
	// name" fallback.
	require.False(t, inst.Started(),
		"precondition: a Deleting instance reports Started()==false")

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	require.NoError(t, p.UpdateContent(inst, 0))

	require.True(t, p.content.fallback,
		"Deleting instance must render a fallback")
	require.Contains(t, p.content.text, "Tearing down session…",
		"Deleting instance must show the teardown fallback")
	require.NotContains(t, p.content.text, "Please enter a name",
		"Deleting instance must NOT show the not-started fallback (#920)")
	require.Contains(t, p.String(), "Tearing down session…",
		"rendered frame must show the teardown fallback")
}

// TestPreviewFallbackHeightNoDoubleCounting ensures that the fallback welcome
// screen does not over-subtract lines for borders/margins that
// TabbedWindow.SetSize has already stripped. Previously it subtracted 7, which
// produced a 6-line deficit versus normal mode and truncated the ASCII art on
// short terminals. See issue #274.
func TestPreviewFallbackHeightNoDoubleCounting(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// Count the number of lines in the fallback text itself so we can make
	// assertions that depend on the ASCII art surviving the layout math.
	fallbackTextLines := len(strings.Split(paneFallbackContent("msg", 80), "\n"))

	t.Run("renders full content at comfortable height", func(t *testing.T) {
		p := NewTabPane(previewFromInstance)
		p.SetSize(80, 30)
		p.setFallbackState("hello world")

		rendered := p.String()
		require.NotEmpty(t, rendered, "fallback render should not be empty")
		require.Contains(t, rendered, "hello world",
			"fallback render should contain the provided message")
		// A sentinel glyph from the ASCII art - its presence means the art
		// wasn't truncated away.
		require.Contains(t, rendered, "█████",
			"fallback render should contain the ASCII art")
	})

	t.Run("matches normal-mode height budget", func(t *testing.T) {
		// At any height, the fallback should compute the same availableHeight
		// as normal mode (p.height), so the rendered output fills the same
		// number of lines.
		for _, h := range []int{20, 25, 30, 50} {
			p := NewTabPane(previewFromInstance)
			p.SetSize(80, h)
			p.setFallbackState("msg")

			rendered := p.String()
			lines := strings.Split(rendered, "\n")

			// We expect at least max(fallbackTextLines, h) lines; the pane
			// pads to fill the available area when content is shorter than
			// the budget.
			expected := h
			if fallbackTextLines > expected {
				expected = fallbackTextLines
			}
			require.GreaterOrEqual(t, len(lines), expected,
				"height=%d: fallback must fill p.height (no double-counting of chrome)",
				h)
			require.Contains(t, rendered, "msg",
				"height=%d: fallback message must remain visible", h)
		}
	})
}

func TestPreviewFallbackOmitsLogoWhenItCannotFit(t *testing.T) {
	for _, tc := range []struct{ width, height int }{
		{56, 20}, // content box in the reported 80x24 layout
		{48, 15}, // compact 80x24 layout variants
		{36, 16}, // content box in a narrower terminal
	} {
		p := NewTabPane(previewFromInstance)
		p.SetSize(tc.width, tc.height)
		p.setFallbackState("Setting up workspace…")

		rendered := p.String()
		require.Contains(t, rendered, "Setting up workspace…")
		require.NotContains(t, rendered, "████",
			"%d-cell panes must omit, not hard-wrap, the fallback logo", tc.width)
		require.Equal(t, tc.height, len(strings.Split(rendered, "\n")))
	}

	p := NewTabPane(previewFromInstance)
	p.SetSize(lipgloss.Width(FallBackText), 30)
	p.setFallbackState("Setting up workspace…")
	require.Contains(t, p.String(), "████",
		"the full fallback logo remains at widths where it fits")
}

// TestPreviewFallbackMatchesNormalModeHeight is the regression test for #616.
// Before the fix, fallback rendering computed availableHeight as p.height - 1
// while normal mode (post-#405) padded to the full p.height, so fallback views
// rendered one line shorter than normal views and left a trailing blank line.
// Both modes must now render the same number of lines for the same p.height.
func TestPreviewFallbackMatchesNormalModeHeight(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// Heights large enough that the height budget — not the fallback ASCII
	// art — bounds the rendered output. At smaller heights the ASCII art
	// overflows the budget and the two modes are not comparable.
	for _, h := range []int{20, 25, 30, 50} {
		// Normal mode: short content padded to full height.
		normal := NewTabPane(previewFromInstance)
		normal.SetSize(80, h)
		normal.content = tabContentState{
			fallback: false,
			text:     "line1\nline2",
		}
		normalLines := len(strings.Split(normal.String(), "\n"))

		// Fallback mode at the same height with a short message.
		fb := NewTabPane(previewFromInstance)
		fb.SetSize(80, h)
		fb.setFallbackState("msg")
		fbLines := len(strings.Split(fb.String(), "\n"))

		require.Equal(t, normalLines, fbLines,
			"height=%d: fallback and normal mode must render the same number of lines",
			h)
	}
}

// setupTwoInstances creates two independent instances in the same test so we
// can exercise the instance-switch path in PreviewPane.UpdateContent. The mock
// tracks tmux sessions in a map keyed by name so each instance has its own
// has-session/new-session bookkeeping.
func setupTwoInstances(t *testing.T, previewA, previewB string) (*session.Instance, *session.Instance, func()) {
	t.Helper()
	log.Initialize(false)

	// Isolate config reads — see setupTestEnvironment for details (#658).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	workdir := t.TempDir()
	setupGitRepo(t, workdir)

	existing := map[string]bool{}
	sessionFor := func(cmd *exec.Cmd) string {
		// tmux targets can be passed as "-t <name>" / "-s <name>" (two args),
		// "-t=<name>" / "-s=<name>" (one arg), or the exact-match
		// "-t =<name>:" / "-s =<name>:" form (#1006). Handle all of them.
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()
			name := sessionFor(cmd)
			switch {
			case strings.Contains(cmdStr, "has-session"):
				if existing[name] {
					return nil
				}
				return fmt.Errorf("session does not exist")
			case strings.Contains(cmdStr, "new-session"):
				existing[name] = true
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()
			if !strings.Contains(cmdStr, "capture-pane") {
				return []byte(""), nil
			}
			name := sessionFor(cmd)
			// Distinguish per-instance output so tests can assert which
			// instance's content was rendered.
			if strings.HasSuffix(name, "-inst_b") {
				return []byte(previewB), nil
			}
			return []byte(previewA), nil
		},
	}

	mkInstance := func(suffix string) *session.Instance {
		title := fmt.Sprintf("test-switch-%s-%d-%s", t.Name(), time.Now().UnixNano(), suffix)
		// Clean any pre-existing tmux session by the same name.
		_ = exec.Command("tmux", "kill-session", "-t", "af_"+title).Run()

		inst, err := session.NewInstance(session.InstanceOptions{
			Title:   title,
			Path:    workdir,
			Program: "bash",
			AutoYes: false,
		})
		require.NoError(t, err)

		pty := &MockPtyFactory{t: t, cmdExec: cmdExec}
		inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps(title, "bash", pty, cmdExec))
		require.NoError(t, inst.Start(true))
		return inst
	}

	a := mkInstance("inst_a")
	b := mkInstance("inst_b")
	cleanup := func() {
		_ = a.Kill()
		_ = b.Kill()
		log.Close()
	}
	return a, b, cleanup
}

// TestPreviewUpdateContentSessionGoneRendersFallback is the #496 regression:
// when the underlying tmux session vanishes between ticks, UpdateContent
// must enter the inactive-session fallback rather than propagate an error
// that the app-level handleError would log at ERROR every 100ms.
func TestPreviewUpdateContentSessionGoneRendersFallback(t *testing.T) {
	var sessionCreated, sessionGone atomic.Bool

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "has-session") {
				if sessionGone.Load() {
					return fmt.Errorf("session gone")
				}
				if sessionCreated.Load() {
					return nil
				}
				return fmt.Errorf("session does not exist")
			}
			if strings.Contains(cmdStr, "new-session") {
				sessionCreated.Store(true)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "capture-pane") {
				if sessionGone.Load() {
					return nil, fmt.Errorf("exit status 1")
				}
				return []byte("hello world"), nil
			}
			return []byte(""), nil
		},
	}

	setup := setupTestEnvironment(t, cmdExec)
	defer setup.cleanupFn()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)

	// Happy path first: session alive, normal content renders.
	require.NoError(t, p.UpdateContent(setup.instance, 0))
	require.False(t, p.content.fallback,
		"happy path must not render fallback")
	require.Contains(t, p.content.text, "hello world")

	// Session vanishes externally; redirect ErrorLog so we can prove
	// UpdateContent does not log anything at ERROR.
	var errBuf bytes.Buffer
	prev := log.ErrorLog.Writer()
	log.ErrorLog.SetOutput(&errBuf)
	defer log.ErrorLog.SetOutput(prev)

	sessionGone.Store(true)

	err := p.UpdateContent(setup.instance, 0)
	require.NoError(t, err,
		"dead session must NOT bubble an error up to handleError")
	require.True(t, p.content.fallback,
		"preview must enter fallback state when session is gone")
	require.Contains(t, p.content.text, "Session no longer running")
	require.Empty(t, errBuf.String(),
		"no ERROR log line on session-gone fallback path")
}

// TestResetToNormalModeDoesNotClearFallbackFlag is the #577 regression, updated
// for the off-loop-scroll refactor (#1637). ResetToNormalMode no longer captures
// preview content on the event loop; it clears scroll state and leaves p.content
// untouched (a live restore rides the immediate off-loop refresh the app
// dispatches after ESC). The #577 mismatch — fallback==true holding real terminal
// text — is therefore structurally impossible on the exit path: ResetToNormalMode
// never writes text, so it can never desync the flag from the text. This test
// pins both halves: exit leaves the content flag/text consistent, and the
// following refresh restores real content with fallback cleared.
func TestResetToNormalModeDoesNotClearFallbackFlag(t *testing.T) {
	const expectedContent = "$ terminal output"

	sessionCreated := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "has-session") {
				if sessionCreated {
					return nil
				}
				return fmt.Errorf("session does not exist")
			}
			if strings.Contains(cmdStr, "new-session") {
				sessionCreated = true
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			cmdStr := cmd.String()
			if strings.Contains(cmdStr, "capture-pane") {
				return []byte(expectedContent), nil
			}
			return []byte(""), nil
		},
	}

	setup := setupTestEnvironment(t, cmdExec)
	defer setup.cleanupFn()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)

	// Simulate the precondition: pane is in a fallback state (e.g. Loading)
	// and the user has entered scroll mode while it was still showing the
	// fallback. setFallbackState is the production path that gets us into
	// fallback==true; isScrolling/viewport mimic ScrollUp's side effects
	// without depending on tmux capture-pane behavior here.
	p.setFallbackState("Setting up workspace…")
	require.True(t, p.content.fallback,
		"precondition: fallback should be true after setFallbackState")
	p.scroll.Scroll(&p.viewport, scrollOneLineUp)
	p.viewport.SetContent("ESC to exit scroll mode")

	// Exit scroll mode. ResetToNormalMode clears scroll state without capturing
	// (#1637), so it must NOT leave a fallback==true / real-text mismatch — the
	// #577 bug. A live instance matches none of its synchronous fallback cases, so
	// p.content is left exactly as it was (the fallback), consistently.
	require.NoError(t, p.ResetToNormalMode(setup.instance, 0))
	require.False(t, p.scroll.Active(), "should exit scroll mode")
	require.False(t, p.content.fallback && p.content.text == expectedContent,
		"#577: ResetToNormalMode must never leave fallback==true holding real terminal text")

	// The immediate off-loop refresh the app dispatches after ESC restores real
	// content and clears the fallback flag together.
	require.NoError(t, p.UpdateContent(setup.instance, 0))
	require.Equal(t, expectedContent, p.content.text,
		"the post-exit refresh sets text to the captured preview content")
	require.False(t, p.content.fallback,
		"the post-exit refresh clears the fallback flag alongside real content (#577)")

	// And the rendered output must use the normal (left-aligned) branch,
	// not the centered fallback layout — i.e. it must not be padded with
	// the giant top-of-pane vertical whitespace that fallback rendering
	// injects to vertically center its message.
	rendered := p.String()
	require.Contains(t, rendered, expectedContent,
		"normal-mode render must contain the captured content")
	require.False(t, strings.HasPrefix(rendered, "\n\n\n\n"),
		"normal-mode render must not be vertically centered like fallback")
}

// TestPreviewSwitchInstanceResetsScroll is a regression test for issue #470:
// when the user is in scroll mode on instance A and switches to instance B,
// the preview pane must drop the scroll-mode viewport (which still holds A's
// captured history) and render B's current content. Re-rendering the SAME
// instance while scrolling must preserve scroll state.
func TestPreviewSwitchInstanceResetsScroll(t *testing.T) {
	const previewA = "instance-A-content"
	const previewB = "instance-B-content"

	instA, instB, cleanup := setupTwoInstances(t, previewA, previewB)
	defer cleanup()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)

	// Render A normally, then enter scroll mode on A.
	require.NoError(t, p.UpdateContent(instA, 0))
	require.NoError(t, p.ScrollUp(instA, 0))
	require.True(t, p.scroll.Active(), "should be scrolling after ScrollUp on A")

	// Re-render the SAME instance while scrolling: scroll state must
	// survive (this is the original behavior the #470 fix must not break).
	require.NoError(t, p.UpdateContent(instA, 0))
	require.True(t, p.scroll.Active(),
		"re-rendering the same instance must preserve scroll mode")

	// Now switch to B. Scroll state must be cleared and B's content rendered.
	require.NoError(t, p.UpdateContent(instB, 0))
	require.False(t, p.scroll.Active(),
		"switching to a different instance must exit scroll mode")
	require.False(t, p.content.fallback,
		"B is a real running instance; preview must not be in fallback")
	require.Equal(t, previewB, p.content.text,
		"preview must reflect the newly selected instance's content")
	require.NotContains(t, p.viewport.View(), previewA,
		"stale viewport content from A must be cleared")
}

// TestScrollMouseDifferentInstanceResetsScrollMode is a regression test for
// issue #702: the mouse-wheel scroll path (ScrollUp/ScrollDown) runs straight
// off the bubbletea event loop and can fire before the async UpdateContent for
// a newly selected instance has run. When the user is scrolling instance A and
// the selection switches to B, a wheel scroll must NOT scroll A's stale
// viewport — it must reset scroll mode and capture B's content, mirroring the
// guard UpdateContent already had.
func TestScrollMouseDifferentInstanceResetsScrollMode(t *testing.T) {
	const previewA = "instance-A-content"
	const previewB = "instance-B-content"

	instA, instB, cleanup := setupTwoInstances(t, previewA, previewB)
	defer cleanup()

	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	enableHostHistory(p, instA, 0)

	// Enter scroll mode on A via the mouse path (no prior UpdateContent). Scroll
	// entry is I/O-free (#1637); the off-loop refresh (UpdateContent) fills the
	// viewport from A — the two-step flow production drives after a wheel scroll.
	require.NoError(t, p.ScrollUp(instA, 0))
	require.True(t, p.scroll.Active(), "should be scrolling A after ScrollUp(A)")
	require.NoError(t, p.UpdateContent(instA, 0))
	require.Contains(t, p.viewport.View(), previewA,
		"precondition: viewport should hold A's captured content")

	// Selection switches to B, but the refresh for B has not run yet. A wheel-up
	// arrives for B. Before the fix this scrolled A's stale viewport; now ScrollUp
	// drops scroll state and re-keys to B (dropStaleView clears the viewport at
	// once), and the off-loop fill captures B's content — never stale A.
	require.NoError(t, p.ScrollUp(instB, 0))
	require.True(t, p.scroll.Active(),
		"should re-enter scroll mode for B after the switch")
	require.NotContains(t, p.viewport.View(), previewA,
		"stale viewport content from A must be cleared on the scroll path at once")
	require.NoError(t, p.UpdateContent(instB, 0))
	require.Contains(t, p.viewport.View(), previewB,
		"viewport must reflect B after the mouse scroll, not stale A")
	require.NotContains(t, p.viewport.View(), previewA,
		"the fill must capture B, never the previously rendered A (#746)")

	// The same must hold for the ScrollDown entry point.
	require.NoError(t, p.ScrollDown(instA, 0))
	require.NotContains(t, p.viewport.View(), previewB,
		"stale viewport content from B must be cleared on ScrollDown path at once")
	require.NoError(t, p.UpdateContent(instA, 0))
	require.Contains(t, p.viewport.View(), previewA,
		"ScrollDown on a switched-to instance must re-capture its content")
	require.NotContains(t, p.viewport.View(), previewB,
		"stale viewport content from B must be cleared on ScrollDown path")
}
