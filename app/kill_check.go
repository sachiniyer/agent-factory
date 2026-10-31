package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
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
	// remote is set for an off-box session: its recovery hint describes what an
	// off-box archive does, not the local worktree-move one (#5966).
	remote bool
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
func (m *home) openPendingKillConfirm(title string, reserved bool, impact *session.WorktreeCleanupImpact, base killLossAssessment, action tea.Cmd) tea.Cmd {
	// Read the seam here, on the event loop: reading it inside the cmd goroutine
	// would race a parallel test's swap (#960 PR 4 race-fix class).
	check := killLossCheck
	worktree := *impact
	return m.openPendingKillConfirmWith(title, reserved, impact, base, action, func() killLossAssessment { return check(worktree) })
}

// openPendingRemoteKillConfirm is openPendingKillConfirm for an off-box
// session: its workspace is on another host, so the loss is measured by the
// session's agent-server through the daemon (#5571 W8.2) instead of by git on
// this machine's disk.
func (m *home) openPendingRemoteKillConfirm(title string, reserved bool, target sessionActionTarget, base killLossAssessment, action tea.Cmd) tea.Cmd {
	check := remoteKillLossCheck
	return m.openPendingKillConfirmWith(title, reserved, nil, base, action, func() killLossAssessment { return check(target) })
}

func (m *home) openPendingKillConfirmWith(title string, reserved bool, impact *session.WorktreeCleanupImpact, base killLossAssessment, action tea.Cmd, measure func() killLossAssessment) tea.Cmd {
	cmd := m.confirmAction(killConfirmMessage(title, "", reserved, impact), action)
	dialog := m.confirmationOverlay
	dialog.SetPending(killCheckPendingNote)
	return tea.Batch(cmd, func() tea.Msg {
		loss := measure()
		// In-memory warnings captured at open — the unreachable-sandbox notice
		// (#5571 W1.3) — lead; the assessed findings trail them.
		loss.warnings = append(base.warnings, loss.warnings...)
		if loss.severe == "" {
			loss.severe = base.severe
		}
		loss.remote = loss.remote || base.remote
		return killLossCheckedMsg{
			dialog:   dialog,
			title:    title,
			reserved: reserved,
			impact:   impact,
			loss:     loss,
			action:   action,
		}
	})
}

// remoteKillLossCheckTimeout bounds the daemon round-trip (itself a hop to the
// workspace host) the off-box kill dialog waits on, so an unreachable host
// leaves the dialog pending for a bounded time, never forever.
var remoteKillLossCheckTimeout = 20 * time.Second

// workspaceFactsThroughDaemon asks the daemon to measure an off-box session's
// workspace. A package var so tests can substitute a fake.
var workspaceFactsThroughDaemon = func(target sessionActionTarget) (daemon.WorkspaceFactsResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteKillLossCheckTimeout)
	defer cancel()
	var resp daemon.WorkspaceFactsResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.WorkspaceFacts(ctx, daemon.WorkspaceFactsRequest{ID: target.id, Title: target.title, RepoID: target.repoID})
		return e
	})
	return resp, err
}

// remoteKillLossCheck is the seam openPendingRemoteKillConfirm runs; production
// never reassigns it.
var remoteKillLossCheck = func(target sessionActionTarget) killLossAssessment {
	return assessWorkspaceFacts(workspaceFactsThroughDaemon(target))
}

// assessWorkspaceFacts turns a measured off-box workspace into the same
// assessment a local worktree gets: commits that exist only in the workspace are
// severe (the distinct confirm key), everything else a warning. A workspace that
// could not be measured is a fail-closed warning, never silence.
func assessWorkspaceFacts(resp daemon.WorkspaceFactsResponse, err error) killLossAssessment {
	loss := killLossAssessment{remote: true}
	if err != nil || !resp.Measured {
		reason := resp.Reason
		if err != nil {
			reason = err.Error()
		}
		loss.warnings = append(loss.warnings, fmt.Sprintf("Warning: Could not verify the workspace (%s); it may contain unsaved work that will be lost!", reason))
		return loss
	}
	f := resp.Facts
	if !f.Exists {
		loss.warnings = append(loss.warnings, "The workspace is already missing from its host, so there is no work left in it to lose.")
		return loss
	}
	if f.CheckoutMissing {
		// The linked checkout is gone but its clone is not: the session branch, its
		// commits and the stash live in the clone's git directory, which teardown
		// removes with the rest of the session directory.
		loss.warnings = append(loss.warnings, "The agent's checkout is already missing from its host, but the clone it was made from is not: its branches, commits and stashes will be deleted with the session.")
	}
	if f.UnpushedCommits > 0 {
		loss.severe = fmt.Sprintf("Warning: %s exist only in this workspace (not pushed to any remote) and will be permanently lost!", plural(f.UnpushedCommits, "commit"))
	}
	if f.DirtyFiles > 0 {
		loss.warnings = append(loss.warnings, fmt.Sprintf("Warning: This workspace has %s that will be lost!", plural(f.DirtyFiles, "uncommitted file")))
	}
	if f.ExtraBranches > 0 {
		loss.warnings = append(loss.warnings, fmt.Sprintf("It also holds %s that will be deleted.", plural(f.ExtraBranches, "other local branch")))
	}
	if f.Stashes > 0 {
		loss.warnings = append(loss.warnings, fmt.Sprintf("It also holds %s that will be deleted.", plural(f.Stashes, "stash entry")))
	}
	if len(f.Unverified) > 0 {
		// A fact git could not answer is unknown, and unknown is not safe: the
		// counts above are lower bounds for it.
		loss.warnings = append(loss.warnings, fmt.Sprintf("Warning: Could not verify %s; the workspace may hold more work than is listed here and it will be lost!", strings.Join(f.Unverified, ", ")))
	}
	return loss
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	if noun == "other local branch" {
		return fmt.Sprintf("%d other local branches", n)
	}
	if noun == "stash entry" {
		return fmt.Sprintf("%d stash entries", n)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
		detail := killArchiveHint(loss.remote, archiveKey)
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

// killArchiveHint is the recovery hint under a severe kill dialog, true to the
// backend being killed. A local archive moves the worktree aside, keeping it and
// every ref. Off-box, archive is not a full backup: a docker or hook archive
// pushes only the current branch before its sandbox is removed, so other branches
// and stashes are lost, and an ssh or sandbox archive leaves the workspace on its
// host rather than copying anything. Kill removes the workspace for good.
func killArchiveHint(remote bool, archiveKey string) string {
	if remote {
		return fmt.Sprintf("Archive (%s) is not a full backup of a remote workspace: a docker or hook archive pushes only the current branch, so other branches and stashes are lost; copy those off the host first. Kill removes the workspace for good.", archiveKey)
	}
	return fmt.Sprintf("Archive (%s) preserves the worktree and its refs — kill removes the worktree for good.", archiveKey)
}
