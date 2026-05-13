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

	// Simulate running a command that produces lots of output
	err := setup.instance.SendKeys("seq 100")
	require.NoError(t, err)
	err = setup.instance.SendKeys("") // Simulate pressing Enter
	require.NoError(t, err)

	// Create the preview pane
	previewPane := NewPreviewPane()
	previewPane.SetSize(80, 30) // Set reasonable size for testing

	// Step 1: Check initial content - should show normal preview mode
	err = previewPane.UpdateContent(setup.instance)
	require.NoError(t, err)

	// Verify we're not in scrolling mode initially
	require.False(t, previewPane.isScrolling, "Should not be in scrolling mode initially")

	// Step 2: Check that PreviewFullHistory returns all content
	fullHistory, err := setup.instance.PreviewFullHistory()
	require.NoError(t, err)

	// Verify that the full history contains both the command and early output
	require.Contains(t, fullHistory, "$ seq 100", "Full history should contain the command")
	require.Contains(t, fullHistory, "1", "Full history should contain earliest output")

	// Step 3: Enter scroll mode
	err = previewPane.ScrollUp(setup.instance)
	require.NoError(t, err)

	// Verify we entered scrolling mode
	require.True(t, previewPane.isScrolling, "Should be in scrolling mode after ScrollUp")

	// Step 4: Get the content directly from the viewport
	viewportContent := previewPane.viewport.View()
	t.Logf("Viewport content: %q", viewportContent)

	// With proper implementation, the viewport should have the full history content
	// Note: The viewport will be positioned at the bottom initially, so we need to scroll up

	// Step 5: Scroll up multiple times to get to the top
	for range 50 {
		err = previewPane.ScrollUp(setup.instance)
		require.NoError(t, err)
	}

	// Now get the viewport content after scrolling up
	viewportAfterScrollUp := previewPane.viewport.View()
	t.Logf("Viewport after scrolling up: %q", viewportAfterScrollUp)

	// Step 6: Scroll down multiple times
	for range 25 {
		err = previewPane.ScrollDown(setup.instance)
		require.NoError(t, err)
	}

	// Get updated viewport content after scrolling down
	viewportAfterScrollDown := previewPane.viewport.View()
	t.Logf("Viewport after scrolling down: %q", viewportAfterScrollDown)

	// Step 7: Reset to normal mode
	err = previewPane.ResetToNormalMode(setup.instance)
	require.NoError(t, err)

	// Verify we exited scrolling mode
	require.False(t, previewPane.isScrolling, "Should not be in scrolling mode after reset")
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
	previewPane := NewPreviewPane()
	previewPane.SetSize(80, 30) // Set reasonable size for testing

	// Update the preview content (this should display the content without scrolling)
	err := previewPane.UpdateContent(setup.instance)
	require.NoError(t, err)

	// Verify we're not in scrolling mode
	require.False(t, previewPane.isScrolling, "Should not be in scrolling mode")

	// Verify that the preview state is not in fallback mode
	require.False(t, previewPane.previewState.fallback, "Preview should not be in fallback mode")

	// Verify that the preview state contains the expected content
	require.Equal(t, expectedContent, previewPane.previewState.text, "Preview state should contain the expected content")

	// Verify the rendered string contains the content
	renderedString := previewPane.String()
	require.Contains(t, renderedString, "test", "Rendered preview should contain the test content")
}

func TestPreviewExactFitDoesNotTruncate(t *testing.T) {
	p := NewPreviewPane()
	p.SetSize(80, 3)
	p.previewState = previewState{
		fallback: false,
		text:     "one\ntwo\nthree",
	}

	rendered := p.String()
	require.Contains(t, rendered, "three")
	require.NotContains(t, rendered, "...")
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

	p := NewPreviewPane()
	p.SetSize(80, 30)

	// Simulate being in scroll mode with stale viewport content.
	const staleContent = "stale-scroll-content-line\nanother-line"
	p.isScrolling = true
	p.viewport.SetContent(staleContent)

	require.True(t, p.isScrolling, "precondition: should be in scrolling mode")
	require.Contains(t, p.viewport.View(), "stale-scroll-content-line",
		"precondition: viewport should hold stale content")

	// Calling ResetToNormalMode with nil must still clear scroll state.
	err := p.ResetToNormalMode(nil)
	require.NoError(t, err)

	require.False(t, p.isScrolling,
		"isScrolling must be false after ResetToNormalMode(nil)")

	// The rendered output must no longer be the stale scroll-mode viewport.
	rendered := p.String()
	require.NotContains(t, rendered, "stale-scroll-content-line",
		"rendered content must not be the stale scroll-mode viewport after reset")
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
	fallbackTextLines := len(strings.Split(
		lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", "msg"),
		"\n",
	))

	t.Run("renders full content at comfortable height", func(t *testing.T) {
		p := NewPreviewPane()
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
		// as normal mode (p.height - 1), so the rendered output fills the
		// same number of lines.
		for _, h := range []int{20, 25, 30, 50} {
			p := NewPreviewPane()
			p.SetSize(80, h)
			p.setFallbackState("msg")

			rendered := p.String()
			lines := strings.Split(rendered, "\n")

			// We expect at least max(fallbackTextLines, h-1) lines; the pane
			// pads to fill the available area when content is shorter than
			// the budget.
			expected := h - 1
			if fallbackTextLines > expected {
				expected = fallbackTextLines
			}
			require.GreaterOrEqual(t, len(lines), expected,
				"height=%d: fallback must fill p.height-1 (no double-counting of chrome)",
				h)
			require.Contains(t, rendered, "msg",
				"height=%d: fallback message must remain visible", h)
		}
	})
}

// setupTwoInstances creates two independent instances in the same test so we
// can exercise the instance-switch path in PreviewPane.UpdateContent. The mock
// tracks tmux sessions in a map keyed by name so each instance has its own
// has-session/new-session bookkeeping.
func setupTwoInstances(t *testing.T, previewA, previewB string) (*session.Instance, *session.Instance, func()) {
	t.Helper()
	log.Initialize(false)

	workdir := t.TempDir()
	setupGitRepo(t, workdir)

	existing := map[string]bool{}
	sessionFor := func(cmd *exec.Cmd) string {
		// tmux targets can be passed as either "-t <name>" / "-s <name>"
		// (two args) or "-t=<name>" / "-s=<name>" (one arg). Handle both.
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return cmd.Args[i+1]
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

	p := NewPreviewPane()
	p.SetSize(80, 30)

	// Happy path first: session alive, normal content renders.
	require.NoError(t, p.UpdateContent(setup.instance))
	require.False(t, p.previewState.fallback,
		"happy path must not render fallback")
	require.Contains(t, p.previewState.text, "hello world")

	// Session vanishes externally; redirect ErrorLog so we can prove
	// UpdateContent does not log anything at ERROR.
	var errBuf bytes.Buffer
	prev := log.ErrorLog.Writer()
	log.ErrorLog.SetOutput(&errBuf)
	defer log.ErrorLog.SetOutput(prev)

	sessionGone.Store(true)

	err := p.UpdateContent(setup.instance)
	require.NoError(t, err,
		"dead session must NOT bubble an error up to handleError")
	require.True(t, p.previewState.fallback,
		"preview must enter fallback state when session is gone")
	require.Contains(t, p.previewState.text, "Session no longer running")
	require.Empty(t, errBuf.String(),
		"no ERROR log line on session-gone fallback path")
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

	p := NewPreviewPane()
	p.SetSize(80, 30)

	// Render A normally, then enter scroll mode on A.
	require.NoError(t, p.UpdateContent(instA))
	require.NoError(t, p.ScrollUp(instA))
	require.True(t, p.isScrolling, "should be scrolling after ScrollUp on A")

	// Re-render the SAME instance while scrolling: scroll state must
	// survive (this is the original behavior the #470 fix must not break).
	require.NoError(t, p.UpdateContent(instA))
	require.True(t, p.isScrolling,
		"re-rendering the same instance must preserve scroll mode")

	// Now switch to B. Scroll state must be cleared and B's content rendered.
	require.NoError(t, p.UpdateContent(instB))
	require.False(t, p.isScrolling,
		"switching to a different instance must exit scroll mode")
	require.False(t, p.previewState.fallback,
		"B is a real running instance; preview must not be in fallback")
	require.Equal(t, previewB, p.previewState.text,
		"preview must reflect the newly selected instance's content")
	require.NotContains(t, p.viewport.View(), previewA,
		"stale viewport content from A must be cleared")
}
