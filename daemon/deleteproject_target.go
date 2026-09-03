package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// Deciding WHICH project a delete names, before anything is mutated (#3363,
// #3530). Every Git probe and registry read a delete needs happens here, ahead
// of taskTargetMu (#3361), and every refusal it can reach is a refusal that has
// changed nothing. deleteproject.go executes the target this produces.

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
func normalizeDeleteProjectPath(path string) (root string, repoID string, matchedRecord bool, err error) {
	root = config.ExpandTilde(strings.TrimSpace(path))
	repo, err := config.RepoFromPath(root)
	if err == nil {
		return repo.Root, repo.ID, false, nil
	}
	// Absence is INDEPENDENT evidence, so it is asked first (#3530 review id
	// 3919749878). A path that provably holds nothing cannot be hiding a
	// checkout whichever way git failed, and refusing there would make a
	// sessionless missing project undeletable for as long as the git
	// executable is unavailable.
	if !config.PathIsDeterminatelyFree(root, err) {
		if errors.Is(err, config.ErrRepoProbeUnanswered) {
			// git never answered, so this is not evidence that the path is
			// unresolved — and choosing EITHER identity here picks a project.
			// A stale record's id would archive and suppress the old project
			// while the user selected the checkout occupying its path (#3530
			// review id 3914971755). Refuse; nothing has been mutated
			// (#3500's rule).
			return "", "", false, fmt.Errorf("delete project: could not determine what is at %q — git never answered the probe, so which project this names is unknown; nothing was changed — retry once the path is readable: %w", root, err)
		}
		// "git failed" is not "nothing is there" (#3530 review id 3919346198).
		// A live checkout whose metadata git will not read — dubious
		// ownership, an unreadable .git, a permission error — exits normally,
		// and treating that as unresolved sends the delete into the registry
		// fallback: it would then archive and suppress a STALE row's project
		// instead of the checkout the user selected. Refuse; nothing has been
		// mutated.
		return "", "", false, fmt.Errorf("delete project: could not determine what is at %q — git could not read it, so which project this names is unknown; nothing was changed — repair the checkout's access or metadata and retry: %w", root, err)
	}
	root = filepath.Clean(root)
	projects, listErr := config.ListProjects()
	if listErr != nil {
		// Unknown, not "no such record" (#3530 review id 3914971766). Falling
		// through would invent an id that matches nothing and report a
		// successful no-op while the project's sessions and registration are
		// untouched.
		return "", "", false, fmt.Errorf("delete project: could not read the durable project registry to identify %q; nothing was changed: %w", root, listErr)
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
		if pathutil.ResolveForCompare(filepath.Clean(project.Root)) != target {
			continue
		}
		// The record's OWN spelling, whether or not it has recorded an
		// identity yet (#3530 review id 3918379019). A schema-v1 row has no
		// RepoID, and skipping it here dropped the match too: the delete fell
		// through to an id invented from the REQUEST's spelling, which
		// DeregisterProject then cannot reconcile with the stored one — two
		// missing paths cannot be SameFile — so the durable registration
		// survived a delete that reported success. Its provisional id is
		// derived from the stored root for the same reason.
		recorded := filepath.Clean(project.Root)
		if project.RepoID != "" {
			return recorded, project.RepoID, true, nil
		}
		return recorded, config.DerivedRepoIDForUnresolvedRoot(recorded), true, nil
	}
	return root, config.DerivedRepoIDForUnresolvedRoot(root), false, nil
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

// registeredProjectRootForRepoID resolves the path needed by
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
	// Set by any lookup that actually SELECTED a registry row, which is a
	// different question from "a path was supplied" (#3530 review id
	// 3919195012).
	registeredRootMatched := false
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
		repoPath, repoID, registeredRootMatched, normErr = normalizeDeleteProjectPath(repoPath)
		if normErr != nil {
			return deleteProjectTarget{}, normErr
		}
	} else if repoPath == "" {
		var err error
		repoPath, err = registeredProjectRootForRepoID(repoID)
		if err != nil {
			return deleteProjectTarget{repoID: repoID}, fmt.Errorf("delete project %s: could not determine its registered root from repo_id; nothing was changed: %w", repoID, err)
		}
		registeredRootMatched = repoPath != ""
	} else {
		// Both selectors must describe one project. Otherwise RepoID chooses the
		// sessions/root-agent state while RepoPath chooses a potentially different
		// registry row — a split-target partial delete hidden behind success.
		var pathRepoID string
		var normErr error
		var pathMatchedRecord bool
		repoPath, pathRepoID, pathMatchedRecord, normErr = normalizeDeleteProjectPath(repoPath)
		if normErr != nil {
			return deleteProjectTarget{repoID: repoID}, normErr
		}
		registeredRootMatched = pathMatchedRecord
		// An UNREGISTERED project whose root no longer resolves is named by the
		// picker with the historical hash of that path, and both selectors are
		// sent (#3530 review id 3919346222). Normalization invents d-H for it,
		// and rejecting H against d-H made such a row undeletable — a
		// regression from master, where the two were the same value. With no
		// record at that path there is nothing to protect: no project claims H,
		// and H is exactly where the sessions and the opt-in the user can see
		// are filed. Only this exact pairing is honoured, and only when the
		// registry produced no row.
		if !pathMatchedRecord && pathRepoID != repoID &&
			pathRepoID == config.DerivedRepoIDForUnresolvedRoot(repoPath) &&
			repoID == config.RepoIDFromRoot(filepath.Clean(repoPath)) {
			pathRepoID = repoID
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
			registeredRootMatched = true
		}
	}
	claimantProjectID := ""
	// Whether a registry scan actually SELECTED a row, which is the condition
	// the transition gate is documented to want — path presence is not it
	// (#3530 review id 3919195012). A two-selector delete can name another
	// valid workspace of a repository whose unresolved row is mid-transition:
	// the path is non-empty, no row matches it, and treating that as "a row was
	// selected" skipped the reverse gate entirely.
	rowFound := registeredRootMatched
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
				rowFound = true
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
					//
					// And with the path goes the CLAIM that a row was selected
					// (#3530 review id 3919604370): this row is abandoned, so
					// leaving the flag set would tell the transition gate a
					// project had been selected and skip the reverse check —
					// letting the delete act on the candidate identity while
					// the unresolved row survives to reappear.
					repoPath = ""
					rowFound = false
				}
				break
			}
		}
	}
	if deleteProjectPostClaimantHookForTest != nil {
		deleteProjectPostClaimantHookForTest()
	}
	// Only when no row was selected, and only for a REAL identity. A request
	// that reached a record has its project in hand, and an invented id names
	// the record itself — both sides of the question #3638 answered. What is
	// left is the identity arriving alone, which is where a record predating
	// the write-down can neither be matched nor ruled out.
	//
	// HERE rather than inside registeredProjectRootForRepoID, which already
	// walked the registry: rowFound is not settled until the claimant scan
	// above has run, and it can be CLEARED there (an abandoned row drops the
	// path and the claim with it). Asking earlier would answer a different
	// question — "did a path match" rather than "was a project selected" —
	// which is the distinction #3530 review id 3919195012 drew.
	unattributed := config.Project{}
	if !rowFound && !config.IsDerivedRepoID(repoID) {
		record, found, err := unattributableLegacyRecord(repoID)
		if err != nil {
			return deleteProjectTarget{repoID: repoID}, fmt.Errorf("delete project %s: could not read the durable project registry to check whether a record predating repository identities may be this project; nothing was changed: %w", repoID, err)
		}
		if found {
			unattributed = record
		}
	}
	// LAST — after every registry lookup, after the two selectors have been
	// checked against each other, and after the claimant scan, because all of
	// those establish whether this request selected a project at all, which is
	// what decides whether the identity is ambiguous (#3530 review id
	// 3915722493). Nothing has been mutated yet, so a refusal here is literally
	// "nothing was changed".
	//
	// After the claimant scan specifically (#3530 review id 3917929613): that
	// scan reads the recorded checkout, so a checkout REAPPEARING during it
	// would otherwise be authorized by a marker match while the transition gate
	// had already opened on the absence observed before it. Checking last makes
	// the reappearance visible. It narrows the window rather than closing it —
	// check-then-act is not atomic — and what remains is bounded by the
	// delete's own fence.
	if err := m.refuseIfIdentityInTransition(repoID, rowFound); err != nil {
		return deleteProjectTarget{repoID: repoID}, err
	}
	return deleteProjectTarget{
		repoID:                    repoID,
		repoPath:                  repoPath,
		claimantProjectID:         claimantProjectID,
		unattributedRecordRoot:    unattributed.Root,
		unattributedRecordProject: unattributed.ID,
	}, nil
}

// boundedRecordRootAbsent is recordRootAbsent with that bound. A timeout
// reports NOT absent, which declines whatever the caller would have done on
// absence — the conservative direction every reader of this signal takes.
func boundedRecordRootAbsent(root string) (bool, error) {
	type answer struct {
		absent bool
		err    error
	}
	// Captured BEFORE the goroutine starts. Reading the package var inside it
	// races a test that swaps the probe, because the goroutine outlives the
	// call it was spawned for — which is the whole point of the bound.
	probe := recordRootAbsentProbe
	done := make(chan answer, 1)
	go func() {
		absent, err := probe(root)
		done <- answer{absent: absent, err: err}
	}()
	select {
	case a := <-done:
		return a.absent, a.err
	case <-time.After(boundedRecordRootAbsentTimeout):
		log.WarningLog.Printf("delete project: could not observe recorded root %s within %s; treating it as present, which declines the identity-matched deregistration this pass", root, boundedRecordRootAbsentTimeout)
		return false, nil
	}
}

// unattributableLegacyRecord names the registry record a delete BY IDENTITY may
// be the project of and cannot prove it is not (#3363's third window).
//
// #3638 writes down the identity a project resolved to, so an absent recorded
// path is addressed through what the record says rather than through a hash of
// the path — which would reach whatever repository occupies it now. A record
// written BEFORE that field has nothing to say, so it is addressed by an
// invented id that reaches nothing at all, deliberately.
//
// That answers the question asked from the PATH side and leaves the IDENTITY
// side of it open: a delete naming the real id its sessions were keyed under at
// creation finds no record, and the removal it then performs is a silent
// partial success — the sessions go, the durable registration stays, and the
// project returns on the next start. So the ambiguity is REPORTED. Nothing here
// resolves it: the two identities cannot be connected without the write-down
// that is missing, and guessing is what #3530 removed.
//
// The record has to be one this identity could plausibly have come from, or
// every stale row in the registry becomes a delete blocker. Three conditions,
// all of them properties of the record:
//
//   - it wrote down no identity, so nothing rules it in or out;
//   - its recorded root is determinately absent, so no checkout there can be
//     asked (a root that IS there resolves, or provably is not this record's,
//     and either way the ambiguity does not arise). The registry's own
//     PathExists, deliberately, and not recordRootAbsent: the two disagree
//     about a REGULAR FILE at the recorded root, and this predicate wants the
//     answer PathExists gives. The question here is "can a checkout be there",
//     not "is anything there" — a file proves no repository owns that hash, so
//     the record's claim to it stands unopposed and the ambiguity is real. The
//     other callers ask the other question, about a path they are deciding
//     whether to DEREGISTER;
//   - that root hashes to exactly this identity, which is what a repository
//     rooted there was keyed under. Every other legacy record is provably about
//     somewhere else.
//
// Records are enumerated in registry order, which is by project id, so the one
// reported is stable across calls; two records can only match by recording the
// same root.
//
// The absence observation is the registry's own, taken when it read the record,
// so a checkout arriving at that root a moment later is not seen here. Both ways
// that lands are the conservative one: a root that came back makes this report a
// record it no longer needs to, and the caller REFUSES, having changed nothing —
// the next delete finds the reconciled row by the ordinary path.
func unattributableLegacyRecord(repoID string) (config.Project, bool, error) {
	projects, err := config.ListProjects()
	if err != nil {
		// Unknown, not "no such record" — the registry's own rule. Falling
		// through would let the silent partial success this exists to stop
		// happen on a transient read failure.
		return config.Project{}, false, fmt.Errorf("read durable project registry: %w", err)
	}
	for _, project := range projects {
		if project.RepoID != "" || project.PathExists {
			continue
		}
		if config.RepoIDFromRoot(filepath.Clean(project.Root)) != repoID {
			continue
		}
		return project, true, nil
	}
	return config.Project{}, false, nil
}
