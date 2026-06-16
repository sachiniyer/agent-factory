package ui

import (
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var terminalPaneStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var terminalFooterStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"})

var newTerminalTmuxSessionForRepo = tmux.NewTmuxSessionForRepo

// terminalSession holds a cached tmux session for a specific instance.
type terminalSession struct {
	tmuxSession  *tmux.TmuxSession
	worktreePath string
}

// TerminalPane manages shell tmux sessions in the worktree directory of selected instances.
// Sessions are cached per instance so switching between instances preserves terminal state.
type TerminalPane struct {
	mu            sync.Mutex
	width, height int
	sessions      map[string]*terminalSession // instanceTitle → session
	currentTitle  string                      // currently displayed instance
	content       string
	fallback      bool
	fallbackText  string

	isScrolling bool
	viewport    viewport.Model
}

func NewTerminalPane() *TerminalPane {
	return &TerminalPane{
		sessions: make(map[string]*terminalSession),
		viewport: viewport.New(0, 0),
	}
}

func (t *TerminalPane) SetSize(width, height int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.width = width
	t.height = height
	t.viewport.Width = width
	t.viewport.Height = height
	if s, ok := t.sessions[t.currentTitle]; ok && s.tmuxSession != nil {
		if err := s.tmuxSession.SetDetachedSize(width, height); err != nil {
			log.InfoLog.Printf("terminal pane: failed to set detached size: %v", err)
		}
	}
}

// dropStaleScrollState clears scroll-mode viewport content captured from a
// previously selected instance. Caller must hold t.mu.
//
// UpdateContent runs this on every refresh, but the mouse/keyboard scroll path
// (ScrollUp/ScrollDown) is driven straight off the bubbletea event loop and
// can fire before the async UpdateContent for the newly selected instance has
// run. Without this guard a scroll would re-capture and scroll the previous
// instance's terminal history instead of resetting scroll mode (#746). Unlike
// PreviewPane.dropStaleScrollState this does not adopt the new title:
// currentTitle is owned by ensureSessionLocked, which only sets it once a live
// session exists. Mirrors PreviewPane.dropStaleScrollState (#702), the same
// motivation as the setFallbackState consolidation in #669.
func (t *TerminalPane) dropStaleScrollState(instance *session.Instance) {
	title := ""
	if instance != nil {
		title = instance.Title
	}
	if t.isScrolling && t.currentTitle != "" && t.currentTitle != title {
		t.isScrolling = false
		t.viewport.SetContent("")
		t.viewport.GotoTop()
	}
}

// setFallbackState sets the terminal pane to display a fallback message.
// Caller must hold t.mu.
//
// Also resets scroll-mode state so fallback=true cannot coexist with
// isScrolling=true. String() checks isScrolling before fallback, so leaving
// scroll state intact when switching to a nil/unstarted/remote selection
// would render the prior instance's viewport instead of the fallback (#669).
func (t *TerminalPane) setFallbackState(message string) {
	t.fallback = true
	t.fallbackText = lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message)
	t.content = ""
	t.isScrolling = false
	t.viewport.SetContent("")
}

// UpdateContent captures the tmux pane output for the terminal session.
func (t *TerminalPane) UpdateContent(instance *session.Instance) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if instance == nil {
		t.setFallbackState("Select an instance to open a terminal")
		return nil
	}
	if !instance.Started() {
		t.setFallbackState("Instance is not started yet.")
		return nil
	}

	// Remote instances have no local worktree, so there's no local tmux
	// session to capture. When terminal_cmd is configured the tab is an
	// interactive-only surface (#843): prompt the user to attach. Otherwise
	// keep the "not available" fallback and name the config knob that
	// enables it.
	if instance.IsRemote() {
		if instance.SupportsRemoteTerminal() {
			t.setFallbackState("Press Enter to open a terminal on the remote machine.")
		} else {
			t.setFallbackState("Terminal tab not available for remote sessions.\nConfigure remote_hooks.terminal_cmd to enable it.\nUse the Preview tab to see session output.")
		}
		return nil
	}

	// If the selected instance changed while in scroll mode, exit scroll mode
	// so ensureSessionLocked() runs and updates t.currentTitle. Otherwise
	// Attach() would resolve t.sessions[t.currentTitle] to the previous
	// instance's session (issue #384).
	t.dropStaleScrollState(instance)

	// Skip content updates while in scroll mode
	if t.isScrolling {
		return nil
	}

	// Ensure we have a terminal session for this instance. On failure the
	// instance has no usable session, so show a fallback rather than
	// returning an error: the caller only logs it, leaving the previous
	// instance's captured content on screen (#747). Mirrors the
	// ErrSessionGone fallback handling below.
	if err := t.ensureSessionLocked(instance); err != nil {
		t.setFallbackState(fmt.Sprintf("Failed to start terminal session: %v", err))
		return nil
	}

	s, ok := t.sessions[t.currentTitle]
	if !ok || s.tmuxSession == nil || !s.tmuxSession.DoesSessionExist() {
		t.setFallbackState("Terminal session not available.")
		return nil
	}

	content, err := s.tmuxSession.CapturePaneContent()
	if err != nil {
		// The DoesSessionExist pre-check above can race against an external
		// kill of the tmux session. When CapturePaneContent reports the
		// session is gone, fall through to the fallback state instead of
		// propagating an error that handleError logs at ERROR (#496).
		if errors.Is(err, tmux.ErrSessionGone) {
			t.setFallbackState("Terminal session no longer running.")
			return nil
		}
		return fmt.Errorf("terminal pane: failed to capture content: %w", err)
	}

	t.fallback = false
	t.content = content
	return nil
}

// ensureSession creates or reuses a cached terminal tmux session for the given instance.
func (t *TerminalPane) ensureSession(instance *session.Instance) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ensureSessionLocked(instance)
}

// ensureSessionLocked is the lock-free implementation of ensureSession.
// Caller must hold t.mu.
func (t *TerminalPane) ensureSessionLocked(instance *session.Instance) error {
	if instance == nil || !instance.Started() {
		return nil
	}

	worktreePath := instance.GetWorktreePath()
	if worktreePath == "" {
		return nil
	}

	t.currentTitle = instance.Title

	// Check if we already have a cached session for this instance.
	// The cache is keyed by instance.Title, which is not guaranteed to be unique
	// across instances (titles can collide between CLI, task runner, and TUI).
	// Verify worktreePath matches before reusing a cached session; otherwise
	// kill the stale tmux session and recreate to avoid running commands in
	// the wrong worktree (issue #222).
	if s, ok := t.sessions[instance.Title]; ok {
		if s.worktreePath == worktreePath && s.tmuxSession != nil && s.tmuxSession.DoesSessionExist() {
			return nil
		}
		// Either the session died, or a different instance with the same title
		// claimed the slot. Close the stale tmux session before replacing the
		// cache entry.
		if s.tmuxSession != nil {
			if err := s.tmuxSession.Close(); err != nil {
				log.InfoLog.Printf("terminal pane: failed to close stale session for %s: %v", instance.Title, err)
			}
		}
		delete(t.sessions, instance.Title)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	termName := "term_" + instance.Title
	ts := newTerminalTmuxSessionForRepo(termName, worktreePath, shell)

	// Check if session already exists (e.g. from a previous run). Pass empty
	// workDir so Restore() does not silently re-spawn — this caller already
	// has its own kill-and-restart fallback below.
	if ts.DoesSessionExist() {
		if err := ts.Restore(""); err != nil {
			// Session exists but can't restore, kill it and start fresh
			if closeErr := ts.Close(); closeErr != nil {
				if ts.DoesSessionExist() {
					return fmt.Errorf("terminal pane: failed to close stale session %s: %w", termName, closeErr)
				}
				log.ErrorLog.Printf("terminal pane: partial cleanup of stale session %s: %v", termName, closeErr)
			}
			ts = newTerminalTmuxSessionForRepo(termName, worktreePath, shell)
			if err := ts.Start(worktreePath); err != nil {
				return fmt.Errorf("terminal pane: failed to start session: %w", err)
			}
		}
	} else {
		if err := ts.Start(worktreePath); err != nil {
			return fmt.Errorf("terminal pane: failed to start session: %w", err)
		}
	}

	t.sessions[instance.Title] = &terminalSession{
		tmuxSession:  ts,
		worktreePath: worktreePath,
	}

	// Set the size
	if t.width > 0 && t.height > 0 {
		if err := ts.SetDetachedSize(t.width, t.height); err != nil {
			log.InfoLog.Printf("terminal pane: failed to set size: %v", err)
		}
	}

	return nil
}

// Attach attaches to the terminal tmux session (full-screen).
func (t *TerminalPane) Attach() (chan struct{}, error) {
	t.mu.Lock()
	s, ok := t.sessions[t.currentTitle]
	if !ok || s.tmuxSession == nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("no terminal session to attach to")
	}
	if !s.tmuxSession.DoesSessionExist() {
		t.mu.Unlock()
		return nil, fmt.Errorf("terminal session does not exist")
	}
	ts := s.tmuxSession
	t.mu.Unlock()
	return ts.Attach()
}

// AttachForInstance binds the terminal pane to the given instance, then
// attaches. Deferred attach flows (the first-time attach help screen) must use
// this rather than Attach(): while the help overlay is open, a background
// refresh tick calls UpdateContent with the live (possibly drifted) selection,
// which rebinds currentTitle. Attach() would then connect to whatever instance
// is selected at dismiss time instead of the one the user pressed Enter on
// (#716).
//
// ensureSessionLocked sets currentTitle only when the instance is started with
// a live worktree; if it bailed early (the captured instance died during the
// help screen) we refuse to attach rather than falling back to a possibly
// drifted currentTitle.
func (t *TerminalPane) AttachForInstance(instance *session.Instance) (chan struct{}, error) {
	if instance == nil {
		return nil, fmt.Errorf("no terminal session to attach to")
	}
	// Remote instances bypass the local tmux session cache entirely: the
	// terminal_cmd hook runs behind its own PTY with the same detach-key
	// plumbing as the remote agent attach (#843). The captured-instance
	// semantics of this method are preserved — the hook is invoked on the
	// instance the user pressed Enter on, not a drifted selection (#716).
	if instance.IsRemote() {
		if !instance.SupportsRemoteTerminal() {
			return nil, fmt.Errorf("remote terminal is not configured: add a terminal_cmd to remote_hooks to enable the Terminal tab for remote sessions")
		}
		return instance.AttachRemoteTerminal()
	}
	t.mu.Lock()
	if err := t.ensureSessionLocked(instance); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	if t.currentTitle != instance.Title {
		t.mu.Unlock()
		return nil, fmt.Errorf("terminal session for %q is no longer available", instance.Title)
	}
	t.mu.Unlock()
	return t.Attach()
}

// Close kills all cached terminal tmux sessions and cleans up.
func (t *TerminalPane) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for title, s := range t.sessions {
		if s.tmuxSession != nil {
			if err := s.tmuxSession.Close(); err != nil {
				log.InfoLog.Printf("terminal pane: failed to close session for %s: %v", title, err)
			}
		}
	}
	t.sessions = make(map[string]*terminalSession)
	t.currentTitle = ""
	t.content = ""
	t.fallback = false
	t.fallbackText = ""
	// Match CloseForInstance: a wholesale teardown of "current" state should
	// also drop scroll-mode state so the pane is not left in a stuck
	// isScrolling=true state if it is ever reused (#619).
	if t.isScrolling {
		t.isScrolling = false
		t.viewport.SetContent("")
		t.viewport.GotoTop()
	}
}

// CloseForInstance kills the cached terminal session for a specific instance.
func (t *TerminalPane) CloseForInstance(title string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[title]; ok {
		if s.tmuxSession != nil {
			if err := s.tmuxSession.Close(); err != nil {
				log.InfoLog.Printf("terminal pane: failed to close session for %s: %v", title, err)
			}
		}
		delete(t.sessions, title)
	}
	if t.currentTitle == title {
		t.currentTitle = ""
		t.content = ""
		t.fallback = false
		t.fallbackText = ""
		// Drop scroll-mode state too; otherwise UpdateContent's isScrolling
		// guard suppresses the next selection's content updates until the
		// user presses ESC (issue #619, regression of #407). Mirrors
		// PreviewPane's instance-change reset.
		if t.isScrolling {
			t.isScrolling = false
			t.viewport.SetContent("")
			t.viewport.GotoTop()
		}
	}
}

func (t *TerminalPane) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	width := t.width
	height := t.height

	if width <= 0 || height <= 0 {
		return ""
	}

	if t.isScrolling {
		return t.viewport.View()
	}

	fallback := t.fallback
	fallbackText := t.fallbackText
	content := t.content

	if fallback {
		// TabbedWindow.SetSize already strips tab-bar and window-frame chrome
		// before sizing this pane, so height is the content height — use it
		// directly, like normal mode below pads to the full height.
		// Subtracting chrome again here rendered the fallback 7 lines short
		// and centered it 4 lines too high (#703). PreviewPane had the same
		// double subtraction and was fixed the same way (#616).
		return renderCenteredFallback(terminalPaneStyle, fallbackText, width, height)
	}

	// Normal mode: show captured content
	lines := strings.Split(content, "\n")

	// strings.Split produces a trailing empty element when content ends in "\n"
	// (common for tmux capture-pane output). Drop it so the off-by-one does not
	// trigger truncation when content actually fits, and so the truncate branch
	// keeps the right slice of lines. Mirrors PreviewPane.String() (#649, #898).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if height > 0 {
		if len(lines) > height {
			lines = lines[len(lines)-height:]
		} else {
			padding := height - len(lines)
			lines = append(lines, make([]string, padding)...)
		}
	}

	contentStr := strings.Join(lines, "\n")
	return terminalPaneStyle.Width(width).Render(contentStr)
}

// enterScrollMode captures the full terminal history and enters scroll mode.
// Caller must hold t.mu.
//
// Looks the session up by the selected instance's title rather than
// t.currentTitle: in the scroll path UpdateContent may not have run yet, so
// currentTitle can still name the previously selected instance and would
// capture its history (#746).
func (t *TerminalPane) enterScrollMode(instance *session.Instance) error {
	if instance == nil {
		return nil
	}
	s, ok := t.sessions[instance.Title]
	if !ok || s.tmuxSession == nil || !s.tmuxSession.DoesSessionExist() {
		return nil
	}

	content, err := s.tmuxSession.CapturePaneContentWithOptions("-", "-")
	if err != nil {
		if errors.Is(err, tmux.ErrSessionGone) {
			t.setFallbackState("Terminal session no longer running.")
			return nil
		}
		return fmt.Errorf("terminal pane: failed to capture full history: %w", err)
	}

	footer := terminalFooterStyle.Render("ESC to exit scroll mode")
	contentWithFooter := lipgloss.JoinVertical(lipgloss.Left, content, footer)
	t.viewport.SetContent(contentWithFooter)
	t.viewport.GotoBottom()
	t.isScrolling = true
	// Adopt the scrolled instance as current. The scroll path can run before
	// UpdateContent has bound currentTitle to the new selection; without this
	// the next refresh's dropStaleScrollState would see a mismatched title and
	// immediately discard the scroll the user just started (#746). Safe wrt
	// #716 — we only get here once a live session for this instance exists.
	t.currentTitle = instance.Title
	return nil
}

// ScrollUp enters scroll mode (if not already) and scrolls up.
func (t *TerminalPane) ScrollUp(instance *session.Instance) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropStaleScrollState(instance)
	if !t.isScrolling {
		return t.enterScrollMode(instance)
	}
	t.viewport.LineUp(1)
	return nil
}

// ScrollDown enters scroll mode (if not already) and scrolls down.
func (t *TerminalPane) ScrollDown(instance *session.Instance) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropStaleScrollState(instance)
	if !t.isScrolling {
		return t.enterScrollMode(instance)
	}
	t.viewport.LineDown(1)
	return nil
}

// ResetToNormalMode exits scroll mode and restores normal content display.
func (t *TerminalPane) ResetToNormalMode() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.isScrolling {
		return
	}
	t.isScrolling = false
	t.viewport.SetContent("")
	t.viewport.GotoTop()
}

// IsScrolling returns whether the terminal pane is in scroll mode.
func (t *TerminalPane) IsScrolling() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.isScrolling
}
