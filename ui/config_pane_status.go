package ui

import "strings"

// The pane's ONE status line (#5571 review). Three sections can produce an
// outcome — the config write path, the Accounts section, the Hosts section —
// and a per-channel precedence cannot report them honestly: whichever order
// the channels are checked in, a stale outcome can always sit in front of a
// newer one. Instead every outcome lands on the shared statusLine through its
// channel's setter, so the line always reports the LATEST event; a clear
// consults the `from` tag so a section retiring ITS status cannot blank a
// newer outcome it does not own. The per-channel fields stay for their own
// readers (the add form's footer, the account-outcome accessor).

// statusChannel identifies which section produced the outcome on the line.
type statusChannel int

const (
	statusFromPane statusChannel = iota
	statusFromAccounts
	statusFromHosts
)

// statusEvent is one outcome: what the line says, whether it is an error, and
// the restart notice a config write can carry. `from` lets a section retire
// ITS status without blanking a newer outcome another section owns.
type statusEvent struct {
	text    string
	isError bool
	notice  string
	from    statusChannel
}

// setStatus records the pane channel's outcome and takes the status line —
// the one line every section's writer feeds, so the newest event always wins.
func (c *ConfigPane) setStatus(text string, isError bool, notice string) {
	c.status, c.statusIsError, c.restartNotice = text, isError, notice
	c.statusLine = statusEvent{text: text, isError: isError, notice: notice, from: statusFromPane}
}

// clearStatus drops the last write's echo, its restart notice, and any error.
//
// All three must go together: leaving statusIsError set while clearing the text
// would render an empty error line, and leaving the notice would tell a user to
// restart for an edit they cannot see. The line blanks only when the pane owns
// the outcome on it — clearing here must not erase a newer one another
// section produced.
func (c *ConfigPane) clearStatus() {
	c.status = ""
	c.statusIsError = false
	c.restartNotice = ""
	if c.statusLine.from == statusFromPane {
		c.statusLine = statusEvent{}
	}
}

// renderStatus renders the pane's one status line — the newest outcome any
// section produced.
//
// The notice is shown AT THE MOMENT OF THE EDIT rather than on close or in a
// banner: a user who sets a value and looks away has been told exactly when
// they were in a position to act on it.
func (c *ConfigPane) renderStatus() string {
	if c.statusLine.text == "" {
		return ""
	}
	// Wrapped for the same reason the purpose is: these are the longest strings
	// on screen (a validator error runs to 200+ characters), and an unwrapped one
	// makes countLines undercount the footer, which steals the window's budget.
	var b strings.Builder
	if c.statusLine.isError {
		b.WriteString(c.wrap(c.statusLine.text, configErrorStyle))
		return b.String()
	}
	b.WriteString(c.wrap(c.statusLine.text, configOKStyle))
	if c.statusLine.notice != "" {
		b.WriteString(c.wrap(c.statusLine.notice, configNoticeStyle))
	}
	return b.String()
}
