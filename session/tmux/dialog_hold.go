package tmux

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// claudeDialogHoldSettle is how long a Claude Code pane must show the same
// held dialog, byte for byte, before af says so (#7635). It is measured from
// the pane's last change.
//
// It is not a paint budget: the gate's own readers already tell a dialog still
// painting from a settled one. It is how long af stays quiet about a hold that
// may still resolve by itself. Every supported build measured paints its
// folder-trust dialog inside one 10ms capture sample (#7635's table), so ten
// seconds is hundreds of paints of headroom on a loaded box, ten of the
// daemon's one-second polls, and half the create path's attempt budget — a
// create that ends on a held dialog has had the notice for the second half of
// its wait.
const claudeDialogHoldSettle = 10 * time.Second

// DialogHoldNotice says the trust gate has held a dialog it will not answer,
// on a pane that has not changed for claudeDialogHoldSettle. The gate still
// holds and still sends no key; the notice only makes the hold visible.
type DialogHoldNotice struct {
	// Subject is what af is waiting on, with the pane size when tmux reported
	// it: "a folder-trust dialog it can't read (pane 60×12)".
	Subject string
	// Since is when the pane last changed. One held, unchanged pane keeps one
	// Since, so a consumer can raise its signal once per hold.
	Since time.Time
}

// dialogHoldLiveRemedy is the advice for a session that is still running, so
// its pane is there to answer. A failed create's pane is gone, and its error
// gives its own advice.
const dialogHoldLiveRemedy = "Widen the pane, or answer it in the session's agent tab."

// dialogHoldReasonLead opens every notice's reason.
const dialogHoldReasonLead = "af is waiting on "

// Reason is the notice as one line for a live session's attention signal.
func (n DialogHoldNotice) Reason() string {
	return dialogHoldReasonLead + n.Subject + ". " + dialogHoldLiveRemedy
}

// IsDialogHoldReason reports whether reason has the shape Reason writes: its
// lead, a subject, and the live remedy.
func IsDialogHoldReason(reason string) bool {
	subject, ok := strings.CutPrefix(reason, dialogHoldReasonLead)
	if !ok {
		return false
	}
	subject, ok = strings.CutSuffix(subject, ". "+dialogHoldLiveRemedy)
	return ok && subject != ""
}

// dialogHoldRecorder times one held pane. It keeps a hash and length of the
// last held capture, never the capture, and the pane process the clock was
// started against, so a respawned pane cannot inherit it.
//
// mu is a leaf: every method takes it for a few assignments and returns. No
// tmux call and no gate decision runs under it.
type dialogHoldRecorder struct {
	mu      sync.Mutex
	sum     [sha256.Size]byte
	size    int
	panePID string
	since   time.Time
	notice  *DialogHoldNotice
}

// clear forgets the hold: the pane read clear, af sent a key, or a new pane
// process started.
func (r *dialogHoldRecorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sum, r.size, r.panePID = [sha256.Size]byte{}, 0, ""
	r.since, r.notice = time.Time{}, nil
}

// observe notes one held capture taken at now. A capture that differs from the
// last held one restarts the clock and withdraws any notice. It returns when
// the pane last changed, whether this capture started the clock, and whether
// the notice is already raised.
func (r *dialogHoldRecorder) observe(now time.Time, sum [sha256.Size]byte, size int) (since time.Time, started, raised bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.since.IsZero() || r.sum != sum || r.size != size {
		r.sum, r.size, r.panePID = sum, size, ""
		r.since, r.notice = now, nil
		return now, true, false
	}
	return r.since, false, r.notice != nil
}

// bindPane records the pane process a clock started at since runs against. A
// clock restarted meanwhile is left alone.
func (r *dialogHoldRecorder) bindPane(since time.Time, panePID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.since.Equal(since) {
		r.panePID = panePID
	}
}

// raise publishes the notice for the clock started at since, if panePID is
// still the process that clock was bound to. A different process means the
// pane was respawned under an identical-looking capture: the clock restarts at
// now against the new process, and nothing is raised. It reports whether the
// notice went up.
func (r *dialogHoldRecorder) raise(now, since time.Time, panePID, subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.since.Equal(since) || r.notice != nil {
		return false
	}
	if r.panePID != panePID {
		r.panePID, r.since = panePID, now
		return false
	}
	r.notice = &DialogHoldNotice{Subject: subject, Since: since}
	return true
}

// current returns the raised notice, if any.
func (r *dialogHoldRecorder) current() (DialogHoldNotice, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notice == nil {
		return DialogHoldNotice{}, false
	}
	return *r.notice, true
}

// DialogHoldNotice returns the notice for a dialog the trust gate has held on
// an unchanged pane past the settle time, or false. It takes only the
// recorder's own lock, so a reader never waits on pane input.
func (t *TmuxSession) DialogHoldNotice() (DialogHoldNotice, bool) {
	return t.dialogHold.current()
}

// recordDialogHold runs after CheckAndHandleTrustPrompt has decided, with the
// capture it decided on. It never changes that decision and sends no key: it
// only times a hold. held is the gate's answer; keysBefore is dialogKeysSent
// from before the decision, so a poll in which af pressed a key counts as
// progress, not as a hold. Caller holds inputMu.
func (t *TmuxSession) recordDialogHold(content string, keysBefore uint64, held bool) {
	if DetectAgentFromCommand(t.programCmd()) != ProgramClaude {
		return
	}
	if !held || t.dialogKeysSent() != keysBefore {
		t.dialogHold.clear()
		return
	}
	now := claudeTrustNow()
	since, started, raised := t.dialogHold.observe(now, sha256.Sum256([]byte(content)), len(content))
	if started {
		pane, _ := t.readPaneIdentity()
		t.dialogHold.bindPane(since, pane.pid)
		return
	}
	if raised || now.Sub(since) < claudeDialogHoldSettle {
		return
	}
	// The pane is read again here, once per hold: its process must still be
	// the one the clock started against, and its size goes in the notice. A
	// failed read reports an unknown process. That matches only a clock whose
	// first read failed too, and then the notice carries no size.
	pane, _ := t.readPaneIdentity()
	subject := claudeDialogHoldSubject(claudeScreenOf(content)) + pane.sizeNote()
	if !t.dialogHold.raise(now, since, pane.pid, subject) {
		return
	}
	log.WarningLog.Printf(
		"session %q: af is waiting on %s, unchanged for %s; af holds and sends no key, and the session is flagged so someone can answer it (#7635)",
		t.sanitizedName, subject, now.Sub(since).Round(time.Second))
}

// claudeDialogHoldSubject names the held dialog from the gate's own readers.
// It reads; it decides nothing. The order follows the gate's: a dialog af
// reads whole but will not answer, then the folder-trust dialog it cannot
// read, then one it cannot prove whole, then any other hold.
func claudeDialogHoldSubject(screen visibleText) string {
	if claudeTrustPromptPresent(screen) {
		return "a Claude Code dialog it won't answer on its own"
	}
	switch claudeFolderTrustDialogOf(screen) {
	case claudeFolderTrustUnreadable:
		return "a folder-trust dialog it can't read"
	case claudeFolderTrustPartial:
		return "a folder-trust dialog it can't prove whole"
	}
	if claudeLegacyTrustDialogOf(screen) == claudeLegacyTrustPartial {
		return "a folder-trust dialog it can't prove whole"
	}
	return "a Claude Code dialog it can't read"
}

// paneIdentity is the pane's process and size, as tmux reports them.
type paneIdentity struct {
	pid           string
	width, height int
}

// sizeNote is " (pane 60×12)", or nothing when the size is unknown.
func (p paneIdentity) sizeNote() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}
	return fmt.Sprintf(" (pane %d×%d)", p.width, p.height)
}

const paneIdentityFormat = "#{pane_pid} #{pane_width} #{pane_height}"

// readPaneIdentity asks tmux for the pane's process id and size. On any
// failure it returns the zero identity: an unknown process and no size.
func (t *TmuxSession) readPaneIdentity() (paneIdentity, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", ExactTarget(t.sanitizedName), paneIdentityFormat)
	if err != nil {
		return paneIdentity{}, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 3 {
		return paneIdentity{}, fmt.Errorf("unexpected pane identity %q", string(output))
	}
	var p paneIdentity
	if _, err := fmt.Sscanf(fields[1]+" "+fields[2], "%d %d", &p.width, &p.height); err != nil {
		return paneIdentity{}, fmt.Errorf("unexpected pane size %q", string(output))
	}
	p.pid = fields[0]
	return p, nil
}
