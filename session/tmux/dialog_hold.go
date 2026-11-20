package tmux

import (
	"crypto/sha256"
	"encoding/hex"
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

// DialogHoldKind is what kind of hold the gate is in, from a closed set. The
// text af shows for a hold is built from the kind and the pane's size and
// from nothing else, so no pane content and no peer's free text reaches it.
type DialogHoldKind string

const (
	// DialogHoldFolderUnreadable: the folder-trust dialog, in a shape af
	// cannot read as rows — a pane too small wraps or scrolls it.
	DialogHoldFolderUnreadable DialogHoldKind = "folder-unreadable"
	// DialogHoldFolderPartial: the folder-trust dialog, not provably whole —
	// a footer no supported build paints, or one only begun.
	DialogHoldFolderPartial DialogHoldKind = "folder-partial"
	// DialogHoldMCPUnreadable and DialogHoldMCPPartial: the same two holds on
	// one of the MCP server dialogs.
	DialogHoldMCPUnreadable DialogHoldKind = "mcp-unreadable"
	DialogHoldMCPPartial    DialogHoldKind = "mcp-partial"
	// DialogHoldRefused: a dialog af reads whole and still will not answer.
	DialogHoldRefused DialogHoldKind = "refused"
	// DialogHoldOther: any other hold, and any kind this build does not know.
	DialogHoldOther DialogHoldKind = "other"
)

// dialogHoldSubjects is the whole vocabulary of a hold's subject.
var dialogHoldSubjects = map[DialogHoldKind]string{
	DialogHoldFolderUnreadable: "a folder-trust dialog it can't read",
	DialogHoldFolderPartial:    "a folder-trust dialog it can't prove whole",
	DialogHoldMCPUnreadable:    "an MCP server dialog it can't read",
	DialogHoldMCPPartial:       "an MCP server dialog it can't prove whole",
	DialogHoldRefused:          "a Claude Code dialog it won't answer on its own",
	DialogHoldOther:            "a Claude Code dialog it can't read",
}

// KnownDialogHoldKind returns kind when it is one of the set, and
// DialogHoldOther for anything else — a kind from a newer peer included.
func KnownDialogHoldKind(kind DialogHoldKind) DialogHoldKind {
	if _, ok := dialogHoldSubjects[kind]; ok {
		return kind
	}
	return DialogHoldOther
}

// maxDialogHoldPaneSide bounds a pane dimension af will print.
const maxDialogHoldPaneSide = 9999

// DialogHoldSubject is what af is waiting on, built from the closed set: the
// kind's words and, when both dimensions are plausible, the pane size — "a
// folder-trust dialog it can't read (pane 60×12)".
func DialogHoldSubject(kind DialogHoldKind, width, height int) string {
	subject := dialogHoldSubjects[KnownDialogHoldKind(kind)]
	if width < 1 || height < 1 || width > maxDialogHoldPaneSide || height > maxDialogHoldPaneSide {
		return subject
	}
	return fmt.Sprintf("%s (pane %d×%d)", subject, width, height)
}

// dialogHoldLiveRemedy is the advice for a session that is still running, so
// its pane is there to answer. A failed create's pane is gone, and its error
// gives its own advice.
const dialogHoldLiveRemedy = "Widen the pane, or answer it in the session's agent tab."

// DialogHoldReason is the hold as one line for a live session's attention
// signal, built from the same closed set.
func DialogHoldReason(kind DialogHoldKind, width, height int) string {
	return "af is waiting on " + DialogHoldSubject(kind, width, height) + ". " + dialogHoldLiveRemedy
}

// DialogHoldNotice says the trust gate has held a dialog it will not answer,
// on a pane that has not changed for claudeDialogHoldSettle. The gate still
// holds and still sends no key; the notice only makes the hold visible.
type DialogHoldNotice struct {
	Kind DialogHoldKind
	// Width and Height are the pane's size, zero when tmux did not report it.
	Width, Height int
	// Since is when the pane last changed.
	Since time.Time
	// Key names the held pane: its incarnation — the pane's process and the
	// moment its tmux session was created — and the hash and length of what
	// it shows. The same pane showing the same bytes has the same key in any
	// process that computes it, so a consumer that persists it can tell "the
	// hold I already flagged" from a new one across its own restart. A pane
	// started again, in this namespace or a fresh one where process ids
	// repeat, has another creation time and so another key. A notice is only
	// raised with the incarnation known, so the key is never empty.
	Key string
}

// Subject is what af is waiting on, with the pane size when known.
func (n DialogHoldNotice) Subject() string { return DialogHoldSubject(n.Kind, n.Width, n.Height) }

// Reason is the notice as one line for a live session's attention signal.
func (n DialogHoldNotice) Reason() string { return DialogHoldReason(n.Kind, n.Width, n.Height) }

// dialogHoldKey derives a held pane's key from its incarnation and what the
// recorder keeps of its capture.
func dialogHoldKey(incarnation string, sum [sha256.Size]byte, size int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", incarnation, size)
	h.Write(sum[:])
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// dialogHoldRecorder times one held pane. It keeps a hash and length of the
// last held capture, never the capture, and the pane incarnation the clock
// was started against, so a respawned pane cannot inherit it.
//
// mu is a leaf: every method takes it for a few assignments and returns. No
// tmux call and no gate decision runs under it.
type dialogHoldRecorder struct {
	mu     sync.Mutex
	sum    [sha256.Size]byte
	size   int
	pane   string // the incarnation the clock is bound to; "" while unknown
	since  time.Time
	notice *DialogHoldNotice
	// checked is when the pane's incarnation was last confirmed for a raised
	// notice. See recheckDue.
	checked time.Time
}

// clear forgets the hold: the pane read clear, af sent a key, or a new pane
// process started.
func (r *dialogHoldRecorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sum, r.size, r.pane = [sha256.Size]byte{}, 0, ""
	r.since, r.notice, r.checked = time.Time{}, nil, time.Time{}
}

// observe notes one held capture taken at now. A capture that differs from the
// last held one restarts the clock and withdraws any notice. It returns when
// the pane last changed, whether this capture started the clock, and whether
// the notice is already raised.
func (r *dialogHoldRecorder) observe(now time.Time, sum [sha256.Size]byte, size int) (since time.Time, started, raised bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.since.IsZero() || r.sum != sum || r.size != size {
		r.sum, r.size, r.pane = sum, size, ""
		r.since, r.notice, r.checked = now, nil, time.Time{}
		return now, true, false
	}
	return r.since, false, r.notice != nil
}

// bindPane records the pane incarnation a clock started at since runs
// against. A clock restarted meanwhile is left alone.
func (r *dialogHoldRecorder) bindPane(since time.Time, incarnation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.since.Equal(since) {
		r.pane = incarnation
	}
}

// raise publishes the notice for the clock started at since, if pane is still
// the incarnation that clock was bound to. A different one means the pane was
// respawned under an identical-looking capture, or was never identified when
// the clock started: the clock restarts at now against this incarnation, and
// nothing is raised. An unknown incarnation raises nothing and binds nothing,
// so no notice ever carries a key that does not name a pane. It reports
// whether the notice went up.
func (r *dialogHoldRecorder) raise(now, since time.Time, pane paneIdentity, kind DialogHoldKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	incarnation := pane.incarnation()
	if !r.since.Equal(since) || r.notice != nil || incarnation == "" {
		return false
	}
	if r.pane != incarnation {
		r.pane, r.since = incarnation, now
		return false
	}
	r.notice = &DialogHoldNotice{
		Kind: kind, Width: pane.width, Height: pane.height,
		Since: since, Key: dialogHoldKey(incarnation, r.sum, r.size),
	}
	r.checked = now
	return true
}

// recheckDue reports whether a raised notice's pane is due to be identified
// again: once per settle time. The capture's hash cannot tell a pane from
// another one respawned outside af that shows the same bytes, and a notice
// that outlived its pane would hand its key, and any dismissal made on it, to
// the replacement. Checking once per settle time bounds that to one settle,
// for one tmux call in ten of the daemon's polls.
func (r *dialogHoldRecorder) recheckDue(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notice != nil && now.Sub(r.checked) >= claudeDialogHoldSettle
}

// recheck applies a fresh reading of the pane to a raised notice. The same
// incarnation confirms it. Another one withdraws the notice and restarts the
// clock at now against the pane that is there: its hold is a new one, with
// its own settle and its own key. An unknown incarnation proves nothing
// either way, so the notice stands and the next recheck asks again.
func (r *dialogHoldRecorder) recheck(now time.Time, pane paneIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notice == nil {
		return
	}
	r.checked = now
	if incarnation := pane.incarnation(); incarnation != "" && incarnation != r.pane {
		r.pane, r.since, r.notice, r.checked = incarnation, now, nil, time.Time{}
	}
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
		t.dialogHold.bindPane(since, pane.incarnation())
		return
	}
	if raised {
		if t.dialogHold.recheckDue(now) {
			pane, _ := t.readPaneIdentity()
			t.dialogHold.recheck(now, pane)
		}
		return
	}
	if now.Sub(since) < claudeDialogHoldSettle {
		return
	}
	// The pane is read again here: its incarnation must still be the one the
	// clock started against, and its size goes in the notice. A failed read
	// leaves the incarnation unknown, and an unknown pane is never flagged:
	// the next poll reads again.
	pane, _ := t.readPaneIdentity()
	kind := claudeDialogHoldKind(claudeScreenOf(content))
	if !t.dialogHold.raise(now, since, pane, kind) {
		return
	}
	log.WarningLog.Printf(
		"session %q: af is waiting on %s, unchanged for %s; af holds and sends no key, and the session is flagged so someone can answer it (#7635)",
		t.sanitizedName, DialogHoldSubject(kind, pane.width, pane.height), now.Sub(since).Round(time.Second))
}

// claudeDialogHoldKind names the clause of CheckAndHandleTrustPrompt's Claude
// branch that held this screen. It asks the gate's own readers in the gate's
// own order, top to bottom, so the first clause that holds here is the one
// that held there. It reads; it decides nothing and sends nothing.
//
// Keep it in step with that branch: a clause added there belongs here, at the
// same position. A screen no clause claims reads DialogHoldOther.
func claudeDialogHoldKind(screen visibleText) DialogHoldKind {
	// A dialog af reads whole. It reached answerClaudeTrustPrompt and that
	// still held: a draft's rule under it, a refusal, a key still pending.
	if claudeTrustPromptPresent(screen) {
		return DialogHoldRefused
	}
	if claudeMCPDialogPartiallyRendered(screen) {
		return DialogHoldMCPPartial
	}
	switch claudeMCPServersPickerOf(screen) {
	case claudeMCPServersPartial:
		return DialogHoldMCPPartial
	case claudeMCPServersUnreadable:
		return DialogHoldMCPUnreadable
	}
	switch claudeMCPServerDialogOf(screen).verdict {
	case claudeMCPServerPartial:
		return DialogHoldMCPPartial
	case claudeMCPServerUnreadable:
		return DialogHoldMCPUnreadable
	}
	switch claudeFolderTrustDialogOf(screen) {
	case claudeFolderTrustPartial:
		return DialogHoldFolderPartial
	case claudeFolderTrustUnreadable:
		return DialogHoldFolderUnreadable
	}
	if claudeLegacyTrustDialogOf(screen) == claudeLegacyTrustPartial {
		return DialogHoldFolderPartial
	}
	if _, held := claudeMCPTrustTailOf(screen); held {
		return DialogHoldMCPUnreadable
	}
	if claudeMCPTrustPaintingOf(screen) {
		return DialogHoldMCPPartial
	}
	if match, held := claudeLaunchDialogRewrapOf(screen); held {
		switch match.kind {
		case claudeLaunchDialogReworded, claudeLaunchDialogLegacy:
			return DialogHoldFolderUnreadable
		case claudeLaunchDialogMCPServer:
			return DialogHoldMCPUnreadable
		}
		return DialogHoldOther // words the folder and MCP questions share
	}
	if claudeMCPServersTailOf(screen) {
		return DialogHoldMCPUnreadable
	}
	if claudeMCPServersPaintingOf(screen) {
		return DialogHoldMCPPartial
	}
	if claudeFolderTrustPaintingOf(screen) || claudeLegacyTrustPaintingOf(screen) ||
		claudeFolderTrustQuestionPaintingOf(screen) {
		return DialogHoldFolderPartial
	}
	return DialogHoldOther
}

// paneIdentity is the pane's process, size, and its tmux session's creation
// time, as tmux reports them.
type paneIdentity struct {
	pid           string
	created       string
	width, height int
}

// incarnation names this run of the pane: its process and the moment its tmux
// session was created. A process id alone can recur — a re-provisioned
// sandbox starts a fresh PID namespace — and the creation time cannot recur
// with it. It is "" unless both are known.
func (p paneIdentity) incarnation() string {
	if p.pid == "" || p.created == "" {
		return ""
	}
	return p.pid + "@" + p.created
}

const paneIdentityFormat = "#{pane_pid} #{pane_width} #{pane_height} #{session_created}"

// readPaneIdentity asks tmux for the pane's process id, size and session
// creation time. On any failure it returns the zero identity: an unknown
// incarnation and no size.
func (t *TmuxSession) readPaneIdentity() (paneIdentity, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", ExactTarget(t.sanitizedName), paneIdentityFormat)
	if err != nil {
		return paneIdentity{}, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 4 {
		return paneIdentity{}, fmt.Errorf("unexpected pane identity %q", string(output))
	}
	var p paneIdentity
	if _, err := fmt.Sscanf(fields[1]+" "+fields[2], "%d %d", &p.width, &p.height); err != nil {
		return paneIdentity{}, fmt.Errorf("unexpected pane size %q", string(output))
	}
	p.pid, p.created = fields[0], fields[3]
	return p, nil
}
