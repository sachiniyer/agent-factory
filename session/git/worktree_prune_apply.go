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
// established (callers fail closed on an error). It is the dirty probe plus what
// `git status` cannot see:
//
//   - index entries flagged assume-unchanged (`h`) or skip-worktree (`S`/`s`):
//     status skips their content, so a modified file reads clean. One
//     `ls-files -v` read covers every non-`H` tag, without touching the index.
//   - ANY submodule. A worktree whose index contains a gitlink (mode 160000),
//     initialized or not, nested or not, is refused outright. The kept branch holds
//     only the gitlink's commit hash, while the submodule's objects and local files
//     can live in the tree, in the registration, or nowhere prune can see, and the
//     "is this submodule's content safe to delete" predicate kept meeting new
//     shapes (#5189, three P1s). Supporting submodule worktrees is a separate,
//     separately reviewed change.
func WorktreeUnrecoverableContent(worktreePath string) ([]string, error) {
	var reasons []string
	dirty, dirtySample, err := WorktreeDirtyEntries(worktreePath)
	if err != nil {
		return nil, err
	}
	if dirty > 0 {
		reasons = append(reasons, fmt.Sprintf("%d uncommitted or ignored file(s) that the kept branch does not contain (%s) — commit, move or delete them, then re-run",
			dirty, sampleList(dirtySample, dirty)))
	}
	out, err := runBoundedWorktreeGit(worktreePath, true, "--no-optional-locks", "-c", "core.fsmonitor=", "ls-files", "-v", "-z")
	if err != nil {
		return nil, fmt.Errorf("could not read the index flags of %s: %w", worktreePath, err)
	}
	flagged := 0
	var flaggedSample []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		if len(rec) < 3 || rec[1] != ' ' {
			return nil, fmt.Errorf("git ls-files -v reported an unreadable record for %s: %q", worktreePath, rec)
		}
		if rec[0] != 'H' {
			flagged++
			if len(flaggedSample) < pruneSamplePaths {
				flaggedSample = append(flaggedSample, rec[2:])
			}
		}
	}
	if flagged > 0 {
		reasons = append(reasons, fmt.Sprintf("%d index entr(ies) flagged assume-unchanged or skip-worktree (%s) — git status cannot see their content, so it may not be on the kept branch; clear the flag (git update-index --no-assume-unchanged / --no-skip-worktree) and commit or discard the change",
			flagged, sampleList(flaggedSample, flagged)))
	}
	// The principle, not a list of shapes: prune deletes the worktree tree and its
	// registration leaf, so neither may hold ANY git object storage — however it got
	// there (absorbed, non-absorbed, retained after the gitlink was removed, a nested
	// repository). Four P1s were one class: objects hiding under a deleted root.
	storage, err := gitObjectStorageUnder(worktreePath)
	if err != nil {
		return nil, err
	}
	if len(storage) > 0 {
		reasons = append(reasons, fmt.Sprintf("git object storage lives inside what pruning deletes (%s) — its commits may exist nowhere else; pruning does not touch such worktrees, so preserve or relocate that repository first",
			sampleList(firstN(storage, pruneSamplePaths), len(storage))))
	}
	subs, err := gitlinkPaths(worktreePath)
	if err != nil {
		return nil, err
	}
	if len(subs) > 0 {
		reasons = append(reasons, fmt.Sprintf("the worktree contains %d submodule(s) (%s) — pruning does not support submodules, because their objects and local files are not covered by the kept branch; remove the archive by hand after preserving them",
			len(subs), sampleList(firstN(subs, pruneSamplePaths), len(subs))))
	}
	return reasons, nil
}

func firstN(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
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

func sampleList(sample []string, total int) string {
	out := strings.Join(sample, ", ")
	if total > len(sample) {
		out += fmt.Sprintf(", … +%d more", total-len(sample))
	}
	return out
}

// gitObjectStorageUnder reports git object storage inside either root prune
// deletes — the worktree tree and its registration leaf — at any depth:
//
//   - a git directory (a directory holding HEAD and an objects/ directory),
//   - a nested .git directory,
//   - a gitfile (.git file) whose target resolves inside either root.
//
// The worktree's own root .git pointer is exempt: it is the link to the main
// repository's object store (commondir), which prune does not touch. The walk
// follows no symlinks, containment is judged after EvalSymlinks, and any read error
// refuses (it is returned, and the caller fails closed).
func gitObjectStorageUnder(worktreePath string) ([]string, error) {
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, p := range []string{worktreePath, leaf} {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("could not resolve %s: %w", p, err)
		}
		roots = append(roots, real)
	}
	var found []string
	for _, root := range roots {
		rootPointer := filepath.Join(roots[0], ".git")
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name := d.Name()
			switch {
			case d.IsDir() && name == ".git":
				found = append(found, path)
				return filepath.SkipDir
			case d.IsDir() && name == "objects":
				if _, err := os.Lstat(filepath.Join(filepath.Dir(path), "HEAD")); err == nil {
					found = append(found, filepath.Dir(path))
					return filepath.SkipDir
				} else if !os.IsNotExist(err) {
					return err
				}
			case !d.IsDir() && name == ".git" && path != rootPointer:
				target, err := gitfileTarget(path)
				if err != nil {
					return err
				}
				for _, r := range roots {
					if within(target, r) {
						found = append(found, path+" → "+target)
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("could not scan %s for git object storage: %w", root, err)
		}
	}
	return found, nil
}

// gitfileTarget resolves the directory a gitfile points at ("gitdir: <path>"),
// relative to the gitfile's own directory, with symlinks resolved when the target
// exists.
func gitfileTarget(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	line := strings.TrimSpace(string(buf[:n]))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s is a .git file but not a gitdir pointer", path)
	}
	target := strings.TrimSpace(rest)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target = filepath.Clean(target)
	if real, err := filepath.EvalSymlinks(target); err == nil {
		return real, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return target, nil
}

// within reports whether path is dir or lies beneath it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
