package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// repoSessionTitlesLocked returns the titles of the repo's sessions the daemon still
// tracks, across every create/restore phase: m.reservedTitles (admitted but not yet
// projected), m.pendingCreates (provisioning, so not yet in m.instances, #2549),
// m.restoresInFlight (the restore-only subset of m.killsInFlight), and m.instances
// (live, non-archived rows). m.instances is not the universe (the same class the #1892
// ghostTaskRuns comment in manager.go warns about, reached through a different door): a
// delete that walked only it would miss a still-provisioning create or a restoring
// archive, deregister the project, and let that session finish into a live orphan.
//
// inFlightOnly selects the ones a delete cannot archive YET — every pending create,
// plus any m.instances row with an op in flight — which is what the up-front fail-closed
// gate refuses on. With inFlightOnly false it returns every live-or-pending session,
// which is the concurrent-create re-check before the deregister. The caller holds m.mu.
func (m *Manager) repoSessionTitlesLocked(repoID string, inFlightOnly bool) []string {
	titleSet := make(map[string]struct{})
	// A successful reservation is already an admitted create, even during the
	// narrow interval before CreateSession publishes pendingCreates. Counting it
	// here makes the delete fence decision atomic with reserveCreate under m.mu.
	for key := range m.reservedTitles {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	// A pending create is always in flight (still provisioning), so it counts either way.
	for key := range m.pendingCreates {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	// Restore claims are admitted under m.mu. Count them even while an archived
	// row has not yet transitioned to Lost+OpRestoring; the matching restore
	// admission checks projectDeletes under this same lock. Other lifecycle ops
	// retain DeleteProject's established per-session partial-failure behavior.
	for key := range m.restoresInFlight {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	for key, inst := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		if inFlightOnly && inst.GetInFlightOp() == session.OpNone {
			continue
		}
		titleSet[title] = struct{}{}
	}
	titles := make([]string, 0, len(titleSet))
	for title := range titleSet {
		titles = append(titles, title)
	}
	return titles
}

// deregisterRootAgents is the durable root_agents removal DeleteProject runs. A
// package var so tests can force a persist failure in isolation (exercising the
// #1740-review fatal-on-config-failure path) without disturbing the real config.
var deregisterRootAgents = config.DeregisterRootAgentsForRepo

// deleteProjectPreSweepHookForTest, when non-nil, runs immediately before the
// durable root_agents sweep decides which identities to remove. That window —
// after the selectors and the claimant were resolved, before the locked config
// mutation — is where a checkout appearing at an absent recorded root makes the
// path's hash somebody else's (#3530 review id 3917445672), and nothing else
// can hold it open: in a real daemon the two are microseconds apart.
var deleteProjectPreSweepHookForTest func()

// DeleteProjectResult reports what DeleteProject did so the control server can
// publish one archived/killed event per affected session (plus a
// projects-changed signal), and the CLI/TUI can report the counts.
type DeleteProjectResult struct {
	RepoID string
	// Archived carries the full committed projection of every archived session so
	// lifecycle events are directly applicable by clients (#2680). Killed needs only
	// {ID, Title} for every in-place/external session torn down instead (archive can't
	// relocate an external worktree; its kill never touches the user's tree/branch).
	Archived []session.InstanceData
	Killed   []session.InstanceData
	// Warnings carries failed on-archive hooks for sessions whose archive still
	// committed. They do not block the remaining sessions or project
	// deregistration, but every caller must surface them as a committed outcome.
	Warnings []string
	// Deregistered is true when this delete removed the repo's durable #2355 registry
	// record (#2456). It is what lets the projects-changed signal fire for a
	// registered project with NO live sessions — otherwise a delete that archived
	// nothing would publish nothing and the registered project would linger.
	Deregistered bool
}

func deleteProjectFailure(result DeleteProjectResult, err error) error {
	if len(result.Warnings) == 0 {
		return err
	}
	return fmt.Errorf("%w; warning(s) from archive(s) that already committed: %s",
		err, strings.Join(result.Warnings, "\n"))
}

// normalizeDeleteProjectPath resolves a delete's path selector to the root and
// identity it addresses.
//
// A path that RESOLVES is its repository's, plainly. A path that does not is
// the case #3363 was about, and the answer is now written down rather than
// guessed: if a registered project last resolved from that path, the delete
// addresses THAT project's own identity — the one its sessions and root agent
// are keyed under — and never whatever repository may occupy the path later.
// Hashing the path was what made those two indistinguishable (#3530).
//
// With no such record, the identity is invented and can match nothing, which is
// the clean idempotent no-op deleting an unknown project must be. That comment
// used to be aspirational; the namespace split makes it true.
func normalizeDeleteProjectPath(path string) (string, string, error) {
	root := config.ExpandTilde(strings.TrimSpace(path))
	repo, err := config.RepoFromPath(root)
	if err == nil {
		return repo.Root, repo.ID, nil
	}
	if errors.Is(err, config.ErrRepoProbeUnanswered) {
		// git never answered, so this is not evidence that the path is
		// unresolved — and choosing EITHER identity here picks a project. A
		// stale record's id would archive and suppress the old project while
		// the user selected the checkout occupying its path (#3530 review id
		// 3914971755). Refuse; nothing has been mutated (#3500's rule).
		return "", "", fmt.Errorf("delete project: could not determine what is at %q — git never answered the probe, so which project this names is unknown; nothing was changed — retry once the path is readable: %w", root, err)
	}
	root = filepath.Clean(root)
	projects, listErr := config.ListProjects()
	if listErr != nil {
		// Unknown, not "no such record" (#3530 review id 3914971766). Falling
		// through would invent an id that matches nothing and report a
		// successful no-op while the project's sessions and registration are
		// untouched.
		return "", "", fmt.Errorf("delete project: could not read the durable project registry to identify %q; nothing was changed: %w", root, listErr)
	}
	// Canonically, not lexically (#3530 review id 3918120745). The request may
	// spell the path through a symlinked ancestor while the record stores what
	// registration resolved, and a lexical miss here hands the delete an
	// invented id: success reported, the real id's sessions and the durable
	// registration untouched. The matched record's OWN root is returned, so
	// every downstream comparison — the claimant scan, the opt-in sweep —
	// works from the spelling the registry uses.
	target := pathutil.ResolveForCompare(root)
	for _, project := range projects {
		if project.RepoID == "" {
			continue
		}
		if pathutil.ResolveForCompare(filepath.Clean(project.Root)) == target {
			return filepath.Clean(project.Root), project.RepoID, nil
		}
	}
	return root, config.DerivedRepoIDForUnresolvedRoot(root), nil
}

// refuseIfIdentityInTransition stops a delete whose target identity the daemon
// is in the middle of deciding (#3530 review ids 3915722493, 3916379586,
// 3917445659).
//
// A record written before Project.RepoID is addressed by an invented id while
// its path does not resolve, and a reconciled record whose recorded root is a
// linked workspace can be filed under a REAL id its checkout no longer resolves
// to. Either way, "which project does this id name" has two answers for as long
// as a probe holds an unconsumed candidate — and a delete that picks one
// archives sessions, tears down worktrees and deregisters a record, none of
// which a verdict arriving a moment later can undo.
//
// So it refuses, and says when to retry. An earlier round redirected instead,
// following the probe to the identity it had resolved; that keyed on an id,
// which a repository at a reused path can legitimately own too, so deleting
// such an occupant could aim at a stale record's project instead (3917445659).
// Refusing needs no cross-identity action at all, which is what makes it safe:
// the ambiguity is reported rather than resolved by guess, and the next ensure
// pass — which has the registry record in hand — completes the transition and
// makes the ordinary path correct.
//
// rowFound says the request already selected a registry row. With one in hand
// nothing is ambiguous: the row IS the project being deleted, whatever else may
// share its identity.
func (m *Manager) refuseIfIdentityInTransition(repoID string, rowFound bool) error {
	pending := m.identityTransitionPendingFor(repoID)
	if !pending && !rowFound {
		pending = m.identityTransitionPendingOn(repoID)
	}
	if !pending {
		return nil
	}
	return fmt.Errorf("delete project: af is still establishing which project %s names — a registered project's recorded checkout is present but its identity is not yet verified — so nothing was changed; delete again once that check settles, or, if that checkout is not the project's and is not coming back, remove the path and delete again", repoID)
}

// registeredProjectRootForRepoID resolves the path needed by// registeredProjectRootForRepoID resolves the path needed by
// config.DeregisterProject when a direct client supplies only the daemon's
// repo identity. Registry read failure is an unknown outcome, not evidence that
// no matching registration exists, so callers must treat an error as fatal
// before mutating sessions or root-agent state.
func registeredProjectRootForRepoID(repoID string) (string, error) {
	projects, err := config.ListProjects()
	if err != nil {
		return "", fmt.Errorf("read durable project registry: %w", err)
	}
	var root string
	for _, project := range projects {
		// A reachable registered worktree resolves through Git so a bare
		// repository's linked workspace matches the bare-derived repo ID. A
		// missing path retains its recorded-root fallback instead of being
		// guessed into a different project.
		candidateID := ""
		if resolvedID, ok := config.ResolveRegisteredProjectRepoID(context.Background(), project); ok {
			candidateID = resolvedID
		} else if !project.PathExists {
			// Only a determinately absent path falls back. It falls back to the
			// identity the record WROTE DOWN, so an absent project is still
			// addressable as itself (#3530/#3363); a record predating that
			// field gets an invented id that matches nothing rather than
			// matching a stranger. A present replacement without this record's
			// checkout marker is positive evidence that it is not the
			// registered checkout, so it does not fall back at all.
			candidateID = config.ReconciledRepoIDForProject(project)
		}
		if candidateID != repoID {
			continue
		}
		if root != "" && filepath.Clean(root) != filepath.Clean(project.Root) {
			return "", fmt.Errorf("repo id %s matches multiple registered roots %q and %q; cannot determine which project to deregister", repoID, root, project.Root)
		}
		root = project.Root
	}
	return root, nil
}

// DeleteProject deletes a project — a repo grouping of sessions (#1735) — with
// archive-then-remove semantics under the single-writer daemon:
//
//   - Every LIVE session of the repo is ARCHIVED (tmux torn down, worktree moved
//     to the archive dir, branch + state preserved) so it stays restorable via
//     RestoreArchived. Already-archived rows are left untouched — they are the
//     restorable state this delete preserves.
//   - An in-place/external worktree session (the always-on root agent, or an
//     `af sessions create --here` session) cannot be archived — archive relocates
//     the worktree, unsupported for the user's own checkout — so it is torn down
//     instead. That teardown never touches the user's tree or branch (#1107).
//   - The repo's root_agents opt-in is dropped (in-memory suppression for this
//     daemon's life + removed from config on disk) so the project does not linger
//     empty in the picker and no always-on root respawns.
//   - A durable project registration whose root matches RepoPath — or whose root
//     the daemon resolves from a RepoID-only request — is removed after every
//     session settles, so a registered-but-sessionless project also disappears.
//
// The user's real git repository is never touched. Because the active-projects
// list is derived from LIVE sessions, archiving them all removes the project from
// it; restoring any archived session makes the repo active again, but does not
// restore its durable registration or root_agents opt-in. Idempotent: deleting an
// unknown project archives nothing, drops no opt-in or registration, and returns
// a zero-count success; a registered project with no sessions is deregistered.
func (m *Manager) DeleteProject(req DeleteProjectRequest) (DeleteProjectResult, error) {
	target, err := m.resolveDeleteProjectTarget(req)
	if err != nil {
		return DeleteProjectResult{RepoID: target.repoID}, err
	}
	m.taskTargetMu.Lock()
	defer m.taskTargetMu.Unlock()
	return m.deleteProject(target)
}

type deleteProjectTarget struct {
	repoID   string
	repoPath string
	// claimantProjectID names the registry record whose root is repoPath, so a
	// suppression tombstone records WHOSE delete it was and a later proven
	// mismatch releases it only for that claimant (#3299 review round 15). It
	// is resolved here rather than at suppression time because finding it
	// stats every recorded root, and #3361's whole point is that a stalled
	// mount must not be walked while taskTargetMu is held. Best-effort: a
	// registry read failure leaves it "" — an unreleasable tombstone, the
	// conservative direction.
	claimantProjectID string
}

// claimantForRecord decides whether a registry row may be recorded as the
// claimant of the tombstone this delete is about to install, and answers with
// POSITIVE evidence only (#3299 review id 3908667983).
//
// Matching a row by PATHNAME never made the checkout being deleted that row's
// project, and an earlier version of this guard only excluded a mismatch the
// snapshot had ALREADY proven. That is absence of evidence standing in for
// evidence: an unrelated clone appearing and being path-deleted before its
// probe published anything — or while its marker is unreadable — still claimed
// the stale row. The retained probe then finishes with identityMismatch, which
// rootDeletionTombstoneApplies reads as disproof of the tombstone this delete
// just installed, and the immutable in-memory legacy entry recreates the root
// it tore down. A delete that undoes itself.
//
// Two things count as evidence, and nothing else does:
//
//   - the checkout at the recorded root carries this record's marker, so it
//     provably IS this project;
//   - the recorded root is determinately ABSENT, so no checkout contradicts
//     the record and it remains the only claimant of that path. This case is
//     load-bearing rather than permissive: deleting a project while its path
//     is unavailable is the ordinary shape here, and it is what lets a later
//     occupant's proven mismatch release the tombstone (#3299 review round 15).
//
// A present path whose marker mismatches, cannot be read, or cannot be probed
// leaves the claimant empty — an unreleasable tombstone, which is the
// conservative direction.
func claimantForRecord(p config.Project) string {
	// Absence is checked FIRST, and the order is the point (#3299 review id
	// 3911002415). Both observations can report "no marker here", but only one
	// of them is evidence FOR the record: an absent root means nothing
	// contradicts it, while a present checkout whose marker does not match
	// positively DISPROVES it. Asking the marker first let a disproof be
	// overturned by an occupant that vanished a moment later, and the claim
	// that followed kept repoPath alive — so the delete could deregister the
	// original project's row on the strength of a checkout already shown not
	// to be it.
	if absent, err := recordRootAbsent(p.Root); err == nil && absent {
		return p.ID
	}
	if matches, err := config.ProjectCheckoutMatches(p.Root, p.CheckoutID); err == nil && matches {
		return p.ID
	}
	return ""
}

// resolveDeleteProjectTarget performs every filesystem/Git selector probe
// before DeleteProject takes taskTargetMu. A stale registry root must not hold
// the cross-task lifecycle lock while Git waits on an unrelated mount.
//
// It is a method because re-attribution aliasing (#3299) has to be resolved
// with those probes rather than after them: the alias decides WHICH identity
// the delete targets, and the occupant check that guards it is itself a Git
// probe. m.rootAgentLayers is an atomic pointer, so reading the snapshot here
// takes no lock and keeps #3361's pre-lock boundary intact.
func (m *Manager) resolveDeleteProjectTarget(req DeleteProjectRequest) (deleteProjectTarget, error) {
	repoID := strings.TrimSpace(req.RepoID)
	repoPath := strings.TrimSpace(req.RepoPath)
	if repoID == "" {
		if repoPath == "" {
			return deleteProjectTarget{}, fmt.Errorf("delete project: repo_id or repo_path is required")
		}
		// Expand a leading ~ BEFORE canonicalizing: git (RepoFromPath) does not do
		// tilde expansion — the shell normally would — so a literal "~/repo" request
		// (a root_agents key spelling, or a hand-typed CLI arg) must be expanded here
		// or it resolves to nothing and silently misses (#1740 review).
		// Canonicalize the path the SAME way the daemon keys repos everywhere:
		// resolve it to the main-repo root (git toplevel) and hash THAT, so a
		// symlinked / trailing-slash / relative / subdirectory / ".."-laden form of
		// a REAL project still resolves to its sessions' repo id — never a silent
		// miss. Only when the path does not resolve to a git repo (a moved/removed/
		// typo'd project) fall back to hashing the cleaned path: that yields no
		// match, which is the clean idempotent no-op deleting an unknown project
		// must be (Sachin's locked semantics), not a wrong-project hit.
		var normErr error
		repoPath, repoID, normErr = normalizeDeleteProjectPath(repoPath)
		if normErr != nil {
			return deleteProjectTarget{}, normErr
		}
	} else if repoPath == "" {
		var err error
		repoPath, err = registeredProjectRootForRepoID(repoID)
		if err != nil {
			return deleteProjectTarget{repoID: repoID}, fmt.Errorf("delete project %s: could not determine its registered root from repo_id; nothing was changed: %w", repoID, err)
		}
	} else {
		// Both selectors must describe one project. Otherwise RepoID chooses the
		// sessions/root-agent state while RepoPath chooses a potentially different
		// registry row — a split-target partial delete hidden behind success.
		var pathRepoID string
		var normErr error
		repoPath, pathRepoID, normErr = normalizeDeleteProjectPath(repoPath)
		if normErr != nil {
			return deleteProjectTarget{repoID: repoID}, normErr
		}
		if pathRepoID != repoID {
			return deleteProjectTarget{repoID: repoID}, fmt.Errorf("delete project: repo_id %s does not match repo_path %q (repo id %s); nothing was changed", repoID, repoPath, pathRepoID)
		}
		// A bare session persists its identity directory as repo_path, while the
		// durable project record names the linked workspace that was registered.
		// Once both selectors prove the same repo, use that registered root for
		// deregistration just as the repo-ID-only form does.
		registeredRoot, err := registeredProjectRootForRepoID(repoID)
		if err != nil {
			return deleteProjectTarget{repoID: repoID}, fmt.Errorf("delete project %s: could not determine its registered root after validating repo_path; nothing was changed: %w", repoID, err)
		}
		if registeredRoot != "" {
			repoPath = registeredRoot
		}
	}
	// LAST, after every registry lookup above and after the two selectors have
	// been checked against each other: those establish whether this request
	// selected a project at all, which is what decides whether the identity is
	// ambiguous (#3530 review id 3915722493). Nothing has been mutated yet, so
	// a refusal here is literally "nothing was changed".
	if err := m.refuseIfIdentityInTransition(repoID, repoPath != ""); err != nil {
		return deleteProjectTarget{repoID: repoID}, err
	}
	claimantProjectID := ""
	if repoPath != "" {
		if projects, _, _, _, err := config.ListProjectsDetailed(); err == nil {
			for _, p := range projects {
				// Canonical for the same reason normalizeDeleteProjectPath is
				// (#3530 review id 3918120745): a record whose root is spelled
				// through a symlink must still be recognised as the row this
				// delete selected, or its tombstone goes unclaimed.
				if pathutil.ResolveForCompare(filepath.Clean(p.Root)) != pathutil.ResolveForCompare(filepath.Clean(repoPath)) {
					continue
				}
				claimantProjectID = claimantForRecord(p)
				if claimantProjectID == "" {
					// The row matches this path by NAME, but nothing proves the
					// checkout being deleted is that row's project — an
					// unrelated clone at an unresolved project's old path,
					// deleted before attribution has a verdict. Deregistering
					// on a pathname match alone would destroy the original
					// project's registry directory and its personal config on
					// behalf of a delete that never targeted it (#3299 review
					// id 3910519845). Drop the path: the delete still tears
					// down and suppresses the occupant, and the stale record
					// stays for its own owner to resolve or remove.
					repoPath = ""
				}
				break
			}
		}
	}
	return deleteProjectTarget{
		repoID:            repoID,
		repoPath:          repoPath,
		claimantProjectID: claimantProjectID,
	}, nil
}

// deleteProject performs DeleteProject while taskTargetMu is already held from
// blocker preflight through the final session mutation.
func (m *Manager) deleteProject(resolved deleteProjectTarget) (DeleteProjectResult, error) {
	repoID := resolved.repoID
	repoPath := resolved.repoPath
	result := DeleteProjectResult{RepoID: repoID}

	// FAIL CLOSED, before mutating ANY state, if a session for this repo is still being
	// created (#2549). A create lives in m.pendingCreates through its slow provisioning
	// and only joins m.instances at the end (manager_create.go), and ArchiveSession
	// refuses a session with an in-flight op — so such a session can neither be seen by
	// an m.instances snapshot nor archived. Rather than block the caller on an arbitrary
	// clock (a git clone / image pull settles in minutes, not the seconds a bounded wait
	// could afford), refuse immediately and name the session: the create settles on its
	// own and a retry then removes the project cleanly. Because this runs before the
	// durable removals below, "nothing was changed" is literally true.
	m.mu.Lock()
	starting := m.repoSessionTitlesLocked(repoID, true)
	if len(starting) == 0 {
		if m.projectDeletes == nil {
			m.projectDeletes = make(map[string]struct{})
		}
		m.projectDeletes[repoID] = struct{}{}
		m.stampProjectDeleteLocked(repoID)
	}
	m.mu.Unlock()
	if len(starting) > 0 {
		sort.Strings(starting)
		return result, fmt.Errorf("delete project %s: session(s) %v are still starting or changing; nothing was changed — delete again once their operations finish", repoID, starting)
	}
	defer func() {
		m.mu.Lock()
		delete(m.projectDeletes, repoID)
		// Stamp the REMOVE too, not only the install above. A create that began
		// before this delete did samples a counter value below the install's stamp,
		// so the install alone already covers it — but a create that began while
		// this delete was ALREADY running samples above the install and would see
		// nothing move. Stamping here puts the transition after that sample, which
		// is what makes an in-progress delete visible to a create that could not
		// have seen its start (#2947).
		m.stampProjectDeleteLocked(repoID)
		m.mu.Unlock()
	}()

	// A task-target refusal is predictable and must precede BOTH stores this
	// operation mutates. Discovering it inside the later archive loop used to
	// remove root_agents and suppress respawn before returning the error. The
	// caller then had neither a deleted project nor its previous lifecycle
	// configuration. taskTargetMu keeps this preflight stable through the loop.
	taskTargets, err := m.preflightDeleteProjectTaskTargets(repoID)
	if err != nil {
		return result, err
	}

	// Durably drop the repo's root_agents opt-in FIRST, before mutating ANY state,
	// and treat a failed write as FATAL to the whole delete (#1740 review): if this
	// removal fails, a daemon restart would re-register the root and the project
	// would REAPPEAR — so reporting success would be a lie. Persisting before any
	// in-memory change also keeps the failure ATOMIC: on error nothing is archived
	// AND no in-memory suppression is left behind, so a failed delete leaves the
	// project fully intact. A project with no opt-in is a nil, nil no-op.
	// BOTH identities in ONE durable mutation (#3299 review rounds 6-7): a
	// re-attributed project's legacy key spelled as the unavailable recorded
	// path matches only its derived hash, and two separate writes could
	// remove one opt-in and then fail — falsifying the nothing-was-changed
	// guarantee below.
	// root_agents keys are PATHS, and the matcher resolves them to a canonical
	// identity — so a delete targeting a provisional id would sweep nothing and
	// leave the durable opt-in to start the root again on the next daemon run
	// (#3530 review id 3914971851).
	//
	// The PATH decides this, not the identity (#3530 review ids 3916379565,
	// 3916757161). A root_agents key is a path, and rootAgentKeyMatchesRepo
	// falls back to hashing one it cannot resolve — so a stale key spelled as
	// this project's recorded root answers to that path's hash and to nothing
	// else, whenever the recorded root is not its repository's identity root.
	// Master swept it by accident, because it addressed such a project BY that
	// hash; addressing the project as itself is what made the sweep have to say
	// so out loud.
	//
	// And the hash is supplied ONLY while the recorded root does not resolve
	// (review id 3917445672). That is not an optimisation: a repository
	// appearing there legitimately OWNS that id, so supplying it once a
	// checkout is present would sweep the occupant's own opt-in on behalf of a
	// delete that targeted this project. The check runs here, immediately
	// before the locked write, rather than at selector time — as late as it can
	// be, though check-then-act cannot be made atomic and a checkout appearing
	// inside that window is the residue this states rather than hides.
	if deleteProjectPreSweepHookForTest != nil {
		deleteProjectPreSweepHookForTest()
	}
	optInIDs := []string{repoID}
	if repoPath != "" {
		if pathID := config.RepoIDFromRoot(filepath.Clean(repoPath)); pathID != repoID {
			if _, err := config.RepoFromPath(repoPath); err != nil {
				optInIDs = append(optInIDs, pathID)
			}
		}
	}
	if removed, cfgErr := deregisterRootAgents(optInIDs...); cfgErr != nil {
		return result, fmt.Errorf("delete project %s: could not durably remove its root_agents opt-in — the project would reappear on daemon restart, so nothing was changed; retry: %w", repoID, cfgErr)
	} else if len(removed) > 0 {
		log.InfoLog.Printf("delete project %s: removed %d root_agents opt-in(s): %v", repoID, len(removed), removed)
	}

	// The repo's durable #2355 registry record is dropped LATER, only after every
	// session it promised to archive is actually archived (#2549): deregistering here,
	// before the archive, is what let a session still finishing its create survive as a
	// live orphan in a repo no longer in the registry. See the archive loop below.

	// The durable removal succeeded, so now apply the in-memory suppression that
	// stops the ensure loop re-creating the always-on root (m.cfg is immutable
	// after start). Doing it before the teardown guarantees no poll tick respawns
	// the root we are about to tear down; doing it only AFTER the persist means a
	// failed write above never leaves a dangling suppression (#1740 review).
	// The tombstone records WHOSE delete this was, so a later proven
	// mismatch can release it only for the deleted claimant itself (#3299
	// review round 15). Resolved before the lock — see deleteProjectTarget.
	m.suppressRootAgent(repoID, resolved.claimantProjectID)
	// A re-attribution probe for the deleted project is deliberately LEFT
	// RUNNING (#3299 review rounds 9-11, converged): while its marker read
	// is unfinished, the attribution-pending gate fails the candidate repo
	// closed — which doubles as this delete's suppression — and its eventual
	// completion publishes the reattributedFrom alias that carries the
	// tombstones above to the real identity. Retiring or exempting the probe
	// (both tried) opened a window where an in-memory legacy entry could
	// resurrect the deleted root before the alias existed.

	// Archive/kill every live session for the repo BEFORE removing its durable identity,
	// so no session outlives the project's registry entry (#2549). The phase-1 gate above
	// already refused if any session was mid-create, so every live session here is settled
	// and archivable. Acting with m.mu released — ArchiveSession/KillSession take their own
	// per-session locks and would deadlock under it.
	type target struct {
		id       string
		title    string
		external bool
	}
	var targets []target
	m.mu.Lock()
	for key, inst := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		targets = append(targets, target{id: inst.ID, title: title, external: inst.IsExternalWorktree()})
	}
	m.mu.Unlock()

	// Deterministic order so a partial failure + retry is stable and the logs read
	// consistently.
	sort.Slice(targets, func(i, j int) bool { return targets[i].title < targets[j].title })

	var errs []error
	for _, t := range targets {
		if t.external {
			// Carry the stable identity captured under m.mu into the destructive
			// lookup. Besides making a concurrent completed kill distinguishable,
			// this prevents a same-title replacement from being torn down in the
			// original target's place. Legacy id-less rows retain title lookup.
			killed, err := m.KillSession(KillSessionRequest{ID: t.id, Title: t.title, RepoID: repoID})
			if errors.Is(err, errSessionNotFound) {
				// This is idempotent success only because t came from DeleteProject's
				// own under-lock snapshot: the target existed then and is gone now,
				// which is precisely the requested end state (#2124). A standalone
				// KillSession for a stale/never-existing target still returns the
				// same not-found error. Report the snapshotted identity so counts and
				// lifecycle events do not understate what the delete achieved.
				killed = session.InstanceData{ID: t.id, Title: t.title}
				err = nil
			}
			if isMutationCommitted(err) {
				// A committed kill is durable but NOT finished: the tombstoned row
				// was retained, so at the project level this session genuinely was
				// not removed and the delete must stay a retryable failure. Flatten
				// the marker before joining — errors.As walks the join, so a
				// committed member would make the whole project-delete failure read
				// as mutation_committed on the HTTP envelope, and a delete that
				// never deregistered would print as ok-with-warning (#3234).
				err = errors.New(err.Error())
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("session %q: %w", t.title, err))
				continue
			}
			result.Killed = append(result.Killed, session.InstanceData{ID: killed.ID, Title: killed.Title})
			continue
		}
		// Carry the same snapshotted stable identity into archive that the
		// external-session kill path above carries into KillSession. A title-only
		// lookup here can resolve a NEW same-title session created after the
		// snapshot and archive work the user never confirmed deleting.
		_, archived, err := m.archiveSession(ArchiveSessionRequest{ID: t.id, Title: t.title, RepoID: repoID}, taskTargets)
		if errors.Is(err, errSessionNotFound) {
			// Snapshot-gated idempotency, exactly like the kill path: this target
			// existed under m.mu and is now authoritatively absent by stable ID, so
			// the original is already gone. Do not infer anything about a possible
			// replacement; the live-session re-check below observes it separately
			// and keeps the project registered for a fresh delete decision.
			archived = session.InstanceData{ID: t.id, Title: t.title}
			err = nil
		}
		// A target that is ALREADY archived is idempotent SUCCESS, not a failure
		// (#2108). The snapshot above is a point-in-time read taken under m.mu and
		// then acted on with the lock released, so a concurrent ArchiveSession can
		// land in that window and leave a target in exactly the state this delete
		// wants it in. Counting that as a failure returned a partial-failure error,
		// omitted the session from Archived (an undercount), and pushed the TUI down
		// the error path — all for a session that IS archived. ArchiveSession returns
		// the resolved identity alongside the sentinel, so the row is reported with
		// its real {id, title}. Every OTHER error stays a failure: a busy session, an
		// op in flight, or a teardown that broke is genuinely not archived, and the
		// caller must be told to retry.
		if errors.Is(err, ErrAlreadyArchived) {
			err = nil
		}
		if isMutationCommitted(err) {
			result.Warnings = append(result.Warnings, err.Error())
			err = nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", t.title, err))
			continue
		}
		result.Archived = append(result.Archived, archived)
	}

	if len(errs) > 0 {
		// A genuine teardown failure — including a session that raced into an in-flight op
		// after the phase-1 gate and so refused to archive. The delete is idempotent, so a
		// retry finishes the rest; the registry is NOT deregistered below, so no orphan is
		// left behind.
		return result, deleteProjectFailure(result, fmt.Errorf(
			"delete project %s: archived %d, tore down %d, but %d session(s) could not be removed (retry to finish): %w",
			repoID, len(result.Archived), len(result.Killed), len(errs), errors.Join(errs...)))
	}

	// Concurrent-create guard (#2549): re-check under m.mu, immediately before the
	// deregister, that no session appeared for the repo while the lock was released for the
	// archive above. A create that BEGINS after the phase-1 gate — landing in
	// m.pendingCreates, or completing into m.instances — would otherwise orphan the same
	// way through a narrower window. If one appeared, abort WITHOUT deregistering: the
	// sessions archived so far stay archived (idempotent), and a retry finishes once the
	// new session settles. A create landing in the microseconds between this check and the
	// DeregisterProject call below is the one remaining, deliberately documented window;
	// closing it fully would mean holding m.mu across the registry file lock (an ABBA
	// hazard) for a gap this narrow.
	m.mu.Lock()
	appeared := m.repoSessionTitlesLocked(repoID, false)
	m.mu.Unlock()
	if len(appeared) > 0 {
		sort.Strings(appeared)
		return result, deleteProjectFailure(result, fmt.Errorf(
			"delete project %s: archived %d, tore down %d, but a session started for this project meanwhile (%v); the project was NOT removed — delete again to finish",
			repoID, len(result.Archived), len(result.Killed), appeared))
	}

	// Every session for the repo is archived and none is starting — NOW drop the durable
	// #2355 registry record (after the archive, #2549): the deregister must never land
	// while a session survives, or the delete leaves a live orphan in a repo no longer in
	// the registry. repoPath is either caller-provided and canonicalized, or resolved from
	// the daemon's durable registry for the documented RepoID-only form. An empty root
	// means no matching registration existed at resolution time, so there is nothing to
	// deregister. Recorded so the projects-changed publish fires even when no session was
	// archived (a registered, sessionless project).
	if repoPath != "" {
		deregistered, regErr := config.DeregisterProject(repoPath)
		if regErr != nil {
			// SYMMETRIC to an archive failure, and NOT "nothing changed": the sessions are
			// already archived here, so a retry re-archives nothing and finishes the deregister
			// — at worst an empty-but-registered project, never a live orphan.
			return result, deleteProjectFailure(result, fmt.Errorf(
				"delete project %s: archived %d session(s) but could not remove its durable registry record — the project still lists; retry to finish, a retry re-archives nothing: %w",
				repoID, len(result.Archived), regErr))
		}
		if deregistered {
			result.Deregistered = true
			log.InfoLog.Printf("delete project %s: removed its durable registry record", repoID)
		}
	}

	log.InfoLog.Printf("deleted project %s: archived %d session(s), tore down %d in-place session(s)", repoID, len(result.Archived), len(result.Killed))
	return result, nil
}

func (m *Manager) preflightDeleteProjectTaskTargets(repoID string) (map[string][]task.Task, error) {
	m.mu.Lock()
	var titles []string
	hasRoot := false
	for key, instance := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || instance == nil || instance.GetLiveness() == session.LiveArchived {
			continue
		}
		titles = append(titles, title)
		hasRoot = hasRoot || title == session.RootSessionTitle
	}
	m.mu.Unlock()
	// An enabled root is part of the project's effective live-session set even
	// while its process is momentarily absent: the ensure loop will recreate it,
	// and task delivery waits for that same promise. Include the reserved target
	// before the empty-roster return, or DeleteProject can suppress re-creation
	// and strand a root-targeted task behind a title ordinary auto-create refuses.
	// For this consumer UNKNOWN behaves like yes (#3264): a fail-closed repo
	// (unloadable personal config, unlistable registry) reports
	// willMaterialize=false, but deleting through that answer would drop the
	// root_agents opt-in and leave the enabled task stranded the moment the
	// config becomes readable again — the exact hazard this preflight refuses.
	if !hasRoot {
		switch m.rootAgentMaterializeVerdictFor(repoID).reason {
		case rootAgentWillMaterialize, rootAgentRegistryUnreadable, rootAgentPersonalUnreadable, rootAgentProjectUnresolved, rootAgentRecordsUnreadable, rootAgentAttributionPending:
			titles = append(titles, session.RootSessionTitle)
		}
	}
	sort.Strings(titles)
	if len(titles) == 0 {
		return make(map[string][]task.Task), nil
	}

	taskTargets, err := m.loadEnabledTaskTargets(repoID)
	if err != nil {
		return nil, fmt.Errorf("delete project %s: could not determine whether enabled tasks target its sessions; nothing was changed: %w", repoID, err)
	}

	var blockers []string
	for _, title := range titles {
		targeted := taskTargets[title]
		if len(targeted) > 0 {
			blockers = append(blockers, fmt.Sprintf("session %q: %s", title, describeTargetTasks(targeted)))
		}
	}
	if len(blockers) > 0 {
		return nil, fmt.Errorf("delete project %s: enabled task(s) target session(s) it must remove: %s; disable or retarget them, then delete the project again; nothing was changed", repoID, strings.Join(blockers, "; "))
	}
	return taskTargets, nil
}

// suppressRootAgent marks repoID's project as deleted for the rest of this
// daemon's life so the ensure loop stops (re-)creating its always-on root agent,
// and clears the kill-grace record so no stale grace window survives (#1735). The
// ensure loop is keyed by config path, not repoID, so the deletedRootRepos check
// (which resolves each path to its repoID) is where suppression takes effect.
func (m *Manager) suppressRootAgent(repoID, claimantProjectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedRootRepos[repoID] = claimantProjectID
	delete(m.rootKilledAt, repoID)
}
