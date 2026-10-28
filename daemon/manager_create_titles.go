package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// Title admission for session creation (#1145 split out of manager_create.go,
// which crossed the 1000-line limit; the archived-record reuse half split on to
// manager_archived_rename.go). These helpers all run under the manager lock and
// answer one question between them: can this title be used — which held branch,
// live lane, or existing record blocks it — and which suffix resolves it.

// branchesHeldByWorktrees is a seam for the bounded Git branch-holder probe.
var branchesHeldByWorktrees = git.BranchesHeldByWorktrees

// worktreeBranchBindings is the evidence-bearing form used by --here admission,
// which must inspect the target's actual branch rather than derive one from its
// requested title.
var worktreeBranchBindings = git.WorktreeBranchBindings

func (m *Manager) worktreeHeldBranchesLocked(repoPath string, remote bool) map[string][]string {
	if remote {
		return nil
	}
	held, err := branchesHeldByWorktrees(repoPath)
	if err != nil {
		m.warn().Printf("could not list worktree branch holds for %s; resolving the session title without them (a name an archived worktree holds will fail at worktree add instead of being skipped): %v", repoPath, err)
		return nil
	}
	return held
}

func (m *Manager) branchForTitle(title string) string {
	return git.BranchForTitle(m.cfg.BranchPrefix, title)
}

// reserveCreate keeps the established unit-test seam free of a long-lived
// reservation. Production uses reserveCreateForSession and holds admission from
// its final observation through live-row publication.
func (m *Manager) reserveCreate(req CreateSessionRequest) (*config.RepoContext, string, func(), *session.InstanceData, error) {
	return m.reserveCreateWithWorktreeAdmission(req, false)
}

func (m *Manager) reserveCreateForSession(req CreateSessionRequest) (*config.RepoContext, string, func(), *session.InstanceData, error) {
	return m.reserveCreateWithWorktreeAdmission(req, true)
}

// refuseHeldBranchReuseLocked refuses an explicit-title create BEFORE the
// archived-name-reuse rename touches anything, when the branch the new session
// would derive is already checked out by a registered worktree (#2127). Runs
// under m.mu.
//
// renameArchivedForReuseLocked frees a TITLE. It does not free a BRANCH: per
// #2013 archiving relocates a worktree and repairs its registration rather than
// removing it, so the archived session keeps <prefix><title> checked out, and
// the new session derives that same branch. `git worktree add` then refuses it
// and the create fails — with the archived session already renamed out of the
// way for a create that never happened, the one state reserveCreate's own
// comment promises a refusal never leaves behind.
//
// So ask git first. This does not make reuse-archived-name WORK for a local
// session — only releasing or relocating the branch can, and that changes what
// happens to a user's branches, so it is a separate call tracked on #2127. What
// it does is turn a state-corrupting failure into an honest one that names the
// blocker and how to clear it.
//
// Three deliberate non-firings:
//
//   - No archived collision means renameArchivedForReuseLocked will not rename
//     anything, so there is no invariant to protect. The create proceeds and
//     fails at `git worktree add` exactly as before. Widening this into a
//     general "is the branch free" gate over every explicit title would refuse
//     creates that have nothing to do with this bug.
//   - Hook and --here creates never derive <prefix><title> at all: a hook
//     session takes no local worktree (backend_local is the only caller of
//     NewGitWorktree), and --here attaches to the repo's OWN working tree at ITS
//     current branch (NewGitWorktreeInPlace). A hold on a branch the create will
//     not use must not block it.
//   - A probe that could not RUN yields nil holds, and nil must never refuse.
//     "I could not ask git" is not "the branch is held"; treating it as one
//     would block a legitimate reuse on the strength of an answer git never
//     gave — the fabricated-negative failure this repo keeps paying for. On a
//     failed probe the create proceeds and, if the branch really is held, fails
//     loudly at `git worktree add`: precisely the pre-guard behavior, which
//     destroyed nothing. Only a branch git POSITIVELY reports as held refuses.
func (m *Manager) refuseHeldBranchReuseLocked(repoID, repoPath, title string, namespace runtimeNameNamespace, inPlace bool, diskData []session.InstanceData) error {
	if namespace != runtimeNamespaceLocalTmux || inPlace {
		return nil
	}
	archived, _, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, diskData)
	if err != nil {
		return err
	}
	if archived == nil {
		return nil
	}
	branch := m.branchForTitle(title)
	// Indexing a nil map is the nil-probe path: not held, so no refusal.
	holders := m.worktreeHeldBranchesLocked(repoPath, false)[branch]
	if len(holders) == 0 {
		return nil
	}
	// A holder that is NOT the archived session gets its own refusal, because the
	// generic one below would be false (#3404): it blames the archived session for
	// a branch some other worktree has checked out, sending the user to
	// `af sessions kill` on a session whose removal would not release anything.
	// The reclaim declines on the same fact, so branching here does not answer the
	// question differently — it only says out loud which worktree is in the way.
	for _, holder := range holders {
		if archivedWorktreeHoldsBranch(archived, holder) {
			continue
		}
		if lane := m.liveLaneHoldingWorktreeLocked(holder, diskData); lane != "" {
			return liveHeldBranchRefusal(title, branch, lane, holder)
		}
		return fmt.Errorf("cannot create session %q: branch %q is checked out by the worktree at %s — not only by the archived session %q holding that name — and the new session would derive that same branch. Moving another worktree's branch aside is not af's call, so freeing the archived name would not free the branch and the create would fail at `git worktree add` — release that branch yourself, or create this session under a different name",
			title, branch, config.ShellQuotePath(holder), archived.Title)
	}
	// The branch is held — but as of #2127 the rename can often take it with it,
	// which is the whole point of the durable fix. Refuse only when it cannot.
	//
	// This asks the same question renameArchivedForReuseLocked will act on, rather
	// than a cheaper approximation of it: if the two could disagree, the guard
	// would either refuse a reuse that would have worked, or wave through one that
	// then failed at `git worktree add` with the archived session already renamed —
	// the exact state this function exists to prevent.
	newTitle, terr := m.uniqueArchivedTitleLocked(repoID, repoPath, archived.Title, archived.Program, namespace, diskData)
	if terr == nil && m.reclaimArchivedBranchLocked(repoPath, archived, newTitle) != "" {
		return nil
	}
	return fmt.Errorf("cannot create session %q: the archived session %q still has branch %q checked out at %s, and the new session would derive that same branch. Its branch cannot be moved aside automatically (it is published, externally owned, or its state could not be determined), so freeing the name would not free the branch and the create would fail at `git worktree add` — permanently delete the archived session to release both (%s), or create this session under a different name",
		title, archived.Title, branch, config.ShellQuotePath(holders[0]),
		shellsuggest.PositionalCommand("af", []string{"sessions", "kill"}, archived.Title))
}

// refuseNonReusableTitleConflictLocked preserves title-admission precedence
// ahead of the worktree collision guard. A create serialized behind the winner
// of the same-title race must report the established "already exists" or
// "reserved" error, rather than misdescribe that winner's worktree as an
// unrelated branch collision.
//
// The archived-only case is deliberately excluded: reserveCreate renames that
// record to free the requested title. Every other record conflict is final and
// can be reported before inspecting Git without changing any state.
func (m *Manager) refuseNonReusableTitleConflictLocked(repoID, repoPath, title string, namespace runtimeNameNamespace, diskData []session.InstanceData) error {
	archived, _, err := m.findArchivedOnlyCollisionLocked(repoID, repoPath, title, namespace, diskData)
	if err != nil || archived != nil {
		return err
	}
	return m.findTitleRecordConflictLocked(repoID, repoPath, title, namespace, diskData)
}

// refuseLiveHeldBranchLocked is the narrow #4092 create admission guard. A
// normal local create derives its branch from the title; --here must instead use
// the target worktree's observed branch and path. Only a positively identified
// live lane refuses, and AF never renames, detaches, resets, or moves either
// worktree on this path.
func (m *Manager) refuseLiveHeldBranchLocked(repoPath, workspace, title string, namespace runtimeNameNamespace, inPlace bool, diskData []session.InstanceData) error {
	if namespace != runtimeNamespaceLocalTmux {
		return nil
	}
	var branch string
	var holders []string
	if inPlace {
		var err error
		branch, holders, err = inPlaceBranchHolders(repoPath, workspace)
		if err != nil {
			return fmt.Errorf("cannot create session %q in place: af could not verify the target worktree's actual branch; nothing was started: %w", title, err)
		}
	} else {
		branch = m.branchForTitle(title)
		holders = m.worktreeHeldBranchesLocked(repoPath, false)[branch]
	}
	for _, holder := range holders {
		lane := m.liveLaneHoldingWorktreeLocked(holder, diskData)
		if lane == "" {
			continue
		}
		if inPlace {
			if branch == "" {
				return liveHeldInPlaceDetachedRefusal(title, workspace, lane, holder)
			}
			return liveHeldInPlaceBranchRefusal(title, branch, workspace, lane, holder)
		}
		return liveHeldBranchRefusal(title, branch, lane, holder)
	}
	return nil
}

func inPlaceBranchHolders(repoPath, workspace string) (string, []string, error) {
	bindings, err := worktreeBranchBindings(repoPath)
	if err != nil {
		return "", nil, err
	}
	var target *git.WorktreeBranchBinding
	for index := range bindings {
		if !sameCreateWorktreePath(bindings[index].Path, workspace) {
			continue
		}
		if target != nil {
			return "", nil, fmt.Errorf("worktree %s appears more than once in Git's branch listing", config.ShellQuotePath(workspace))
		}
		target = &bindings[index]
	}
	if target == nil {
		return "", nil, fmt.Errorf("worktree %s is absent from Git's branch listing", config.ShellQuotePath(workspace))
	}
	if target.HeadSHA == "" {
		return "", nil, fmt.Errorf("worktree %s has no observed HEAD", config.ShellQuotePath(workspace))
	}
	if target.Detached {
		if target.Branch != "" {
			return "", nil, fmt.Errorf("worktree %s is reported as both detached and on branch %q", config.ShellQuotePath(workspace), target.Branch)
		}
		return "", []string{target.Path}, nil
	}
	if target.Branch == "" {
		return "", nil, fmt.Errorf("worktree %s has neither a branch nor a detached-HEAD marker", config.ShellQuotePath(workspace))
	}
	holders := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Branch == target.Branch {
			holders = append(holders, binding.Path)
		}
	}
	return target.Branch, holders, nil
}

func sameCreateWorktreePath(left, right string) bool {
	left = pathutil.ResolveForCompare(left)
	return left != "" && left == pathutil.ResolveForCompare(right)
}

func (m *Manager) liveLaneHoldingWorktreeLocked(holder string, diskData []session.InstanceData) string {
	target := pathutil.ResolveForCompare(holder)
	if target == "" {
		return ""
	}
	lanes := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(title, id string) {
		key := "title:" + title
		if id != "" {
			key = "id:" + id
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		lanes = append(lanes, title)
	}
	for _, candidate := range m.instances {
		if candidate == nil || candidate.IsArchived() || pathutil.ResolveForCompare(candidate.GetWorktreePath()) != target {
			continue
		}
		add(candidate.Title, candidate.ID)
	}
	for _, data := range diskData {
		if session.IsArchivedData(data) || pathutil.ResolveForCompare(data.Worktree.WorktreePath) != target {
			continue
		}
		add(data.Title, data.ID)
	}
	sort.Strings(lanes)
	if len(lanes) == 0 {
		return ""
	}
	return lanes[0]
}

func liveHeldBranchRefusal(title, branch, lane, holder string) error {
	handoff := shellsuggest.PositionalCommand("af", []string{"sessions", "handoff", "--to", "<agent>"}, lane)
	return fmt.Errorf("cannot create session %q: branch %q is already checked out by live lane %q at %s. Refusing to bind two live worktrees to one branch because a sibling branch move can turn the idle lane's unchanged index into a staged revert. Continue in that workspace with `%s`, or choose a different session title; af did not rename, detach, reset, or move either worktree",
		title, branch, lane, config.ShellQuotePath(holder), handoff)
}

func liveHeldInPlaceBranchRefusal(title, branch, workspace, lane, holder string) error {
	handoff := shellsuggest.PositionalCommand("af", []string{"sessions", "handoff", "--to", "<agent>"}, lane)
	return fmt.Errorf("cannot create session %q in place at %s: its current branch %q is already checked out by live lane %q at %s. Refusing to activate two live worktrees on one branch because a sibling branch move can turn the idle lane's unchanged index into a staged revert. Continue in the existing lane with `%s`, or check out a different branch in the target worktree yourself; af did not rename, detach, reset, or move either worktree",
		title, config.ShellQuotePath(workspace), branch, lane, config.ShellQuotePath(holder), handoff)
}

func liveHeldInPlaceDetachedRefusal(title, workspace, lane, holder string) error {
	handoff := shellsuggest.PositionalCommand("af", []string{"sessions", "handoff", "--to", "<agent>"}, lane)
	return fmt.Errorf("cannot create session %q in place at %s: its detached HEAD worktree is already used by live lane %q at %s. Refusing to activate two live lanes in one worktree because both would share its files and index. Continue in the existing lane with `%s`, or choose a different worktree yourself; af did not rename, detach, reset, or move either worktree",
		title, config.ShellQuotePath(workspace), lane, config.ShellQuotePath(holder), handoff)
}

// worktreeAdmissionLockForRepo serializes branch/path selection through the
// filesystem operation that commits it. Create, archived restore, and every
// daemon entry that can reach LocalBackend's missing-worktree rebuild use it: a
// free-path observation is not a reservation unless every peer that can
// populate that path is excluded until the observing operation has acted.
// It is keyed by the canonical repository ID already resolved at entry.
func (m *Manager) worktreeAdmissionLockForRepo(repoID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worktreeAdmissionLocks == nil {
		m.worktreeAdmissionLocks = make(map[string]*sync.Mutex)
	}
	lock := m.worktreeAdmissionLocks[repoID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.worktreeAdmissionLocks[repoID] = lock
	}
	return lock
}

// reclaimArchivedBranchLocked decides the branch name the archived session moves
// to when its title is reused, or "" for "leave the branch where it is" (#2127).
//
// This is the durable half of the fix. #2129 shipped an honest refusal because
// freeing a title never freed the branch; moving the branch aside WITH the title
// makes the reclaim complete, so reuse-archived-name actually works for a local
// session instead of always refusing.
//
// Instance.ArchivedBranchForReclaim owns whether the archived session's branch may
// be touched at all (not local, external, published, or unknown — it declines, and
// its doc comment carries the reasoning). This adds the two questions that are the
// daemon's to answer: what the new name should be, and whether it is free.
//
// Deriving the new branch from the new TITLE rather than suffixing the old branch
// keeps the archived row internally coherent: after the move its title, worktree
// directory, and branch all say the same thing, which is what a later restore
// presents to the user.
func (m *Manager) reclaimArchivedBranchLocked(repoPath string, archived *session.Instance, newTitle string) string {
	current, ok := archived.ArchivedBranchForReclaim()
	if !ok {
		return ""
	}
	candidate := m.branchForTitle(newTitle)
	if candidate == "" || candidate == current {
		return ""
	}
	held := m.worktreeHeldBranchesLocked(repoPath, false)
	// Only move a branch that is actually BLOCKING, and this narrowness is the
	// point rather than caution. A freed archived branch (its worktree detached, as
	// after a `git checkout --detach`) is not in anyone's way: `git worktree add
	// <path> <branch>` attaches the new session to it, which is the shipped
	// behaviour where reuse-archived-name already completes and where the new
	// session deliberately continues the archived branch's history.
	//
	// Renaming there would change that outcome — the new session would get a fresh
	// branch off base and the old history would move to a name nobody asked for —
	// which is a semantic change #2127 never asked for. #2127 is about the case
	// where the branch is HELD and the create therefore CANNOT proceed at all.
	holders := held[current]
	if len(holders) == 0 {
		return ""
	}
	// Held by WHOM is a second question, and skipping it is what let this rename
	// somebody else's branch (#3404). The two inputs come from different places and
	// can disagree: `current` is the branch name CACHED on the archived record,
	// while `held` is what git reports right now. Detach the archived worktree —
	// supported, and exactly the case the paragraph above is about — and that cache
	// outlives the hold, so a wholly unrelated live worktree can be sitting on the
	// same branch name. "Some worktree holds it" is then true while "the archived
	// session holds it" is false, and the reclaim renamed the OTHER worktree's
	// branch from <prefix>foo to <prefix>foo-archived, silently and with no consent
	// from whoever was working in it.
	//
	// So the holder must BE this archived session's worktree, and an unresolvable
	// answer declines: an empty holder path, or an archived record carrying no
	// worktree path of its own, is "I cannot tell whose branch this is", which must
	// never authorize rewriting it. Same fail-closed direction as the published and
	// candidate-existence probes on either side of this one.
	// Renaming a multiply-held ref would move every attached worktree's HEAD.
	if len(holders) != 1 || !archivedWorktreeHoldsBranch(archived, holders[0]) {
		return ""
	}
	// The candidate name must be genuinely FREE, and "not checked out" is not the
	// same as "free": `git branch -m` refuses to rename onto ANY existing branch,
	// idle or held. Checking only the checked-out map (the P3 on #2465) let a plain
	// branch of the candidate's name through the guard, after which the rename
	// failed with exit 128 — the guard having promised a name it had not actually
	// cleared. BranchExists closes that, and its unknown answer declines, because a
	// name that cannot be ruled out must be treated as taken rather than renamed
	// onto.
	if len(held[candidate]) > 0 {
		return ""
	}
	if !archived.ArchivedCandidateBranchIsFree(candidate) {
		return ""
	}
	return candidate
}

// archivedWorktreeHoldsBranch reports whether holder — the worktree path git
// names as having a branch checked out — is the archived session's OWN worktree
// (#3404). Callers use it to keep a branch decision inside the session it is
// about; a false answer means the branch belongs to someone else's worktree, or
// that af could not tell, and both must be treated the same way.
//
// Fail-closed by construction. An empty holder (git reported no path) or an
// archived record with no worktree path of its own returns false, so "I do not
// know whose this is" can never read as "it is ours".
//
// Both sides go through pathutil.ResolveForCompare rather than being compared
// verbatim: git reports a worktree by its resolved real path while af stores the
// spelling the worktree was created with, so on a symlinked root (macOS /var ->
// /private/var, the #2110 case) a raw string compare says "a different worktree"
// about the same directory — and here that mistake fails closed into never
// reclaiming, which is silent rather than loud.
func archivedWorktreeHoldsBranch(archived *session.Instance, holder string) bool {
	own := archived.GetWorktreePath()
	if holder == "" || own == "" {
		return false
	}
	return pathutil.ResolveForCompare(holder) == pathutil.ResolveForCompare(own)
}

func (m *Manager) nextAvailableTitleLocked(repoID, repoPath, baseTitle, program string, namespace runtimeNameNamespace, diskData []session.InstanceData, inPlace bool) (string, error) {
	// Shape errors belong to the base, not to any candidate's availability.
	// Validate once before the suffix walk so controls do not burn all 10,000
	// rungs and whitespace cannot turn into a punctuation-only "   -2" title.
	// allowReserved stays true here because a base of "root" deliberately skips
	// the reserved bare candidate and resolves to "root-2" below.
	if err := m.validateTitleShapeLocked(repoPath, baseTitle, namespace, true); err != nil {
		return "", err
	}
	// Session records are not the only thing that can make a candidate unusable
	// (#2091). A branch already CHECKED OUT by some worktree cannot be checked
	// out again, and archiving relocates a worktree rather than removing it
	// (#2013), so an archived session keeps its branch — under a path no record
	// points at once its row has been renamed. The rot that produced: a daily
	// task walked to a suffix its own archived predecessor still held, `git
	// worktree add` refused it, and the task died that way every run, forever.
	// So ask the one component that knows which branches are checked out
	// somewhere, ONCE per walk, and skip those rungs instead of discovering the
	// collision at add time.
	heldBranches := m.worktreeHeldBranchesLocked(repoPath, namespace != runtimeNamespaceLocalTmux)
	// Bound the base before suffixing so a long title's "-N" survives branch
	// truncation and each rung derives a DISTINCT branch; otherwise every rung
	// collapses to the taken base's branch, and the walk skips all 10,000 rungs
	// under m.mu before failing (#2528). The bare base (i == 1) is unchanged.
	boundedBase := git.BoundTitleForDisambiguation(baseTitle)
	var skipped []heldRung
	for i := 1; i <= 10000; i++ {
		candidate := baseTitle
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", boundedBase, i)
		}
		branch := m.branchForTitle(candidate)
		if holders := heldBranches[branch]; len(holders) > 0 {
			// Accumulated rather than logged per rung (#3838): the walk is the whole
			// event, and every rung of it says the same thing.
			skipped = append(skipped, heldRung{title: candidate, holder: strings.Join(holders, ", ")})
			continue
		}
		err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, namespace, false, diskData, inPlace)
		if err == nil {
			m.logHeldSuffixWalkLocked(baseTitle, candidate, skipped)
			return candidate, nil
		}
		// A check that could not RUN is not a taken candidate: no suffix would fare
		// any better, so surface the actionable error instead of spinning through
		// 10,000 of them under the lock.
		if errors.Is(err, errTitleCheckFatal) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an available title for %q%s", baseTitle, heldRungDetail(skipped))
}

// heldRung is one suffix the title walk skipped because a registered worktree
// already has that rung's derived branch checked out (#2091).
type heldRung struct {
	title  string
	holder string
}

// logHeldSuffixWalkLocked reports a completed walk that skipped held rungs as
// ONE line (#3838).
//
// It used to be one INFO line per rung, and the per-rung line carries nothing
// after the first occurrence: a recurring task whose archived predecessors hold
// 110+ consecutive suffixes re-logged its entire own history on every hourly
// create. That was 89% of the daemon log on 2026-09-03 — 1.27 MB/day from one
// message, growing linearly with archive depth. The log is size-capped and
// rotated (#1059), so the harm is not unbounded growth but a diagnostic
// retention window that collapses under a message with no information in it.
//
// The count and the two ends of the run are what survive, because the rungs are
// consecutive: first and last bracket the range an operator would scan, and a
// count that does not match the span between them is itself the signal that the
// walk skipped something else. The individual holder paths are kept where they
// are actually needed — in the error of a walk that FAILED (heldRungDetail).
//
// Silent when nothing was skipped: the uncontested create is the common case and
// has nothing to report. Runs under m.mu with the rest of the walk.
func (m *Manager) logHeldSuffixWalkLocked(base, chosen string, skipped []heldRung) {
	switch len(skipped) {
	case 0:
	case 1:
		m.info().Printf("title %q: skipped 1 suffix held by the worktree at %s; using %q",
			base, skipped[0].holder, chosen)
	default:
		m.info().Printf("title %q: skipped %d suffixes held by existing worktrees (first %s, last %s); using %q",
			base, len(skipped), skipped[0].holder, skipped[len(skipped)-1].holder, chosen)
	}
}

// heldRungDetail names the holders for a walk that exhausted its rungs, as a
// clause appended to the availability error (#3838).
//
// This is the case the per-rung lines existed for, and the one place the
// individual paths earn their bytes: the walk found nothing, and "which
// worktrees are in the way" is the only actionable thing left to say. A failure
// is also rare and terminal, where a routine create is hourly — so the volume
// argument that collapses the success path does not apply here.
func heldRungDetail(skipped []heldRung) string {
	if len(skipped) == 0 {
		return ""
	}
	return fmt.Sprintf("; %d of the suffixes tried have their branches checked out by other worktrees: %s",
		len(skipped), joinHeldRungs(skipped))
}

// heldRungListCap bounds that list. The walk is bounded at 10,000 rungs, so an
// uncapped list is an error string of several hundred kilobytes — and a create
// that fails here fails on every subsequent run, which would put the log volume
// #3838 removes straight back, an order of magnitude larger. 500 is well above
// any real archive depth (the deepest recurring task on the dev box holds ~113),
// so the cap only bites in the pathological case it exists for; below it the
// list is complete.
const heldRungListCap = 500

func joinHeldRungs(skipped []heldRung) string {
	entry := func(rung heldRung) string { return fmt.Sprintf("%q at %s", rung.title, rung.holder) }
	if len(skipped) <= heldRungListCap {
		parts := make([]string, 0, len(skipped))
		for _, rung := range skipped {
			parts = append(parts, entry(rung))
		}
		return strings.Join(parts, " · ")
	}
	// Both ends, because they are what locates the run; the elision names how
	// much it dropped so the list is never silently partial.
	half := heldRungListCap / 2
	parts := make([]string, 0, heldRungListCap+1)
	for _, rung := range skipped[:half] {
		parts = append(parts, entry(rung))
	}
	parts = append(parts, fmt.Sprintf("… %d more …", len(skipped)-2*half))
	for _, rung := range skipped[len(skipped)-half:] {
		parts = append(parts, entry(rung))
	}
	return strings.Join(parts, " · ")
}
