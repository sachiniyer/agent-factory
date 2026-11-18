package app

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// handleRebindProject dispatches the picker's `b` verb: rebind a registered
// project's stable id to the checkout at the entered path. The user-typed path
// is resolved against the TUI's filesystem — this process shares the user's
// machine, so ResolveUserPath is correct here exactly as in handleAddProject —
// and the daemon performs the registry write off the event loop, mirroring
// addProjectCmd. The picker stays open until the answer lands: a rejection
// (not a git repo, path owned by another project) is corrected inline via
// SetRebindError, a success closes and refreshes.
func (m *home) handleRebindProject(req overlay.RebindRequest) (tea.Model, tea.Cmd) {
	abs, err := config.ResolveUserPath(req.Path)
	if err != nil {
		m.projectPickerOverlay.SetRebindError(fmt.Sprintf("cannot resolve path: %s", req.Path))
		return m, nil
	}
	req.Path = abs
	return m, m.rebindProjectCmd(req)
}

// rebindProjectCmd rebinds a registered project through the daemon — the single
// writer (#960) — off the event loop, mirroring addProjectCmd/deleteProjectCmd.
// The reply carries the request's token so handleProjectRebound can tell it
// apart from a reply to any other picker.
func (m *home) rebindProjectCmd(req overlay.RebindRequest) tea.Cmd {
	return func() tea.Msg {
		// The registration's recorded (root, checkout) pair as the picker read
		// it: the rebind applies only if no other rebind — and no reclone at
		// the same path — has moved either half since (#4822 spec).
		project, err := rebindProjectThroughDaemon(req.Project.RegistryID, req.Project.RegistryRoot, req.Project.RegistryCheckoutID, req.Path)
		root := project.Root
		if root == "" {
			root = req.Path
		}
		return projectReboundMsg{
			token:           req.Token,
			projectID:       req.Project.RegistryID,
			oldRepoID:       req.Project.RepoID,
			oldRoot:         req.Project.Root,
			oldRegistryRoot: req.Project.RegistryRoot,
			name:            req.Project.Name,
			root:            root,
			err:             err,
		}
	}
}

// handleProjectRebound finalizes an async rebind. The reply belongs to the
// picker only when that picker is still open AND still waiting on this
// request's token; a picker closed and reopened in the meantime is a different
// request's (or none's), and must neither close nor show this reply's error.
//
// Three outcomes:
//   - Confirmed (success, or a committed error whose follow-up step failed): the
//     picker it owns closes, the Projects section is re-read, and a move of the
//     project the TUI is scoped to is followed to wherever the registry binds it
//     now (followActiveRebind). The success toast shows only when no other
//     picker is on screen — over a different picker it reads as that picker's.
//   - Unknown (lost in transport, cancelled): the picker it owns closes, the
//     Projects section is re-read, the scope is left alone, and one notice says
//     the outcome is unknown. Nothing is followed: the refreshed list is where
//     the user sees what happened.
//   - A definitive refusal: fed back inline to the picker that owns it, which
//     re-arms. An unowned one changed nothing; over a different picker it is
//     logged rather than shown, else it goes to the error box. The "rebound
//     elsewhere" refusal (#4822) first re-reads the registry, so the re-armed
//     form names — and next expects — the root the project is bound to now.
func (m *home) handleProjectRebound(msg projectReboundMsg) (tea.Model, tea.Cmd) {
	pickerOpen := m.projectPickerOverlay != nil && m.state == stateSwitchProject
	owned := pickerOpen && m.projectPickerOverlay.OwnsRebindReply(msg.token)
	committed := msg.err != nil && apiproto.IsMutationCommitted(msg.err)
	if msg.err != nil && !committed {
		if !rebindOutcomeUnknown(msg.err) {
			// A conflict moved a registration: the sidebar's Projects section
			// and the row this picker returns to are both stale in every
			// field, not just the recorded root. They rebuild ONCE — from the
			// daemon snapshot plus the single registry read inside it — so a
			// further registry move cannot leave the rows and the retry's
			// named root carrying different answers (#4888 review). The
			// snapshot fetch runs OFF the loop: local API calls carry no
			// response deadline, and inside Update a stalled one freezes input
			// and paint for the life of the stall. The form stays inert until
			// the reply re-arms it.
			var conflictRefresh tea.Cmd
			if apiclient.IsProjectRebound(msg.err) {
				conflictRefresh = m.rebindConflictSnapshotCmd(msg)
				if owned {
					return m, conflictRefresh
				}
			}
			if owned {
				m.projectPickerOverlay.SetRebindError(msg.err.Error())
				return m, nil
			}
			if pickerOpen {
				// A definitive refusal changed nothing, and shown over a
				// different picker it reads as THAT picker's result — the same
				// reason a stale success raises no toast there. Log it instead.
				log.WarningLog.Printf("stale rebind of project %q refused while another picker is open: %v", msg.name, msg.err)
				return m, conflictRefresh
			}
			return m, tea.Batch(conflictRefresh, m.handleError(fmt.Errorf("failed to rebind project %q: %w", msg.name, msg.err)))
		}
		if owned {
			m.closeProjectPicker()
		}
		m.refreshSidebarProjects()
		return m, m.handleError(fmt.Errorf("Rebind of %s · outcome unknown · check the project list", msg.name))
	}
	// The completion CONFIRMED the record moved — err is nil or the move
	// committed — so the old binding is dead even where the refresh's registry
	// read lands degraded and cannot prove the absence. Tombstone it before
	// the refresh, so the preserving merge cannot resurrect the old root and
	// checkout id from the last-known set (#7522 review).
	m.noteProjectRebound(msg.projectID)
	// The refresh resolves every root the follow compares in the same phase as
	// its registry read, so the reply holds Update for one bound in total even
	// when the root the rebind moved AWAY from — no longer in the registry — is
	// the one on a dead mount (#7397 review R1). The binding the notice names
	// and the follow targets is then read from the view that refresh installed:
	// the registry state the Projects section now shows, so a newer rebind by
	// another client is never displayed while an older one is followed.
	m.refreshSidebarProjectsResolving(rebindFollowPaths(msg, m.repoRoot))
	follow := m.renderedRebindFollow(msg)
	if owned {
		m.closeProjectPicker()
	}
	var notice tea.Cmd
	switch {
	case committed:
		// The move landed; a follow-up step failed. Follow it, and say what failed.
		notice = m.handleError(fmt.Errorf("rebound project %q, but: %w", msg.name, msg.err))
	case owned || !pickerOpen:
		// Name the root the registry holds NOW, never this reply's echo: another
		// client may have rebound the project again since this request committed.
		// A record that is gone (or unreadable) gets a root-free message.
		text := fmt.Sprintf("Rebound project '%s'", msg.name)
		if follow.bound {
			text = fmt.Sprintf("Rebound project '%s' to %s", msg.name, follow.root)
		}
		notice = m.showTransientMessage(text)
	}
	model, followCmd := m.followActiveRebind(msg, follow)
	return model, tea.Batch(notice, followCmd)
}

// rebindConflictRefreshTimeout bounds the off-loop snapshot fetch a conflict
// re-arm waits on. The daemon already answered with its definitive refusal, so
// a snapshot that accepts the connection and never answers must not leave the
// form pending forever — while it pends, the rebind-mode form consumes Esc and
// every other key. At the deadline the reply lands with the fetch marked
// failed, and the form re-arms on the refusal it already holds (#4888 review).
// Same bound the web modal applies to the identical re-read (REBIND_REFRESH_MS).
var rebindConflictRefreshTimeout = 10 * time.Second

// rebindConflictSnapshotCmd fetches the cross-repo session snapshot a "rebound
// elsewhere" rebuild needs — off the event loop, mirroring fetchSnapshotCmd's
// all-repos poll (#4888 review). The fetcher var is captured on the loop
// rather than read inside the goroutine, the same reason fetchSnapshotCmd
// captures its seams: a reassignment mid-flight must not race the read. The
// fetch itself carries rebindConflictRefreshTimeout as a real deadline on the
// transport read — not merely a select beside it — so a daemon that accepts
// the connection and never answers is cancelled outright rather than leaked
// as a goroutine parked on an unanswered socket per conflict (#4888 review).
// A stalled reply is reported as a failed fetch, which the preserve-merge
// treats as "session rows unknown, registry truth still applies".
func (m *home) rebindConflictSnapshotCmd(msg projectReboundMsg) tea.Cmd {
	fetch := allReposSnapshotFetcher
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rebindConflictRefreshTimeout)
		defer cancel()
		data, skipped, err := fetch(ctx)
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf(
				"rebind conflict refresh timed out after %s — the form re-arms on the daemon's refusal alone",
				rebindConflictRefreshTimeout)
		}
		return rebindConflictSnapshotMsg{
			token:     msg.token,
			projectID: msg.projectID,
			refusal:   msg.err,
			data:      data,
			skipped:   skipped,
			fetchErr:  err,
		}
	}
}

// handleRebindConflictSnapshot applies the conflict-time snapshot once it
// lands: the sidebar's Projects section always refreshes from it (a conflict
// moved a registration whether or not a picker is still waiting), and the
// picker rebuilds from the SAME list — but only while it still owns the reply.
// A picker closed or swapped in between keeps nothing of this reply; the form
// it re-arms would name a root nobody is waiting on.
func (m *home) handleRebindConflictSnapshot(msg rebindConflictSnapshotMsg) (tea.Model, tea.Cmd) {
	// The conflict-time reply's skipped set applies with its rows (#6986): a
	// successful fetch rewrites the retained set, a failed one leaves it.
	if msg.fetchErr == nil {
		m.applySkippedRepos(msg.skipped)
	}
	fresh, degraded := m.buildProjectListFrom(msg.data)
	m.applySidebarProjects(fresh, degraded, msg.fetchErr)
	if m.projectPickerOverlay == nil || m.state != stateSwitchProject ||
		!m.projectPickerOverlay.OwnsRebindReply(msg.token) {
		return m, nil
	}
	m.projectPickerOverlay.SetDegraded(degraded)
	// The rows installed below replace the counts the picker opened with, so
	// the snapshot half's health must travel with them: a conflict-time fetch
	// that failed leaves unconfirmed counts (a cross-repo rebind even adds a
	// zero-count row), and the D gate reads this flag before it lets a
	// delete-project confirmation claim "no live sessions" (#4888 review). A
	// fetch that answered rebuilds the counts, so it clears the flag.
	m.projectPickerOverlay.SetSnapshotDegraded(msg.fetchErr != nil)
	// The re-armed form names the root the rebuilt ROW carries — the read the
	// picker installs — so the message and the retry's expected root always
	// describe the same record. A record gone from the snapshot falls back to
	// the daemon's refusal text.
	text := msg.refusal.Error()
	for _, row := range fresh {
		if row.RegistryID == msg.projectID {
			root := row.RegistryRoot
			if root == "" {
				root = row.Root
			}
			text = fmt.Sprintf("Rebound elsewhere, to %s · Enter retries from there", root)
			break
		}
	}
	if msg.fetchErr != nil {
		// The session half of the snapshot never arrived: fresh carries only
		// the registry-side union, so rebuilding wholesale would drop every
		// session-derived row and zero the counts the daemon still runs. Keep
		// the picker's session-derived rows and re-attach their counts; only
		// registry truth replaces registry truth (#4888 review). Rows the
		// session itself just deleted or rebound are proven absent by the
		// mutations, though — the merge must not resurrect them (#7522 review).
		m.projectPickerOverlay.SetRebindConflict(text,
			overlay.MergePreservingSessionRows(m.dropSessionGoneRows(m.projectPickerOverlay.Projects(), degraded), fresh, degraded))
		return m, nil
	}
	if degraded {
		// The mirror image: the SESSION half landed but the registry read
		// failed, so fresh carries live session rows and no registry records.
		// Installing it wholesale would drop every registry-backed row behind
		// a read that cannot prove absence — and leave rebindTarget's stale
		// pair as the retry expectation (#4888 review). Merge fresh session
		// truth against only the picker's registry-backed rows; the degraded
		// flag preserves each record whose absence was never established, and
		// the merge grafts a same-repo session row onto the stale identity.
		m.projectPickerOverlay.SetRebindConflict(text,
			overlay.MergePreservingSessionRows(m.dropSessionGoneRows(registryBackedRows(m.projectPickerOverlay.Projects()), true), fresh, true))
		return m, nil
	}
	m.projectPickerOverlay.SetRebindConflict(text, fresh)
	return m, nil
}

// rebindOutcomeUnknown reports whether a failed rebind's outcome is not known:
// the shared mutationMayHaveLanded classification (#4820) — a reply lost after
// the daemon may have received the request, an unverifiable or undecodable
// reply, or a cancelled request — minus a committed error, which this handler
// treats as a known, landed move. An error the daemon returned in its envelope
// is a definitive refusal, and so is a transport failure that provably never
// reached the daemon: withDaemonHTTPMutation retried it, nothing was sent, and
// the picker can safely re-arm.
func rebindOutcomeUnknown(err error) bool {
	return mutationOutcomeUnknown(err)
}

// followActiveRebind moves the TUI's scope after a CONFIRMED rebind of the
// project it is scoped to. It follows the registry, never the reply's echo:
// another client may have rebound or deleted the project since this request
// committed, and the Projects section just re-read that newer state — follow
// is built from that very read. A record
// that is gone, a registry that cannot be read, or a record still naming the
// root it recorded (a no-op rebind, or one another client moved back) leaves
// the scope where it is. Unknown outcomes never reach it.
func (m *home) followActiveRebind(msg projectReboundMsg, follow rebindFollow) (tea.Model, tea.Cmd) {
	if !follow.bound || !m.rebindMovedActiveProject(msg, follow.spellings) {
		return m, nil
	}
	root, repoID, checkoutID := follow.root, follow.repoID, follow.checkoutID
	// "Did the record move" compares registry to registry: the recorded root
	// when the rebind was sent against the one now. The row's display root can
	// come from another source, and an unmoved record would then look moved.
	recorded := rebindRecordedRoot(msg)
	if follow.spellings.same(root, recorded) || follow.spellings.same(root, m.repoRoot) {
		return m, nil
	}
	// The follow-up probes the root the registry binds NOW, passing the SAME
	// record's identity the other selection entry points (sidebar Enter, mouse
	// click, picker Enter) carry. verifyBindingStillCurrent vouches for the
	// identity only at the write instant — the registry comment says the record
	// is last-known and drift is repaired by a later rebind, and another client
	// may even have rebound this record since this request committed — so
	// between the commit and this probe the checkout at the followed root may
	// have drifted: de-gitted into an enclosing repo, or replaced by a different
	// clone (#7109). The record's RepoID/CheckoutID (from the same binding read
	// as the root, so root and identity stay consistent even if another client
	// moved the record between the reply and the follow) arm the local branch's
	// repo-id and checkout-marker guards, so a root that no longer holds the
	// registered checkout is refused rather than silently re-scoping sessions,
	// tasks and the program onto a foreign repo — the exact silent adoption
	// #7109/#7116 closed on the three row-selection paths but left open on this
	// one. A rebind writes a real identity (never empty, never invented) and a
	// checkout marker, so the passed identity matches the probe only at a
	// healthy root; it fails closed on exactly the drift it should refuse.
	return m.switchToProjectRoot(root, repoID, checkoutID)
}

// rebindFollow is where the registry the Projects section renders binds a
// rebound project, and the spellings of every root the follow compares, all
// from the one read the reply's refresh made.
type rebindFollow struct {
	root, repoID, checkoutID string
	bound                    bool
	spellings                pathSpellings
}

// renderedRebindFollow builds the follow from the view the reply's refresh just
// installed, so it targets exactly the binding on screen. In remote mode the
// section renders the target's cached registry, which nothing re-reads between
// the rebind and its reply — it would name the root the project just left — so
// there is no binding to name or follow there, as there was none before: the
// local registry holds no record of the target's projects. The next poll shows
// where the project landed.
func (m *home) renderedRebindFollow(msg projectReboundMsg) rebindFollow {
	if isRemoteTarget() || m.registryView == nil {
		return rebindFollow{}
	}
	record, ok := m.registryView.binding(msg.projectID)
	return rebindFollow{
		root: record.Root, repoID: record.RepoID, checkoutID: record.CheckoutID,
		bound: ok, spellings: m.registryView.spellings,
	}
}

// rebindFollowPaths is every root followActiveRebind compares that the
// registry read does not already cover: the root the rebind was sent against,
// the row's, and the active one. The root bound now is a registry root.
func rebindFollowPaths(msg projectReboundMsg, activeRoot string) []string {
	return []string{rebindRecordedRoot(msg), msg.oldRoot, activeRoot}
}

// rebindRecordedRoot is the registry root the rebind was sent against, or the
// row's root when the row carried none.
func rebindRecordedRoot(msg projectReboundMsg) string {
	if msg.oldRegistryRoot != "" {
		return msg.oldRegistryRoot
	}
	return msg.oldRoot
}

// rebindMovedActiveProject reports whether a rebind moved the project the TUI
// is scoped to: the row's aggregation identity is the active repoID, or its
// root is the active root.
func (m *home) rebindMovedActiveProject(msg projectReboundMsg, spellings pathSpellings) bool {
	if msg.oldRepoID != "" && msg.oldRepoID == m.repoID {
		return true
	}
	return spellings.same(msg.oldRoot, m.repoRoot)
}
