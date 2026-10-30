package git

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Deletion-side guards for `af sessions prune --apply` (#5189). They run inside
// the SAME verdict the dry run uses, so a worktree the apply would refuse is
// never listed as reclaimable.

// archiveNonceFile is the af-owned marker stamped into a worktree's registration
// leaf at archive time. A leaf is recreated by `git worktree add`, which cannot
// reproduce a file af wrote into the original, so unlike a path, a branch name or
// a directory mtime the nonce is not a reusable spelling (#5189, Codex round 9).
const archiveNonceFile = "af-archive-nonce"

// legacyPredateMargin is how far BEFORE the archive time a nonce-less (legacy)
// registration leaf must have been created. A filesystem with coarse timestamps
// can give a re-created leaf the SAME mtime as the original, so "not after the
// archive" accepts it; requiring a margin larger than any real timestamp bucket
// (FAT keeps 2 seconds) makes a re-creation after the archive unable to pass.
const legacyPredateMargin = time.Minute

// StampArchiveNonce writes a fresh nonce into the registration leaf of the
// archived worktree and returns it. The caller records the same value on the
// session row.
func StampArchiveNonce(worktreePath string) (string, error) {
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw[:])
	if err := os.WriteFile(filepath.Join(leaf, archiveNonceFile), []byte(nonce+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("could not stamp the archive occupant nonce: %w", err)
	}
	return nonce, nil
}

func registrationLeaf(worktreePath string) (string, error) {
	return boundedPointerCheckValue("regleaf", worktreePath, func(path string) (string, error) {
		return verifyWorktreePointerShape(path)
	})
}

// VerifyArchiveOccupant proves the worktree at worktreePath is the one that was
// archived. With a recorded nonce the leaf must carry exactly it; without one
// (an archive older than the nonce) the leaf must predate the archive by
// legacyPredateMargin. Either way the answer is an identity-mismatch error, and
// never "unknown ⇒ fine".
func VerifyArchiveOccupant(worktreePath, recordedNonce string, archivedAt time.Time) error {
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return err
	}
	if recordedNonce != "" {
		got, err := os.ReadFile(filepath.Join(leaf, archiveNonceFile))
		if err != nil {
			return worktreeIdentityMismatchf("worktree %s carries no archive nonce (%v) — it is not the archived original", worktreePath, err)
		}
		if strings.TrimSpace(string(got)) != recordedNonce {
			return worktreeIdentityMismatchf("worktree %s carries a different archive nonce — it is not the archived original", worktreePath)
		}
		return nil
	}
	st, err := BoundedLstat(leaf)
	if err != nil {
		return fmt.Errorf("could not stat worktree registration %s: %w", leaf, err)
	}
	if bound := archivedAt.Add(-legacyPredateMargin); st.ModTime().After(bound) {
		return worktreeIdentityMismatchf(
			"worktree %s has no archive nonce (archived before af stamped one) and its registration is not at least %s older than the archive, so it cannot be told from a re-created worktree",
			worktreePath, legacyPredateMargin)
	}
	return nil
}

// WorktreeUnrecoverableContent returns one reason per way the worktree holds
// content the kept branch cannot restore, or an error when that could not be
// established (callers fail closed on an error). It is the dirty probe plus the
// two things `git status` cannot see:
//
//   - index entries flagged assume-unchanged (`h`) or skip-worktree (`S`/`s`):
//     status skips their content, so a modified file reads clean. One
//     `ls-files -v` read covers every non-`H` tag, without touching the index.
//   - initialized submodules: the superproject's `status --ignored` never
//     reports ignored files inside one, so each is probed itself, recursively.
func WorktreeUnrecoverableContent(worktreePath string) ([]string, error) {
	var reasons []string
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return nil, err
	}
	// The two roots prune deletes, resolved once: the worktree tree and its
	// registration leaf.
	deleted := make([]string, 0, 2)
	for _, p := range []string{worktreePath, leaf} {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("could not resolve %s: %w", p, err)
		}
		deleted = append(deleted, real)
	}
	if err := probeUnrecoverable(worktreePath, "", 0, deleted, &reasons); err != nil {
		return nil, err
	}
	return reasons, nil
}

const maxSubmoduleDepth = 8

func probeUnrecoverable(dir, label string, depth int, deleted []string, reasons *[]string) error {
	if depth > maxSubmoduleDepth {
		return fmt.Errorf("submodules under %s nest deeper than %d levels", dir, maxSubmoduleDepth)
	}
	where := ""
	if label != "" {
		where = " in submodule " + label
	}
	dirty, dirtySample, err := WorktreeDirtyEntries(dir)
	if err != nil {
		return err
	}
	if dirty > 0 {
		*reasons = append(*reasons, fmt.Sprintf("%d uncommitted or ignored file(s)%s that the kept branch does not contain (%s) — commit, move or delete them, then re-run",
			dirty, where, sampleList(dirtySample, dirty)))
	}
	out, err := runBoundedWorktreeGit(dir, true, "--no-optional-locks", "-c", "core.fsmonitor=", "ls-files", "-v", "-z")
	if err != nil {
		return fmt.Errorf("could not read the index flags of %s: %w", dir, err)
	}
	flagged := 0
	var flaggedSample []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		if len(rec) < 3 || rec[1] != ' ' {
			return fmt.Errorf("git ls-files -v reported an unreadable record for %s: %q", dir, rec)
		}
		if rec[0] != 'H' {
			flagged++
			if len(flaggedSample) < pruneSamplePaths {
				flaggedSample = append(flaggedSample, rec[2:])
			}
		}
	}
	if flagged > 0 {
		*reasons = append(*reasons, fmt.Sprintf("%d index entr(ies)%s flagged assume-unchanged or skip-worktree (%s) — git status cannot see their content, so it may not be on the kept branch; clear the flag (git update-index --no-assume-unchanged / --no-skip-worktree) and commit or discard the change",
			flagged, where, sampleList(flaggedSample, flagged)))
	}
	subs, err := gitlinkPaths(dir)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		subDir := filepath.Join(dir, filepath.FromSlash(sub))
		if _, err := BoundedLstat(filepath.Join(subDir, ".git")); err != nil {
			if os.IsNotExist(err) {
				continue // not initialized: nothing of its own to lose
			}
			return fmt.Errorf("could not inspect submodule %s: %w", subDir, err)
		}
		next := sub
		if label != "" {
			next = label + "/" + sub
		}
		// Do not enumerate the places a submodule's objects can die — invert the
		// rule. Its real git directory (rev-parse follows gitfile indirection) must
		// provably lie OUTSIDE both roots prune deletes, the worktree tree (a
		// non-absorbed submodule keeps .git inside it) and the registration leaf
		// (an absorbed one keeps it under modules/), or the kept branch's gitlink
		// hash may name a commit that exists nowhere else. One predicate covers
		// absorbed, non-absorbed and nested submodules; anything unresolvable
		// refuses.
		subGitDir, err := runBoundedWorktreeGit(subDir, true, "--no-optional-locks", "rev-parse", "--absolute-git-dir")
		if err != nil {
			return fmt.Errorf("could not locate the git directory of submodule %s: %w", next, err)
		}
		realGitDir, err := filepath.EvalSymlinks(strings.TrimSpace(string(subGitDir)))
		if err != nil {
			return fmt.Errorf("could not resolve the git directory of submodule %s: %w", next, err)
		}
		for _, root := range deleted {
			if within(realGitDir, root) {
				*reasons = append(*reasons, fmt.Sprintf("submodule %s keeps its object database at %s, which pruning deletes — its commits may exist nowhere else; move the submodule's git directory outside this worktree (git submodule absorbgitdirs, then relocate it) or push its commits first", next, realGitDir))
				break
			}
		}
		if err := probeUnrecoverable(subDir, next, depth+1, deleted, reasons); err != nil {
			return err
		}
	}
	return nil
}

// gitlinkPaths lists the index's gitlink (mode 160000) paths.
func gitlinkPaths(dir string) ([]string, error) {
	out, err := runBoundedWorktreeGit(dir, true, "--no-optional-locks", "-c", "core.fsmonitor=", "ls-files", "-s", "-z")
	if err != nil {
		return nil, fmt.Errorf("could not list the gitlinks of %s: %w", dir, err)
	}
	var paths []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if !strings.HasPrefix(rec, "160000 ") {
			continue
		}
		if _, path, ok := strings.Cut(rec, "\t"); ok {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// WorktreeHeadMatchesBranch proves every commit of the worktree is on the kept
// branch: HEAD must resolve to the same object as refs/heads/<branch> in the
// origin. It returns that object id for the intent record.
func WorktreeHeadMatchesBranch(worktreePath, repoRoot, branch string) (string, error) {
	head, err := runBoundedWorktreeGit(worktreePath, false, "--no-optional-locks", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("could not resolve the worktree's HEAD: %w", err)
	}
	tip, err := runBoundedWorktreeGit(repoRoot, false, "--no-optional-locks", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("could not resolve branch %s in %s: %w", branch, repoRoot, err)
	}
	h, t := strings.TrimSpace(string(head)), strings.TrimSpace(string(tip))
	if h != t {
		return "", fmt.Errorf("the worktree is at %s but branch %s is at %s — the kept branch does not hold the worktree's commits", h, branch, t)
	}
	return h, nil
}

// pruneNonceFile is stamped into the registration leaf just before prune records
// its intent, so a later resume can prove a leaf is the one prune meant to remove.
const pruneNonceFile = "af-prune-nonce"

// StampPruneNonce writes a fresh prune nonce into the worktree's registration leaf
// and returns the leaf's name (its directory under <repo>/.git/worktrees) and the
// nonce. Both go into the durable intent.
func StampPruneNonce(worktreePath string) (leafName, nonce string, err error) {
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return "", "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	nonce = hex.EncodeToString(raw[:])
	// Creating a file bumps the leaf directory's mtime, and that mtime is the
	// evidence VerifyArchiveOccupant judges a legacy (nonce-less) archive by.
	// Capture it first and put it back, so stamping never alters the proof a later
	// check relies on (a resume that abandons and re-judges, for one). If the time
	// cannot be restored the stamp is withdrawn and prune refuses before touching
	// anything else.
	st, err := BoundedLstat(leaf)
	if err != nil {
		return "", "", fmt.Errorf("could not stat worktree registration %s: %w", leaf, err)
	}
	stamp := filepath.Join(leaf, pruneNonceFile)
	if err := os.WriteFile(stamp, []byte(nonce+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("could not stamp the prune nonce: %w", err)
	}
	if err := os.Chtimes(leaf, st.ModTime(), st.ModTime()); err != nil {
		_ = os.Remove(stamp)
		_ = os.Chtimes(leaf, st.ModTime(), st.ModTime())
		return "", "", fmt.Errorf("could not preserve the registration's timestamp while stamping: %w", err)
	}
	return filepath.Base(leaf), nonce, nil
}

// ErrRegistrationNotOurs marks a registration at the recorded leaf that is not
// the one prune recorded (a different worktree now occupies the name).
var ErrRegistrationNotOurs = fmt.Errorf("the registration at the recorded leaf is not the pruned worktree's")

// RemoveArchivedRegistrationLeaf removes the registration leaf prune recorded, and
// ONLY that leaf, and only when it still carries the recorded nonce. `git worktree
// prune` would drop every stale registration in the repository, including other
// sessions'; a path-only match would drop a replacement that now occupies the
// original path. A leaf already gone is success; one that does not match is left
// alone and reported as ErrRegistrationNotOurs; a locked one is refused.
func RemoveArchivedRegistrationLeaf(repoRoot, leafName, nonce string) error {
	if leafName == "" || nonce == "" || leafName != filepath.Base(leafName) {
		return fmt.Errorf("no recorded registration to remove")
	}
	meta, err := resolveRepoMetadataRoot(repoRoot)
	if err != nil {
		return err
	}
	leaf := filepath.Join(meta, "worktrees", leafName)
	if _, err := os.Lstat(leaf); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	got, err := os.ReadFile(filepath.Join(leaf, pruneNonceFile))
	if err != nil || strings.TrimSpace(string(got)) != nonce {
		return fmt.Errorf("%w (%s)", ErrRegistrationNotOurs, leaf)
	}
	if _, err := os.Lstat(filepath.Join(leaf, "locked")); err == nil {
		return fmt.Errorf("registration %s is locked — refusing to remove it", leaf)
	}
	return os.RemoveAll(leaf)
}

// within reports whether path is dir or lies beneath it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func sampleList(sample []string, total int) string {
	out := strings.Join(sample, ", ")
	if total > len(sample) {
		out += fmt.Sprintf(", … +%d more", total-len(sample))
	}
	return out
}
