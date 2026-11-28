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
// identity: their per-session detail and, when any source can supply it, the
// canonical identity root on disk — the bare common directory for a session
// of a bare repository (#3358), the main root otherwise, and the bare
// clone's PARENT directory for a session keyed under the pre-#3358 identity.
type liveRepoIdentity struct {
	sessions     map[string]suspectSession
	identityRoot string
	rootKnown    bool
}

// suspectSession is the per-session detail a refusal names so the human can
// find the session without a command: its worktree path, its stable session
// id (#1195), and whether archive can even act on it — an in-place session,
// a root agent, or a remote session has no worktree archive can relocate, so
// archive.go refuses them outright and the refusal must not print that
// command.
type suspectSession struct {
	worktreePath string
	sessionID    string
	unarchivable bool
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
	add := func(repoID, title, identityRoot string, detail suspectSession) {
		if repoID == "" || title == "" {
			return
		}
		set := sets[repoID]
		if set == nil {
			set = &liveRepoIdentity{sessions: make(map[string]suspectSession)}
			sets[repoID] = set
		}
		// A title can surface in several sources at once as a create runs —
		// reserved, pending, materialized — so merge detail field-wise: the
		// later, richer source fills what the earlier one could not know.
		cur := set.sessions[title]
		if detail.worktreePath != "" {
			cur.worktreePath = detail.worktreePath
		}
		if detail.sessionID != "" {
			cur.sessionID = detail.sessionID
		}
		cur.unarchivable = cur.unarchivable || detail.unarchivable
		set.sessions[title] = cur
		if identityRoot != "" {
			set.identityRoot = identityRoot
			set.rootKnown = true
		}
	}
	for key := range m.reservedTitles {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "", suspectSession{})
	}
	for key, pending := range m.pendingCreates {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, pending.Worktree.RepoPath, suspectSession{
			worktreePath: pending.Worktree.WorktreePath,
			sessionID:    pending.ID,
			unarchivable: pending.Worktree.ExternalWorktree,
		})
	}
	for key := range m.restoresInFlight {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "", suspectSession{})
	}
	for key, inst := range m.instances {
		repoID, title := splitDaemonInstanceKey(key)
		if inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		add(repoID, title, inst.GetRepoIdentityPath(), suspectSession{
			worktreePath: inst.GetWorktreePath(),
			sessionID:    inst.ID,
			unarchivable: inst.IsExternalWorktree() || !inst.Capabilities().Archive,
		})
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
// belonging to the absent-rooted row it would deregister. It carries the
// detail the refusal prints — the session's key, its worktree path and
// stable session id — and repoFlagOK, the one verdict a printed `af sessions
// archive --repo <root>` command needs: that <root> provably hashes to this
// session's own key, so the command can only ever resolve to it.
type absentRootSuspect struct {
	title        string
	repoID       string
	root         string
	worktreePath string
	sessionID    string
	unarchivable bool
	repoFlagOK   bool
	class        absentRootClass
	reason       string
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

// excludeDeletedIdentity drops suspects keyed under the deleted row's own
// repo identity and marks that identity cleared for the post-deregister
// re-scan. A real-id row's R-keyed live sessions are handled by the existing
// stranded/starting gates and the archive loop, which key on repoID — the
// scan would otherwise flag them as owners (the row's own checkout marker
// lives in its common dir) and turn ordinary real-id deletes with live
// sessions into refusals. A derived-id row has no sessions under its
// invented d-… identity, so the call is a no-op for it. Marking the identity
// cleared keeps the post-deregister re-scan's unvetted pass from
// re-reporting a session under it — escaped, via fenced, already reports
// such a session with the correct "delete again" remedy.
func (b *absentRootBelonging) excludeDeletedIdentity(repoID string) {
	if b == nil || repoID == "" {
		return
	}
	if b.cleared == nil {
		b.cleared = make(map[string]struct{})
	}
	b.cleared[repoID] = struct{}{}
	kept := make([]absentRootSuspect, 0, len(b.suspects))
	for _, s := range b.suspects {
		if s.repoID == repoID {
			continue
		}
		kept = append(kept, s)
	}
	b.suspects = kept
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
	suspect := func(repoID string, set *liveRepoIdentity, class absentRootClass, reason string, repoFlagRoot bool) {
		// `--repo <root>` provably reaches THIS session only when the probe
		// established that RepoFromPath resolves <root> to <root> itself AND
		// <root> hashes to the session's own key. Anything weaker — a root
		// af cannot describe, a gitfile that resolves elsewhere, a root
		// whose hash is not the session's key — means the printed command
		// could land on a different project, so none is printed.
		flagOK := repoFlagRoot && set.identityRoot != "" &&
			config.RepoIDFromRoot(filepath.Clean(set.identityRoot)) == repoID
		for title, sess := range set.sessions {
			suspects = append(suspects, absentRootSuspect{
				title: title, repoID: repoID, root: set.identityRoot,
				worktreePath: sess.worktreePath, sessionID: sess.sessionID,
				unarchivable: sess.unarchivable, repoFlagOK: flagOK,
				class: class, reason: reason,
			})
		}
	}
	for _, repoID := range sortedRepoIdentitySets(sets) {
		set := sets[repoID]
		switch {
		case checkoutID == "":
			suspect(repoID, set, classUnattributed, "", false)
		case !set.rootKnown:
			suspect(repoID, set, classTransient, "it has not named its repository yet", false)
		default:
			verdict := awaitRootBelonging(set.identityRoot, checkoutID)
			switch {
			case verdict.owner:
				suspect(repoID, set, classOwner, verdict.evidence, verdict.repoFlagRoot)
			case verdict.transient:
				suspect(repoID, set, classTransient, "af could not finish describing its repository within "+absentBelongingProbeTimeout.String(), false)
			case verdict.unproven:
				suspect(repoID, set, classPersistent, verdict.reason, false)
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
// down for a pre-#3358 parent. cleared requires every place the row's
// checkout id could live here — the established common dir, a plain root's
// own marker dir, and each repository-or-indeterminate child's — to read
// cleanly AND carry no marker with the row's id: a clean sweep clears
// whatever the root's shape, because the evidence question was always where
// the id lives, not what the root is. unproven keeps persistent causes —
// including a repository af cannot describe — and transient the wait-budget
// lapse the caller reads as retriable.
type rootBelongingVerdict struct {
	owner     bool
	cleared   bool
	unproven  bool
	transient bool
	evidence  string
	reason    string
	// repoFlagRoot is set when the probe established that RepoFromPath
	// resolves the root to the root itself — a `.git` directory or a
	// bare-shaped directory — so `af sessions archive --repo <root>` can
	// only ever name this session's own key. A gitfile root resolves to a
	// DIFFERENT repository's identity, and anything unproven resolves
	// nowhere provable, so neither earns the flag.
	repoFlagRoot bool
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
//     repository (a .git symlink is followed first — git accepts links to a
//     metadata directory or to a redirect file);
//   - <root>/.git as a FILE is the worktree-side gitdir pointer (a linked
//     worktree, a submodule, --separate-git-dir), which af resolves the way
//     git does — gitdir, then its commondir file — and counts only when the
//     answer exists and is git-shaped;
//   - a bare repository (HEAD next to objects/ and refs/) IS its common
//     directory;
//   - and one level down, <root>/*/agent-factory/ covers the pre-#3358
//     identity: a session keyed under a bare clone's PARENT directory is
//     provably that clone's when the clone holds the row's marker — while a
//     child DETERMINATELY not a repository cannot hold one at all.
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
	commonDir, shape, shapeReason := identityRootCommonDir(root)
	if shape == shapeUnproven {
		return rootBelongingVerdict{unproven: true, reason: shapeReason}
	}
	// `af sessions archive --repo <root>` provably reaches the session keyed
	// under this root only when RepoFromPath resolves <root> to <root>
	// itself — a `.git` directory (git-dir == common-dir, so the toplevel is
	// the identity) or a bare-shaped directory (the direct bare probe names
	// the git dir itself). A gitfile resolves to a different repository's
	// identity, so its --repo could name the wrong project.
	repoFlagRoot := shape == shapeCommonDir &&
		(commonDir == root || commonDir == filepath.Join(root, ".git"))

	// The marker sweep runs over every agent-factory directory this root
	// could own: its established common directory, the root itself when it
	// is not a repository (a de-gitted clone leaves the directory behind),
	// and each immediate child — the pre-#3358 identity rooted here could
	// be a bare clone one level down. A root whose whole sweep reads
	// cleanly and carries no marker with the row's id is CLEARED, whatever
	// its own shape was: the evidence question was always where the row's
	// checkout id lives, and every place it could live here answered.
	sweep := []string{}
	if shape == shapeCommonDir {
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
		if shape == shapeCommonDir && childDir == commonDir {
			continue
		}
		// A child that is DETERMINATELY not a repository — its .git entry
		// and its bare shape both answered absent — cannot be the clone a
		// pre-#3358 identity rooted here would mean, so whatever sits under
		// an agent-factory name inside it cannot be the row's marker and
		// never makes this root unproven (#7914 review). A child af cannot
		// describe — an unreadable one included — stays in the sweep: it
		// could still be a bare clone holding the row's marker, and its own
		// marker-directory read decides.
		if childPlainDir(childDir) {
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
				return rootBelongingVerdict{owner: true, evidence: dir, repoFlagRoot: repoFlagRoot}
			}
		}
	}
	if unproven != "" {
		return rootBelongingVerdict{unproven: true, reason: unproven}
	}
	return rootBelongingVerdict{cleared: true, repoFlagRoot: repoFlagRoot}
}

// childPlainDir reports whether a directory is DETERMINATELY not a
// repository: every shape probe — the .git entry and the bare-metadata
// triplet — answered "absent", not "unreadable". Any answer that failed
// leaves the child in the sweep, where its marker directory's own read
// decides.
func childPlainDir(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// identityRootShape is what an identity root's .git entry says the root is:
// a repository whose common directory could be named (shapeCommonDir), not a
// repository at all (shapePlain — sweep it and its children for marker
// directories), or a repository af cannot describe (shapeUnproven — never
// cleared, because a gitfile that does not resolve to an existing git-shaped
// common directory could still be the row's clone).
type identityRootShape int

const (
	shapeCommonDir identityRootShape = iota
	shapePlain
	shapeUnproven
)

// identityRootCommonDir locates the common directory an identity root's
// marker would live in, or classifies the root as plain (sweep it for marker
// dirs) or unproven (a repository whose metadata af cannot describe). The
// .git entry is read, never the worktree records inside the common directory
// itself.
func identityRootCommonDir(root string) (commonDir string, shape identityRootShape, reason string) {
	dotgit := filepath.Join(root, ".git")
	resolved := dotgit
	lst, lerr := os.Lstat(dotgit)
	switch {
	case lerr == nil && lst.Mode()&os.ModeSymlink != 0:
		// A .git symlink — to the metadata directory OR to a gitdir
		// redirect file — is a layout git accepts (#3278 review): resolve it
		// before classifying, or a metadata DIRECTORY reads as a redirect
		// file and the repository reports undescribable.
		r, evalErr := filepath.EvalSymlinks(dotgit)
		if evalErr != nil {
			return "", shapeUnproven, "the .git symlink could not be resolved: " + evalErr.Error()
		}
		resolved = r
	case lerr != nil && !errors.Is(lerr, os.ErrNotExist):
		return "", shapeUnproven, "the .git entry could not be inspected: " + lerr.Error()
	}
	info, err := os.Stat(resolved)
	switch {
	case err == nil && info.IsDir():
		return resolved, shapeCommonDir, ""
	case err == nil:
		if common, ok := gitfileCommonDir(resolved); ok {
			return common, shapeCommonDir, ""
		}
		// A .git file that does not resolve to an existing git-shaped
		// common directory is a repository af cannot describe — unproven,
		// never cleared: its real common dir could still hold the row's
		// marker where the sweep cannot see it (#7914 review).
		return "", shapeUnproven, "the .git file could not be resolved to a common directory that exists and is git-shaped"
	case errors.Is(err, os.ErrNotExist):
		if lerr == nil {
			// The entry was there a moment ago — it changed mid-probe.
			return "", shapeUnproven, "the .git entry changed while it was being inspected"
		}
		if identityRootBareShaped(root) {
			return root, shapeCommonDir, ""
		}
		return "", shapePlain, ""
	default:
		return "", shapeUnproven, "the .git entry could not be inspected: " + err.Error()
	}
}

// gitfileCommonDir resolves a .git file to the repository's real common
// directory the way git does — the same file `git rev-parse
// --git-common-dir` reads, durable under af's housekeeping (nothing in af
// rewrites a checkout's own pointer). The pointer names the per-worktree
// GITDIR; a linked worktree's gitdir then redirects through its `commondir`
// file, while every other gitdir (a submodule's module dir, a
// --separate-git-dir metadata tree) IS the common directory. The resolved
// directory counts only when it exists and is git-shaped: an answer that
// cannot be verified is no answer at all, so a moved, dangling or
// whitespace-truncated target never reads as a clone that holds no markers
// (#7914 review: clearing on an unverified resolution is a fail-open).
func gitfileCommonDir(dotgit string) (string, bool) {
	gitdir, ok := readGitdirPointer(dotgit)
	if !ok {
		return "", false
	}
	common := gitdir
	if cd, err := readGitPointerLine(filepath.Join(gitdir, "commondir")); err == nil {
		if filepath.IsAbs(cd) {
			common = filepath.Clean(cd)
		} else {
			common = filepath.Join(gitdir, cd)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// commondir exists-or-not is part of the answer; a read that failed
		// for another reason leaves the real common dir undetermined.
		return "", false
	}
	if !identityRootBareShaped(common) {
		return "", false
	}
	return common, true
}

// readGitPointerLine reads the single line a git pointer file holds — a
// worktree's .git redirect or a gitdir's commondir — preserving the path
// bytes: only the trailing line ending is stripped, never whitespace inside
// the path (session/git's parseGitdirPointerLine, #3278 review — TrimSpace
// truncated real metadata directory names ending in a space).
func readGitPointerLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSuffix(line, "\r"), nil
}

// readGitdirPointer parses a .git file's `gitdir:` line into an absolute
// path, relative targets resolving against the file's own directory. Only
// the format's single separator space is stripped: git writes the target
// literally, and a separate-git-dir name that genuinely ends in whitespace
// must survive the parse.
func readGitdirPointer(dotgit string) (string, bool) {
	line, err := readGitPointerLine(dotgit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", false
	}
	gitdir := strings.TrimPrefix(target, " ")
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(dotgit), gitdir)
	}
	return filepath.Clean(gitdir), true
}

// identityRootBareShaped reports whether root has the directory shape of
// git metadata — HEAD next to objects/ and refs/: a bare repository's top
// level, or any resolved common directory. It is not ownership evidence
// (the ledger is never consulted); it only verifies that a candidate
// directory really is the metadata the marker sweep should read.
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

// suspectRef names one suspect for a human to find without a command: its
// title, its stable session id (#1195), and its worktree path — falling back
// to the identity root af recorded when no materialized row can say where
// its workspace lives.
func suspectRef(s absentRootSuspect) string {
	path := s.worktreePath
	if path == "" {
		path = s.root
	}
	if s.sessionID != "" {
		return fmt.Sprintf("%q (session id %s, worktree %s)", s.title, s.sessionID, path)
	}
	return fmt.Sprintf("%q (worktree %s)", s.title, path)
}

// suspectRemedy is the per-suspect half of the refusal: what resolves THIS
// session, without ever printing a command that could land on a different
// one. The bare `af sessions archive <title>` resolves by cwd, and a
// same-titled session in another project is exactly the ambiguity it cannot
// see — so the only archive command printed is `--repo <root>`, and only
// when <root> provably hashes to this session's own key (repoFlagOK).
// Otherwise the remedy names the session — title, stable session id and
// worktree path — and points at the TUI or web UI, where it is listed. A
// session archive cannot act on at all (an in-place session, a root agent,
// or a remote session has no worktree to relocate — archive.go refuses
// them) gets no command: it must be stopped or removed by the user.
func suspectRemedy(s absentRootSuspect) string {
	if s.unarchivable {
		return fmt.Sprintf("%s cannot be archived — in-place, root-agent and remote sessions have no worktree archive can relocate — so it must be stopped or removed by the user", suspectRef(s))
	}
	if s.repoFlagOK {
		return fmt.Sprintf("archive %s from the TUI or web UI, where it is listed — or run %s",
			suspectRef(s), shellsuggest.PositionalCommand("af", []string{"sessions", "archive", "--repo", s.root}, s.title))
	}
	return fmt.Sprintf("archive %s from the TUI or web UI, where it is listed", suspectRef(s))
}

// suspectRemedies joins the per-suspect remedies a clause names.
func suspectRemedies(ss []absentRootSuspect) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, suspectRemedy(s))
	}
	return strings.Join(out, "; ")
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
			"session(s) %v are live under %s — inside it af found this record's own checkout marker (%s), so a sibling worktree of that clone could be their workspace: %s",
			names(owners), strings.Join(roots, " and "), strings.Join(evidences, " and "), suspectRemedies(owners)))
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
			"session(s) %v are live under a repository af cannot describe (%s) — for the ones that are this project's: %s; for the rest, making the repository readable again (restoring it, fixing permissions, or removing the unreadable entry) also clears them",
			names(persistents), strings.Join(parts, "; "), suspectRemedies(persistents)))
	}
	if len(unattributed) > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"session(s) %v are live and this record predates checkout ids, so nothing af wrote can clear them — for the ones that are this project's: %s",
			names(unattributed), suspectRemedies(unattributed)))
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
	if len(owners) > 0 {
		parts = append(parts, fmt.Sprintf(
			"session(s) %v are live under the clone that held the removed record's own checkout marker — they are this project's: %s",
			titlesOf(owners), suspectRemedies(owners)))
	}
	if len(other) > 0 {
		parts = append(parts, fmt.Sprintf(
			"session(s) %v could not be vetted against the removed record — for the ones that are this project's: %s",
			titlesOf(other), suspectRemedies(other)))
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
