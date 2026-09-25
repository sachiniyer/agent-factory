package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// This file moves the kill confirmation's data-loss checks off the event loop
// (#4848). It is the pattern for a key action whose blocking work decides what
// the user sees next:
//
//  1. The key handler does only in-memory work, then makes the key's effect
//     visible in the same Update — here the confirmation opens at once — with a
//     static (never animated) note while the result is outstanding.
//  2. The blocking work runs in a returned tea.Cmd, through a package-var seam
//     read on the event loop, so a test can swap in a fake that fails if Update
//     calls it.
//  3. The result comes back as a message carrying the identity of the state it
//     was started for, and the handler drops it when that state is gone.
//  4. A second press while a result is pending must not start a duplicate. Here
//     the open dialog owns the keyboard, so the second press cannot reach
//     handleKill; each moved action states its own rule.

// killCheckPendingNote is the confirmation's hint line while the loss checks
// run. Static text by rule — no spinner (#1766).
const killCheckPendingNote = "Checking for unsaved work…"

// killLossAssessment is what killing a session's worktree would destroy. Two
// independent losses, each with its own copy: a dirty worktree (uncommitted
// changes, #815) and local-only commits (committed-but-unmerged-and-unpushed
// work, #2022). A session can be both, so warnings accumulate. severe is set
// only with positive evidence of unmerged, unpushed commits; it escalates the
// confirmation to the critical-content guarantee (#1973) and a distinct confirm
// key, exactly as the reserved-root kill (#1238) does.
type killLossAssessment struct {
	warnings []string
	severe   string
}

// assessKillLoss runs the git reads behind the kill confirmation. It blocks —
// tens of ms normally, bounded by killGitTimeout per read — so it must only be
// reached from a tea.Cmd, never from Update (#4848).
func assessKillLoss(impact session.WorktreeCleanupImpact) killLossAssessment {
	var loss killLossAssessment
	wt := impact.Path
	if w := killConfirmationWarning(wt); w != "" {
		loss.warnings = append(loss.warnings, w)
	}
	if line, severe := unmergedCommitWarning(wt, impact.Branch, impact.BaseCommitSHA, impact.DeleteBranch); line != "" {
		if severe {
			loss.severe = line
		} else {
			// Fail-closed (unverifiable): warn, but do not force the extra
			// keystroke — we have not established that work is being lost.
			loss.warnings = append(loss.warnings, line)
		}
	}
	return loss
}

// killLossCheck is the seam killLossCheckCmd runs. A var only so tests can
// substitute a fake that proves Update never calls it; production never
// reassigns it.
var killLossCheck = assessKillLoss

// killLossCheckedMsg completes a pending kill confirmation. dialog is the
// overlay the check was started for: the result applies only while that exact
// dialog is still open, so a cancelled dialog — or a newer one for another
// press — never receives a stale assessment.
type killLossCheckedMsg struct {
	dialog   *overlay.ConfirmationOverlay
	title    string
	reserved bool
	impact   *session.WorktreeCleanupImpact
	loss     killLossAssessment
	action   tea.Cmd
}

// openPendingKillConfirm opens the kill confirmation before its loss checks
// have run. The copy already names what kill removes (from the in-memory
// cleanup impact); the confirm is withheld until the checks land, because the
// bare prompt may only show with positive evidence that nothing unmerged is lost
// (#2022). It returns the cmd that runs the checks.
func (m *home) openPendingKillConfirm(title string, reserved bool, impact *session.WorktreeCleanupImpact, action tea.Cmd) tea.Cmd {
	cmd := m.confirmAction(killConfirmMessage(title, "", reserved, impact), action)
	dialog := m.confirmationOverlay
	dialog.SetPending(killCheckPendingNote)
	// Read the seam here, on the event loop: reading it inside the cmd goroutine
	// would race a parallel test's swap (#960 PR 4 race-fix class).
	check := killLossCheck
	worktree := *impact
	return tea.Batch(cmd, func() tea.Msg {
		return killLossCheckedMsg{
			dialog:   dialog,
			title:    title,
			reserved: reserved,
			impact:   impact,
			loss:     check(worktree),
			action:   action,
		}
	})
}

// handleKillLossChecked replaces the pending dialog with the complete one, or
// drops the result when the dialog it was started for is gone.
func (m *home) handleKillLossChecked(msg killLossCheckedMsg) (tea.Model, tea.Cmd) {
	if m.state != stateConfirm || m.confirmationOverlay != msg.dialog {
		return m, nil
	}
	return m, m.openKillConfirm(msg.title, msg.reserved, msg.impact, msg.loss, msg.action)
}

// openKillConfirm opens the complete kill confirmation for an assessed loss.
func (m *home) openKillConfirm(title string, reserved bool, impact *session.WorktreeCleanupImpact, loss killLossAssessment, action tea.Cmd) tea.Cmd {
	if loss.severe != "" {
		// The severe consequence and any dirty-worktree warning are the critical
		// content the user is consenting to; they must render in full or the
		// overlay refuses the confirm (#1973). Only the recovery hint — genuine
		// elaboration — goes in the clippable detail.
		critical := killConfirmMessage(title, joinWarnings(append([]string{loss.severe}, loss.warnings...)), reserved, impact)
		archiveKey := keys.GlobalKeyBindings[keys.KeyArchive].Help().Key
		detail := fmt.Sprintf("Archive (%s) preserves the worktree and its refs — kill removes the worktree for good.", archiveKey)
		cmd := m.confirmActionWithDetail(critical, detail, action)
		m.confirmationOverlay.SetConfirmKey(unmergedKillConfirmKey)
		return cmd
	}

	cmd := m.confirmAction(killConfirmMessage(title, joinWarnings(loss.warnings), reserved, impact), action)
	if reserved {
		// Break the muscle-memory D+y reflex on the daemon-managed singleton
		// (#1238): a distinct confirm key means a reflexive 'y' — the ordinary
		// kill confirmation — is ignored, so the user has to read the warning
		// and press the named key before root is torn down.
		m.confirmationOverlay.SetConfirmKey(rootKillConfirmKey)
	}
	return cmd
}
