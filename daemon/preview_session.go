package daemon

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// maxPreviewSessionLines bounds PreviewSession: it is a glance at what an agent
// is doing, not a capture. Scrollback and other tabs stay with Preview and
// `af sessions preview`.
const maxPreviewSessionLines = 40

// PreviewSessionRequest asks for the last Lines lines of a session's agent tab.
// ID is the session's stable id; Lines 0 means maxPreviewSessionLines.
type PreviewSessionRequest struct {
	ID    string `json:"id"`
	Lines int    `json:"lines"`
}

// PreviewSessionResponse is the agent tab's visible screen as plain text.
type PreviewSessionResponse struct {
	Text string `json:"text"`
}

// PreviewSession is the public, read-only short form of Preview: the last lines
// of a session's agent tab as plain text, with escape sequences and control
// characters removed. It reads a subset of what Preview already returns to the
// same callers.
//
// It resolves by stable id in the tracked instance map only — never a title,
// never a refresh — so an unknown id costs no disk scan, whoever sends it.
func (s *controlServer) PreviewSession(req PreviewSessionRequest, resp *PreviewSessionResponse) error {
	if req.ID == "" {
		return errors.New("id is required")
	}
	lines := req.Lines
	if lines == 0 {
		lines = maxPreviewSessionLines
	}
	if lines < 0 || lines > maxPreviewSessionLines {
		return fmt.Errorf("lines must be between 1 and %d, got %d", maxPreviewSessionLines, req.Lines)
	}
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	m := s.manager
	m.mu.Lock()
	instance, repoID := m.trackedSessionByIDLocked(req.ID)
	killing := false
	if instance != nil {
		_, killing = m.killsInFlight[daemonInstanceKey(repoID, instance.Title)]
	}
	m.mu.Unlock()
	switch {
	case instance == nil:
		return fmt.Errorf("session %q not found; pass the session id, not its title", req.ID)
	case killing:
		return fmt.Errorf("session %q is being deleted", req.ID)
	case instance.IsArchived():
		return fmt.Errorf("session %q is archived; restore it to preview it", req.ID)
	case !instance.Started():
		return fmt.Errorf("session %q is not running; restore or start it to preview it", req.ID)
	}
	tabID := ""
	for _, t := range instance.GetTabs() {
		if t.Kind == session.TabKindAgent {
			tabID = t.ID
			break
		}
	}
	if tabID == "" {
		return fmt.Errorf("session %q has no agent tab to preview", req.ID)
	}
	snap, err := instance.AgentServer().PreviewByID(tabID, false)
	if err != nil {
		return previewSessionErr(req.ID, err)
	}
	resp.Text = lastLines(plainText(snap.Content), lines)
	return nil
}

// previewSessionErr gives the two ways a live capture can lose its pane their own
// actionable messages; anything else passes through.
func previewSessionErr(id string, err error) error {
	switch {
	case errors.Is(err, session.ErrTabGone):
		return fmt.Errorf("session %q agent tab closed during the preview; try again", id)
	case errors.Is(err, tmux.ErrSessionGone):
		return fmt.Errorf("session %q is not running; restore it to preview it", id)
	}
	return err
}

// plainText strips escape sequences, then the control bytes xansi.Strip keeps
// (C0 other than newline and tab, DEL, C1) and invalid UTF-8 such as a raw
// 8-bit CSI, so the text is safe to show anywhere.
func plainText(s string) string {
	s = xansi.Strip(s)
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r == utf8.RuneError && size == 1 {
			continue
		}
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lastLines returns the last n lines of text with trailing spaces trimmed and
// the screen's trailing blank rows dropped, so a short pane is not mostly empty.
func lastLines(text string, n int) string {
	all := strings.Split(text, "\n")
	for i := range all {
		all[i] = strings.TrimRight(all[i], " \t")
	}
	end := len(all)
	for end > 0 && all[end-1] == "" {
		end--
	}
	start := max(0, end-n)
	return strings.Join(all[start:end], "\n")
}
