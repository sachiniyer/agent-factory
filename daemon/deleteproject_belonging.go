package daemon

// #7854: a legacy registry row whose recorded root is ABSENT decides which
// sessions belong to it from the row's path-hash identity and the sessions'
// own paths. A session created from a sibling worktree of the bare clone the
// root belonged to is keyed under that clone's identity — or, before #3358,
// under the identity of the directory the clone sits in — and none of its
// paths touch the recorded root, so the delete deregistered the row while the
// session stayed live.
//
// The scan answers "could this live session be the row's?" without git's
// worktree ledger, which `git worktree prune` erases on every archive
// (#7853's lesson). The evidence is what af itself wrote and never deletes:
// the session's canonical repo identity root, and inside that clone's common
// directory the checkout markers registration minted there — a recorded
// checkout id on the row means exactly one common dir holds it.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// absentBelongingProbeTimeout bounds one identity root's whole ownership
// probe — stats, marker reads, and the one-level-down sweep — run as a single
// parked observation. The scan never holds a daemon lock, so the bound exists
// for the filesystem alone: a clone parked on a dead mount must not freeze a
// delete, and a lapsed bound classifies as transient — retriable — not as
// evidence.
const absentBelongingProbeTimeout = 3 * time.Second

// liveRepoIdentity buckets the live sessions keyed under one repository
// identity: their titles and, when any source can supply it, the canonical
// identity root on disk — the bare common directory for a session of a bare
// repository (#3358), the main root otherwise, and the bare clone's PARENT
// directory for a session keyed under the pre-#3358 identity.
type liveRepoIdentity struct {
	titles       map[string]struct{}
	identityRoot string
	rootKnown    bool
}

// liveRepoIdentitiesLocked snapshots every live repository identity and the
// sessions keyed under it from the same four sources repoSessionTitlesLocked
// reads — reserved titles, pending creates, in-flight restores and the
// instance map — but bucketed by EACH session's own repo identity rather than
// the identity under deletion. That is the difference that sees a session
// created from a sibling worktree: it is keyed under its repo's identity,
// invisible to a scan of only the row's ids (#7854). The caller holds m.mu.
func (m *Manager) liveRepoIdentitiesLocked() map[string]*liveRepoIdentity {
	sets := make(map[string]*liveRepoIdentity)
	add := func(repoID, title, identityRoot string) {
		if repoID == "" || title == "" {
			return
		}
		set := sets[repoID]
		if set == nil {
			set = &liveRepoIdentity{titles: make(map[string]struct{})}
			sets[repoID] = set
		}
		set.titles[title] = struct{}{}
		if identityRoot != "" {
			set.identityRoot = identityRoot
			set.rootKnown = true
		}
	}
	for key := range m.reservedTitles {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "")
	}
	for key, pending := range m.pendingCreates {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, pending.Worktree.RepoPath)
	}
	for key := range m.restoresInFlight {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "")
	}
	for key, inst := range m.instances {
		repoID, title := splitDaemonInstanceKey(key)
		if inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		add(repoID, title, inst.GetRepoIdentityPath())
	}
	return sets
}

// absentRootClass is what the scan could make of a session — and therefore
// which remedy the refusal names. Each class names a remedy that RESOLVES it:
// a retriable gap is retried, a possibly-owned session is archived.
type absentRootClass int

const (
	// classOwner is the only affirmative evidence: the row's own checkout
	// marker lives inside this repository's common directory — or, for a
	// pre-#3358 parent identity, inside a bare clone it contains. A sibling
	// worktree of that clone could be the session's workspace, so the session
	// could be the row's. Remedy: archive it.
	classOwner absentRootClass = iota
	// classTransient covers answers af could not GET: the session has not
	// named its repository yet (a reserved title, an in-flight restore, a
	// create before pendingCreates publishes a root), or the probe's wait
	// budget lapsed on an unresponsive filesystem. Remedy: retry the delete.
	classTransient
	// classPersistent covers answers af could not get for a reason retry
	// will not fix: a vanished root, a directory that is no repository shape
	// af recognises, or marker evidence that exists but cannot be read.
	// Remedy: archive the session if it is the project's; restoring the
	// repository is the other way af can describe it.
	classPersistent
	// classUnattributed is a row that predates checkout ids: there is no
	// durable value to discriminate any repository on, so every live identity
	// stays suspect. Remedy: archive the sessions that are the project's.
	classUnattributed
)

// absentRootSuspect is one live session the delete could not clear of
// belonging to the absent-rooted row it would deregister.
type absentRootSuspect struct {
	title  string
	root   string
	class  absentRootClass
	reason string
}

// absentRootBelonging is the verdict of the absent-branch ownership scan:
// cleared names every live repository identity the evidence proved could NOT
// own the row's recorded root, and suspects names the live sessions nothing
// cleared. checkoutID is the row's own recorded checkout id — the evidence
// the cleared verdicts rest on — kept so the post-deregister re-scan can vet
// a session that materialized mid-delete the same way.
type absentRootBelonging struct {
	checkoutID string
	cleared    map[string]struct{}
	suspects   []absentRootSuspect
}

// unvettedIdentities is the post-deregister form of "could belong": the live
// repository identities this scan never cleared. An identity proven clear
// stays clear — the evidence names the clone, not the session — but an
// identity the scan never saw must be vetted now, because a session that
// materialized during the deregister is suspect whether it arrived before or
// after the row came off.
func (b *absentRootBelonging) unvettedIdentities(sets map[string]*liveRepoIdentity) map[string]*liveRepoIdentity {
	unvetted := make(map[string]*liveRepoIdentity)
	for repoID, set := range sets {
		if _, ok := b.cleared[repoID]; ok {
			continue
		}
		unvetted[repoID] = set
	}
	return unvetted
}

// vetIdentities classifies live identity sets the same way the resolution
// scan does — the marker evidence still reads, because it lives in the
// clone's common directory and survives the row's removal.
func (b *absentRootBelonging) vetIdentities(sets map[string]*liveRepoIdentity) []absentRootSuspect {
	_, suspects := classifyRepoIdentities(sets, b.checkoutID)
	return suspects
}

// classifyRepoIdentities is the verdict per live repository identity. A
// cleared verdict is withheld until evidence exists for it: a row recorded
// before checkout ids has nothing to discriminate on, a session with no
// nameable root has nothing to probe, and a probe that cannot answer inside
// its bound is unproven rather than "clear".
func classifyRepoIdentities(sets map[string]*liveRepoIdentity, checkoutID string) (cleared map[string]struct{}, suspects []absentRootSuspect) {
	cleared = make(map[string]struct{})
	suspect := func(set *liveRepoIdentity, class absentRootClass, reason string) {
		for title := range set.titles {
			suspects = append(suspects, absentRootSuspect{title: title, root: set.identityRoot, class: class, reason: reason})
		}
	}
	for _, repoID := range sortedRepoIdentitySets(sets) {
		set := sets[repoID]
		switch {
		case checkoutID == "":
			suspect(set, classUnattributed, "")
		case !set.rootKnown:
			suspect(set, classTransient, "it has not named its repository yet")
		default:
			verdict := awaitRootBelonging(set.identityRoot, checkoutID)
			switch {
			case verdict.owner:
				suspect(set, classOwner, verdict.evidence)
			case verdict.transient:
				suspect(set, classTransient, "af could not finish describing its repository within "+absentBelongingProbeTimeout.String())
			case verdict.unproven:
				suspect(set, classPersistent, verdict.reason)
			default:
				cleared[repoID] = struct{}{}
			}
		}
	}
	sort.Slice(suspects, func(i, j int) bool {
		if suspects[i].title != suspects[j].title {
			return suspects[i].title < suspects[j].title
		}
		return suspects[i].root < suspects[j].root
	})
	return cleared, suspects
}

func sortedRepoIdentitySets(sets map[string]*liveRepoIdentity) []string {
	ids := make([]string, 0, len(sets))
	for id := range sets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// rootBelongingVerdict is what one identity root's probe established. owner
// is the affirmative evidence, and evidence names the common directory the
// row's marker was found in — the root itself for a bare clone, one level
// down for a pre-#3358 parent. cleared requires that every marker directory
// the root could mean was readable AND carried no marker with the row's id —
// a difference or an absence is proof of a different clone ONLY when the read
// itself succeeded. unproven keeps persistent causes, transient the
// wait-budget lapse the caller reads as retriable.
type rootBelongingVerdict struct {
	owner     bool
	cleared   bool
	unproven  bool
	transient bool
	evidence  string
	reason    string
}

// absentBelongingProbeFlights bounds an unresponsive identity-root probe to
// one parked observation per (root, checkoutID) pair. Callers time out
// independently, and a later probe of the SAME question joins the parked one
// instead of stranding a second goroutine on the same dead filesystem (the
// checkoutMarkerReadFlights contract, applied to the whole probe). The key
// carries the checkout id because the verdict DEPENDS on it: sharing by root
// alone would hand one absent-rooted row's delete another row's cleared
// verdict, deregistering a project whose marker was never compared (#7914
// review).
type rootBelongingProbeKey struct {
	root       string
	checkoutID string
}

var absentBelongingProbeFlights sync.Map

type rootBelongingObservation struct {
	done    chan struct{}
	verdict rootBelongingVerdict
}

func awaitRootBelonging(root, checkoutID string) rootBelongingVerdict {
	key := rootBelongingProbeKey{root: root, checkoutID: checkoutID}
	obs := &rootBelongingObservation{done: make(chan struct{})}
	actual, loaded := absentBelongingProbeFlights.LoadOrStore(key, obs)
	if !loaded {
		go func() {
			obs.verdict = probeRootBelonging(root, checkoutID)
			absentBelongingProbeFlights.CompareAndDelete(key, obs)
			close(obs.done)
		}()
	} else {
		obs = actual.(*rootBelongingObservation)
	}
	timer := time.NewTimer(absentBelongingProbeTimeout)
	defer timer.Stop()
	select {
	case <-obs.done:
		return obs.verdict
	case <-timer.C:
		select {
		case <-obs.done:
			return obs.verdict
		default:
			return rootBelongingVerdict{transient: true}
		}
	}
}

// probeRootBelongingHookForTest, when non-nil, runs at the top of every
// probeRootBelonging inside the parked observation, so a test can hold two
// concurrent deletes' probes open at once and see whether a second (root,
// checkoutID) ran its own probe or joined another row's parked verdict
// (#7914 review: the flight key must carry the checkout id).
var probeRootBelongingHookForTest func(root string)

// probeRootBelonging runs the whole per-root probe as filesystem reads only —
// no git subprocess, and nothing from the worktree ledger. It answers which
// common directories this root could mean, then reads af's own checkout
// markers there:
//
//   - <root>/.git as a directory names the common directory of a non-bare
//     repository;
//   - <root>/.git as a FILE is the worktree-side gitdir pointer (a linked
//     worktree, a submodule, --separate-git-dir), which af resolves to the
//     common directory rather than leaving the root undescribable;
//   - a bare repository (HEAD next to objects/ and refs/) IS its common
//     directory;
//   - and one level down, <root>/*/agent-factory/ covers the pre-#3358
//     identity: a session keyed under a bare clone's PARENT directory is
//     provably that clone's when the clone holds the row's marker.
//
// Marker reads are by content under ANY home's file name, because a row
// records the marker VALUE, not which home minted it. A common directory is
// cleared only when it is readable and no checkout-id-* carries the row's id;
// a marker directory the user deleted after registration is the known
// residue — the owner then reads as a never-registered clone — and it is
// deliberately accepted rather than treated as proof, because the
// alternative makes a missing directory proof of ownership.
func probeRootBelonging(root, checkoutID string) rootBelongingVerdict {
	if probeRootBelongingHookForTest != nil {
		probeRootBelongingHookForTest(root)
	}
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return rootBelongingVerdict{unproven: true, reason: "the path no longer exists"}
	case err != nil:
		return rootBelongingVerdict{unproven: true, reason: "the path could not be inspected: " + err.Error()}
	case !info.IsDir():
		return rootBelongingVerdict{unproven: true, reason: "the path is not a directory"}
	}
	commonDir, classifiable, classReason := identityRootCommonDir(root)

	// The marker sweep runs over every agent-factory directory this root
	// could own: its established common directory, the root itself when the
	// shape leaves the question open (a de-gitted bare clone leaves the
	// directory behind), and each immediate child — the pre-#3358 identity
	// rooted here could be a bare clone one level down.
	sweep := []string{}
	if classifiable {
		sweep = append(sweep, commonDir)
	} else {
		sweep = append(sweep, root)
	}
	children, err := os.ReadDir(root)
	if err != nil {
		return rootBelongingVerdict{unproven: true, reason: "the directory could not be listed for a bare clone inside it: " + err.Error()}
	}
	for _, child := range children {
		if !child.IsDir() {
			continue
		}
		childDir := filepath.Join(root, child.Name())
		if classifiable && childDir == commonDir {
			continue
		}
		sweep = append(sweep, childDir)
	}

	unproven := ""
	for _, dir := range sweep {
		ids, present, err := config.CheckoutIDsInCommonDir(dir)
		if err != nil {
			unproven = err.Error()
			continue
		}
		if !present {
			continue
		}
		for _, id := range ids {
			if id == checkoutID {
				return rootBelongingVerdict{owner: true, evidence: dir}
			}
		}
	}
	switch {
	case unproven != "":
		return rootBelongingVerdict{unproven: true, reason: unproven}
	case !classifiable:
		return rootBelongingVerdict{unproven: true, reason: classReason}
	default:
		return rootBelongingVerdict{cleared: true}
	}
}

// identityRootCommonDir locates the common directory an identity root's
// marker would live in, or reports the root undescribable. The .git entry is
// read, never the worktree records inside the common directory itself.
func identityRootCommonDir(root string) (commonDir string, ok bool, reason string) {
	dotgit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotgit)
	if err == nil {
		if info.IsDir() {
			return dotgit, true, ""
		}
		if common, ok := commonDirFromGitFile(dotgit); ok {
			return common, true, ""
		}
		return "", false, "the .git file could not be resolved to a common directory"
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, "the .git entry could not be inspected: " + err.Error()
	}
	if identityRootBareShaped(root) {
		return root, true, ""
	}
	return "", false, "no .git entry and not bare-shaped"
}

// commonDirFromGitFile resolves the worktree-side gitdir pointer a .git file
// holds — the same file `git rev-parse --git-common-dir` reads, durable under
// af's housekeeping (nothing in af rewrites a checkout's own pointer). A
// linked worktree's gitdir lives at <common>/worktrees/<name>; any other
// gitdir (a submodule's module dir, --separate-git-dir) IS the common
// directory.
func commonDirFromGitFile(dotgit string) (string, bool) {
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", false
	}
	gitdir := strings.TrimSpace(target)
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(dotgit), gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if filepath.Base(filepath.Dir(gitdir)) == "worktrees" {
		return filepath.Dir(filepath.Dir(gitdir)), true
	}
	return gitdir, true
}

// identityRootBareShaped reports whether root has the directory shape of a
// bare repository — HEAD next to objects/ and refs/. It is not ownership
// evidence (the ledger is never consulted); it only locates the common
// directory the marker sweep targets.
func identityRootBareShaped(root string) bool {
	head, err := os.Lstat(filepath.Join(root, "HEAD"))
	if err != nil || head.IsDir() {
		return false
	}
	objects, err := os.Stat(filepath.Join(root, "objects"))
	if err != nil || !objects.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(root, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	return true
}

// absentRootBelongingScan is the absent-recorded-root ownership scan run at
// target resolution, before any durable write: every live repository identity
// is cleared, or its sessions are suspects.
func (m *Manager) absentRootBelongingScan(checkoutID string) *absentRootBelonging {
	m.mu.Lock()
	sets := m.liveRepoIdentitiesLocked()
	m.mu.Unlock()

	cleared, suspects := classifyRepoIdentities(sets, checkoutID)
	return &absentRootBelonging{checkoutID: checkoutID, cleared: cleared, suspects: suspects}
}

// archiveSuggestion renders the one command that resolves a suspect whose
// resolution is archiving the session: --repo scoped to the repository af
// saw it under, because titles are unique per repo, not fleet-wide.
func archiveSuggestion(s absentRootSuspect) string {
	argv := []string{"sessions", "archive"}
	if s.root != "" {
		argv = append(argv, "--repo", s.root)
	}
	return shellsuggest.PositionalCommand("af", argv, s.title)
}

// absentRootBelongingRefusal renders the delete refusal for live sessions the
// absent-root scan could not clear. Each suspect class names the remedy that
// resolves THAT class — archiving resolves possibly-owned sessions; retrying
// resolves the ones af could not finish describing.
func absentRootBelongingRefusal(repoID, repoPath string, suspects []absentRootSuspect) error {
	var owners, transients, persistents, unattributed []absentRootSuspect
	for _, s := range suspects {
		switch s.class {
		case classOwner:
			owners = append(owners, s)
		case classTransient:
			transients = append(transients, s)
		case classUnattributed:
			unattributed = append(unattributed, s)
		default:
			persistents = append(persistents, s)
		}
	}
	titles := make([]string, 0, len(suspects))
	for _, s := range suspects {
		titles = append(titles, s.title)
	}
	titles = session.DedupeSorted(titles)

	clauses := []string{}
	names := func(ss []absentRootSuspect) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, s.title)
		}
		return session.DedupeSorted(out)
	}
	cmds := func(ss []absentRootSuspect) string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, archiveSuggestion(s))
		}
		return strings.Join(out, "; ")
	}
	if len(owners) > 0 {
		roots := session.DedupeSorted(func() []string {
			out := make([]string, 0, len(owners))
			for _, s := range owners {
				out = append(out, s.root)
			}
			return out
		}())
		evidences := session.DedupeSorted(func() []string {
			out := make([]string, 0, len(owners))
			for _, s := range owners {
				out = append(out, s.reason)
			}
			return out
		}())
		clauses = append(clauses, fmt.Sprintf(
			"session(s) %v are live under %s — inside it af found this record's own checkout marker (%s), so a sibling worktree of that clone could be their workspace; archive them: %s",
			names(owners), strings.Join(roots, " and "), strings.Join(evidences, " and "), cmds(owners)))
	}
	if len(transients) > 0 {
		reasons := make(map[string]struct{})
		for _, s := range transients {
			reasons[s.reason] = struct{}{}
		}
		clauses = append(clauses, fmt.Sprintf(
			"af could not finish asking what repository session(s) %v belong to (%s) — retry the delete once that settles",
			names(transients), strings.Join(sortedKeys(reasons), "; ")))
	}
	if len(persistents) > 0 {
		parts := make([]string, 0, len(persistents))
		for _, s := range persistents {
			parts = append(parts, fmt.Sprintf("%s (%s)", s.root, s.reason))
		}
		sort.Strings(parts)
		clauses = append(clauses, fmt.Sprintf(
			"session(s) %v are live under a repository af cannot describe (%s) — archive the ones that are this project's: %s; for the rest, restoring the repository so af can describe it also clears them",
			names(persistents), strings.Join(parts, "; "), cmds(persistents)))
	}
	if len(unattributed) > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"session(s) %v are live and this record predates checkout ids, so nothing af wrote can clear them — archive the ones that are this project's: %s",
			names(unattributed), cmds(unattributed)))
	}
	return fmt.Errorf(
		"delete project %s: its recorded root %s is absent and the record predates af's repository identities, so af cannot establish which live sessions it owned — and it could not clear session(s) %v; nothing was changed. %s",
		repoID, repoPath, titles, strings.Join(clauses, " "),
	)
}

// absentRootEscapedReports renders the post-deregister half: sessions that
// materialized during the deregister under identities the evidence could not
// clear. The record is already gone, so unlike the resolution-time refusal
// this cannot advise a retry — a retry finds no row, runs no scan, and vets
// nothing. The remedy is archiving the sessions that are the removed
// project's.
func absentRootEscapedReports(suspects []absentRootSuspect) string {
	owners, other := []absentRootSuspect{}, []absentRootSuspect{}
	for _, s := range suspects {
		if s.class == classOwner {
			owners = append(owners, s)
		} else {
			other = append(other, s)
		}
	}
	var parts []string
	titlesOf := func(ss []absentRootSuspect) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, s.title)
		}
		return session.DedupeSorted(out)
	}
	cmdsOf := func(ss []absentRootSuspect) string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, archiveSuggestion(s))
		}
		return strings.Join(out, "; ")
	}
	if len(owners) > 0 {
		parts = append(parts, fmt.Sprintf(
			"session(s) %v are live under the clone that held the removed record's own checkout marker — they are this project's; archive them: %s",
			titlesOf(owners), cmdsOf(owners)))
	}
	if len(other) > 0 {
		parts = append(parts, fmt.Sprintf(
			"session(s) %v could not be vetted against the removed record — archive the ones that are this project's: %s",
			titlesOf(other), cmdsOf(other)))
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
