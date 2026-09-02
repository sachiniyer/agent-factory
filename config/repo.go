package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// repoIDPattern restricts a repoID to characters that are safe to use as a
// single path segment. Legitimate IDs from RepoIDFromRoot are 12 lowercase
// hex characters; tests and any future ID schemes are constrained to the
// same character class so the value can never escape its parent directory.
var repoIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ErrNotGitRepository identifies the one CurrentRepo failure for which callers
// may deliberately fall back to global scope. Other git failures (missing git,
// an invalid .git file, safe-directory rejection, permissions) must remain
// visible rather than being mistaken for "outside git."
var ErrNotGitRepository = errors.New("not inside a git repository")

// ErrRepoProbeUnanswered marks a repository resolution that failed WITHOUT AN
// ANSWER: the git child could not be started, died on a signal, or was
// abandoned mid-read when its WaitDelay expired. It is the deliberate
// complement of ErrNotGitRepository — "we could not ask git" as against "we
// asked and the answer is no" — so a caller narrating a resolution failure can
// tell a claim about the PATH apart from a claim about the SUBPROCESS (#3500).
//
// Callers test it with errors.Is. Errors carrying it keep their original text:
// the sentinel is joined into the chain rather than wrapped around the message,
// because the classification is for code and the wording belongs to whichever
// call site narrates the failure.
var ErrRepoProbeUnanswered = errors.New("git repository probe did not complete")

// unansweredProbeError joins ErrRepoProbeUnanswered into an error's chain
// without changing what the error prints.
type unansweredProbeError struct{ err error }

func (e *unansweredProbeError) Error() string   { return e.err.Error() }
func (e *unansweredProbeError) Unwrap() []error { return []error{ErrRepoProbeUnanswered, e.err} }

// markUnansweredProbe classifies a failed git invocation: err marked with
// ErrRepoProbeUnanswered when nothing proves git answered, and err untouched
// when it did.
//
// CLASSIFICATION and ATTRIBUTION are separate steps here, and the order is the
// whole point. What happened is decided from the command's own outcome and
// nothing else, so a deadline expiring after a completed ExitError can never
// retag an answer as unknown (#3500 review). Only once the outcome says
// unanswered is ctx consulted, and then only to say WHY: a caller whose own
// deadline killed the child gets `context.DeadlineExceeded` in the chain and
// can test for it, instead of an opaque "signal: killed" (#3517).
func markUnansweredProbe(ctx context.Context, err error) error {
	if err == nil || !probeWentUnanswered(err) {
		return err
	}
	if cause := ctx.Err(); cause != nil {
		return &unansweredProbeError{err: fmt.Errorf("%w (the caller's context ended first: %w)", err, cause)}
	}
	return &unansweredProbeError{err: err}
}

// probeWentUnanswered reports whether a failed git invocation left the question
// unanswered.
//
// The test is deliberately INVERTED: an answer is proven only by an
// *exec.ExitError carrying a real exit status — git ran, diagnosed, and exited
// — and every other outcome is unanswered. Enumerating the failure modes the
// other way round is how #3500 happened in the first place; that list is never
// complete, and each gap becomes a fabricated verdict about a user's path. Some
// of what the inverted rule catches for free:
//
//   - a fork/exec failure, which is an *fs.PathError and matches NEITHER
//     exec error type (measured: "exec format error", and a box out of process
//     slots produces the same shape with EAGAIN);
//   - Cmd.WaitDelay expiring before the output pipe closed, so the read was
//     abandoned mid-flight — #3500 was observed on a host whose load baseline
//     is 60-95, where the then-100ms allowance was inside the drain's own noise
//     (repoGitWaitDelay carries that measurement and why it is now 2s, #3503);
//   - a signalled death, which ProcessState reports as a negative exit code;
//   - a cancelled context, whose child CommandContext kills — hence signalled;
//   - anything that loses the output after a clean exit.
//
// It deliberately does NOT consult ctx.Err(): a deadline that expires between
// Output returning a completed ExitError and this call would otherwise retag an
// answer as unknown (#3500 review). The command's own outcome is the evidence.
func probeWentUnanswered(err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.ProcessState.ExitCode() >= 0 {
		return false
	}
	return true
}

// RepoProbeUnanswered reports whether a repository-resolution failure left the
// question unanswered — the git child was never started, was killed, or its
// output was abandoned — rather than answered with a verdict about the path.
//
// It is the predicate every site that NARRATES such a failure should branch on
// (#3504). "git answered, and the answer is no" and "we could not ask git" are
// different states, and only the first entitles a caller to tell a user that
// their path is not a repository.
func RepoProbeUnanswered(err error) bool {
	return errors.Is(err, ErrRepoProbeUnanswered)
}

// ClassifyGitProbeError marks a failed git invocation with ErrRepoProbeUnanswered
// when nothing proves git answered, for callers that run a repository probe of
// their OWN rather than going through RepoFromPath (session/git's repo check,
// doctor's setup probe). Sharing the classifier is what keeps those callers from
// re-deriving the rule and drifting from it — the enumeration this replaced had
// a measured gap (a fork/exec failure is an *fs.PathError, matching neither exec
// error type), and a second copy would grow its own.
//
// It returns err unchanged when the outcome proves git answered.
func ClassifyGitProbeError(ctx context.Context, err error) error {
	return markUnansweredProbe(ctx, err)
}

// RepoProbeUnansweredClaim words what an unanswered probe entitles a caller to
// say about path. subject names the thing being resolved ("--repo", "project
// path", "root_agents entry"), so one sentence serves every surface and cannot
// drift between them. The ANSWERED half stays with each call site, whose
// existing copy is already correct for a real verdict.
func RepoProbeUnansweredClaim(subject, path string) string {
	return fmt.Sprintf("%s %q could not be checked: git never answered the probe (the subprocess was killed, could not be started, or was abandoned mid-read), so whether the path is a git repository is unknown", subject, path)
}

// maxRepoIDLength caps the size of an accepted repoID. Legitimate IDs are
// 12 chars; the cap is loose enough to accommodate future schemes while
// preventing unbounded allocation in path joins or error messages.
const maxRepoIDLength = 128

// repoGitWaitDelay bounds how long Output keeps waiting for the child's pipes
// to reach EOF once the child has exited or the caller's context is done.
//
// WHAT IT CANNOT DO, and the old comment here claimed otherwise (#3503). That
// comment called this "the fail-closed edge for hooks/wrappers around git on a
// stale mount". It is not, and no value of this constant could make it one:
// Cmd.WaitDelay's timer starts only when the context is done OR the child has
// exited, and the entry points that matter — RepoFromPath and CurrentRepo —
// pass context.Background(). A git wedged on a stale mount never exits and that
// context is never done, so the timer never starts. Measured: with a child that
// sleeps forever and WaitDelay at 100ms, Output was still blocked after 3s.
// Bounding that hang needs a deadline on the CALLER's context, which those two
// entry points deliberately do not have (see RepoFromPathContext's contract).
//
// WHY 2s AND NOT THE PREVIOUS 100ms. What this actually bounds is the parent's
// own pipe drain, and 100ms sat inside the noise of a loaded box. Measured over
// 300 `git rev-parse --show-toplevel` runs at load ~95 on a 16-core host, the
// gap from git's last write to Wait returning was p50 1.0ms, p90 5.3ms, p99
// 33.7ms — and max 184ms, past the old budget. No helper can explain that one:
// strace shows rev-parse performs exactly one execve and never forks, so
// nothing exists to inherit the pipe. The old value turned ordinary scheduler
// latency into a failed repository probe.
//
// And 100ms was too tight even for the case it WAS written for. With a helper
// genuinely holding the pipe, Output returned ErrWaitDelay at 104ms while
// already holding git's complete, correct answer (measured). Discarding an
// answer you are already holding is not fail-closed; it is just lossy. 2s is
// what every other WaitDelay in this repo uses for this same mechanism —
// session/git, session/tmux, hooks, github, docker, the daemon probes.
//
// A var, not a const, only so tests can drive both values; production never
// reassigns it.
var repoGitWaitDelay = 2 * time.Second

// minRepoProbeWaitDelay floors the allowance repoProbeWaitDelay computes.
//
// The floor is not cosmetic: WaitDelay == 0 means NO BOUND AT ALL, so a caller
// whose deadline has all but elapsed must never be able to arithmetic its way
// into disabling the guard entirely.
const minRepoProbeWaitDelay = 50 * time.Millisecond

// repoProbeWaitDelay picks the drain allowance for one probe, from the caller's
// own promise rather than from a single global number (#3503, question 2).
//
// The axis that matters is not which git command runs — since #3500 every probe
// here is load-bearing, because an unanswered one fails the whole resolution
// rather than falling back — but whether the CALLER imposed a deadline:
//
//   - No deadline (RepoFromPath, CurrentRepo): there is no budget to overrun,
//     so the probe takes the full allowance. Being generous costs a bounded
//     extra wait in the rare inherited-pipe case; being stingy costs a spurious
//     failure on every loaded box, which is what #3503 reported.
//   - A deadline (ResolveRegisteredProjectRepoID gives these probes 250ms
//     inside a 1s registry scan; app's project-path scan gives them 150ms):
//     the allowance is the time the caller has left, so it tracks each
//     caller's own value instead of hardcoding copies of them here.
//
// BE PRECISE ABOUT WHAT THE DEADLINE CASE BUYS, because the obvious phrasing —
// "the drain fits inside the caller's budget" — is FALSE and an earlier draft
// of this comment said it (#3594 review). WaitDelay's timer starts when the
// context is done, so an allowance granted there is ADDED to the deadline, not
// carved out of it: a 250ms probe whose git is still alive at the deadline and
// whose pipe is held by a descendant can take ~500ms.
//
// What it actually guarantees is that the overrun is PROPORTIONAL TO WHAT THE
// CALLER ASKED FOR and never the global default: a caller that promised 150ms
// can be late by another 150ms, but nothing can drag it to 2.15s. That is the
// property worth having, and it is the one the tests assert.
//
// Nor can a bounded caller have both. The measured drain tail is ~184ms, so any
// budget below that cannot simultaneously clear the drain and honour itself.
// The tie breaks toward the budget, and only for these callers, because a
// caller that set a deadline has ALREADY accepted it may not get an answer in
// time and has a fallback for that. An unbounded caller has made no such
// peace, which is exactly why it never trades an answer for time.
func repoProbeWaitDelay(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return repoGitWaitDelay
	}
	remaining := time.Until(deadline)
	if remaining >= repoGitWaitDelay {
		return repoGitWaitDelay
	}
	if remaining < minRepoProbeWaitDelay {
		return minRepoProbeWaitDelay
	}
	return remaining
}

// ValidateRepoID enforces the shape of a repository identifier before it is
// used to construct a filesystem path. Returns an error when the id is
// empty, exceeds maxRepoIDLength, or contains any character outside
// [a-zA-Z0-9_-] — in particular, "." (used in traversal), "/", or "\".
// Callers that legitimately accept an empty id as "all repos" must check
// that case before calling this function.
func ValidateRepoID(repoID string) error {
	if repoID == "" {
		return fmt.Errorf("invalid repo id: empty")
	}
	if len(repoID) > maxRepoIDLength {
		return fmt.Errorf("invalid repo id: length %d exceeds maximum %d", len(repoID), maxRepoIDLength)
	}
	if !repoIDPattern.MatchString(repoID) {
		return fmt.Errorf("invalid repo id: must match %s", repoIDPattern.String())
	}
	return nil
}

// repoRootResolution keeps repository identity separate from the checkout a
// caller asked to use as its workspace. They normally coincide. For a linked
// worktree they differ, and for a bare repository's worktree there is no main
// working tree at all: identityRoot is the bare common directory while
// workspaceRoot is the linked worktree.
type repoRootResolution struct {
	identityRoot       string
	workspaceRoot      string
	legacyIdentityRoot string
}

// resolveMainRepoRoot returns the repository's identity root, resolving
// through linked worktrees so that every worktree gets the same repo ID. For a
// bare repository this is the bare directory itself. pathArgs should be empty
// for cwd, or []string{"-C", path}.
func resolveMainRepoRoot(pathArgs ...string) (string, error) {
	resolved, err := resolveRepoRoots(pathArgs...)
	if err != nil {
		return "", err
	}
	return resolved.identityRoot, nil
}

func resolveRepoRoots(pathArgs ...string) (repoRootResolution, error) {
	return resolveRepoRootsContext(context.Background(), pathArgs...)
}

func resolveRepoRootsContext(ctx context.Context, pathArgs ...string) (repoRootResolution, error) {
	// Get the toplevel for the current location
	topCmd := exec.CommandContext(ctx, "git", append(pathArgs, "rev-parse", "--show-toplevel")...)
	topCmd.WaitDelay = repoProbeWaitDelay(ctx)
	// The outside-repository classification below parses Git's diagnostic. Force
	// that one command to the locale the parser expects so a translated stderr
	// cannot turn ordinary absence into a fatal scope-resolution error (#3134).
	topCmd.Env = append(os.Environ(), "LC_ALL=C")
	topOut, err := topCmd.Output()
	if err != nil {
		// A bare repository has no toplevel, but it is still a valid identity
		// root. Accept that direct shape so callers holding the resolved identity
		// can continue to address repo-scoped state.
		bareRoot, bareErr := resolveDirectBareRepoRootContext(ctx, pathArgs...)
		if bareErr == nil && bareRoot != "" {
			return repoRootResolution{identityRoot: bareRoot, workspaceRoot: bareRoot}, nil
		}
		// The stderr verdict is only as good as the process that produced it.
		// A diagnostic written by a git that was then killed — a signal, our own
		// cancellation landing in the window between the write and the exit —
		// proves nothing, so a COMPLETED exit is required before that text is
		// read as an answer (#3500 review round 4). Without this gate the one
		// branch that returns a definite verdict is also the one branch that
		// never consults the classifier.
		var exitErr *exec.ExitError
		if !probeWentUnanswered(err) && errors.As(err, &exitErr) &&
			(strings.Contains(string(exitErr.Stderr), "not a git repository (or any of the parent directories)") ||
				strings.Contains(string(exitErr.Stderr), "not a git repository (or any parent up to mount point")) &&
			!gitMetadataMayExist(pathArgs...) {
			return repoRootResolution{}, fmt.Errorf("%w: %s", ErrNotGitRepository, strings.TrimSpace(string(exitErr.Stderr)))
		}
		// The bare shape is checked by a SECOND probe, and its outcome has to be
		// classified too (#3500 review). A bare repository's toplevel refusal is
		// an answer about a work tree, not about whether this is a repository at
		// all; if the probe that would have settled that was killed, the
		// question is open however cleanly the first one failed. The definite
		// outside-repository answer above still wins — that one needs no bare
		// probe to be true — and an ordinary non-repo path is unaffected,
		// because there the bare probe answers (exit 128) like the first.
		resolveErr := err
		if !probeWentUnanswered(err) && bareErr != nil && probeWentUnanswered(bareErr) {
			resolveErr = bareErr
		}
		return repoRootResolution{}, fmt.Errorf("failed to get git repo root: %w", markUnansweredProbe(ctx, resolveErr))
	}
	toplevel := trimGitOutputLine(topOut)

	// Get git-dir and git-common-dir to detect if we're in a linked worktree.
	// In the main repo: git-dir == ".git", git-common-dir == ".git"
	// In a worktree:    git-dir == "<main>/.git/worktrees/<name>",
	//                   git-common-dir == "<main>/.git"
	infoCmd := exec.CommandContext(ctx, "git", "-C", toplevel, "rev-parse", "--git-dir", "--git-common-dir")
	infoCmd.WaitDelay = repoProbeWaitDelay(ctx)
	infoOut, err := infoCmd.Output()
	if err != nil {
		// Classified from the command's own outcome, never from ctx state
		// (#3500 review): git ANSWERING with an error is what authorizes the
		// main-worktree fallback below — an old git, a shape it will not
		// describe — and a probe that never answered authorizes nothing.
		// Falling back there would hand a linked worktree its own toplevel as
		// its identity root, silently splitting one repository's ID in two.
		if probeWentUnanswered(err) {
			return repoRootResolution{}, fmt.Errorf("failed to inspect git directories for %s: %w", toplevel, markUnansweredProbe(ctx, err))
		}
		return repoRootResolution{identityRoot: toplevel, workspaceRoot: toplevel}, nil
	}
	parts := strings.SplitN(trimGitOutputLine(infoOut), "\n", 2)
	if len(parts) != 2 {
		return repoRootResolution{identityRoot: toplevel, workspaceRoot: toplevel}, nil
	}
	gitDir := parts[0]
	commonDir := parts[1]

	// If they're equal, we're in the main working tree
	if gitDir == commonDir {
		return repoRootResolution{identityRoot: toplevel, workspaceRoot: toplevel}, nil
	}

	// Resolve commonDir to an absolute path
	if !filepath.IsAbs(commonDir) {
		if filepath.IsAbs(gitDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		} else {
			commonDir = filepath.Join(toplevel, gitDir, commonDir)
		}
	}
	commonDir = filepath.Clean(commonDir)
	bare, err := gitDirIsBareContext(ctx, commonDir)
	if err != nil {
		return repoRootResolution{}, err
	}
	if bare {
		return repoRootResolution{
			identityRoot:       commonDir,
			workspaceRoot:      toplevel,
			legacyIdentityRoot: filepath.Dir(commonDir),
		}, nil
	}

	// commonDir is the main repo's .git directory.
	// For submodules, git stores the worktree path in core.worktree inside the git dir.
	// For regular repos, core.worktree is unset and the parent of .git is the repo root.
	wtCmd := exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(commonDir, "config"), "core.worktree")
	wtCmd.WaitDelay = repoProbeWaitDelay(ctx)
	wtOut, err := wtCmd.Output()
	if err == nil {
		worktree := trimGitOutputLine(wtOut)
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(commonDir, worktree)
		}
		return repoRootResolution{identityRoot: filepath.Clean(worktree), workspaceRoot: toplevel}, nil
	}
	// Same rule as the probe above: an ANSWER authorizes the fallback. Here the
	// answer is usually "no such key", which is exactly how a non-submodule repo
	// reports itself; a killed probe cannot tell that apart from a submodule.
	if probeWentUnanswered(err) {
		return repoRootResolution{}, fmt.Errorf("failed to read core.worktree for %s: %w", commonDir, markUnansweredProbe(ctx, err))
	}
	// Fallback: parent of .git directory (correct for non-submodule repos)
	return repoRootResolution{identityRoot: filepath.Dir(commonDir), workspaceRoot: toplevel}, nil
}

func resolveDirectBareRepoRootContext(ctx context.Context, pathArgs ...string) (string, error) {
	bareCmd := exec.CommandContext(ctx, "git", append(pathArgs, "rev-parse", "--is-bare-repository")...)
	bareCmd.WaitDelay = repoProbeWaitDelay(ctx)
	bareOut, err := bareCmd.Output()
	if err != nil || strings.TrimSpace(string(bareOut)) != "true" {
		return "", err
	}
	dirCmd := exec.CommandContext(ctx, "git", append(pathArgs, "rev-parse", "--absolute-git-dir")...)
	dirCmd.WaitDelay = repoProbeWaitDelay(ctx)
	dirOut, err := dirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve bare git directory: %w", err)
	}
	return filepath.Clean(trimGitOutputLine(dirOut)), nil
}

// trimGitOutputLine removes Git's record terminator without stripping bytes
// that are valid in a path. In particular, TrimSpace would corrupt repository
// directories whose names end in a space or tab.
func trimGitOutputLine(out []byte) string {
	return strings.TrimSuffix(string(out), "\n")
}

func gitDirIsBareContext(ctx context.Context, gitDir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "rev-parse", "--is-bare-repository")
	cmd.WaitDelay = repoProbeWaitDelay(ctx)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to inspect git common directory %s: %w", gitDir, markUnansweredProbe(ctx, err))
	}
	switch value := strings.TrimSpace(string(out)); value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("failed to inspect git common directory %s: unexpected bare result %q", gitDir, value)
	}
}

// gitMetadataMayExist fails closed when a failed rev-parse could be caused by
// broken or unreadable repository metadata. Git reports the same no-ancestor
// diagnostic for an unreadable .git directory that it reports outside Git, so
// stderr alone cannot authorize a global-config fallback.
func gitMetadataMayExist(pathArgs ...string) bool {
	if os.Getenv("GIT_DIR") != "" {
		return true
	}
	start := "."
	if len(pathArgs) == 2 && pathArgs[0] == "-C" {
		start = pathArgs[1]
	}
	current, err := filepath.Abs(start)
	if err != nil {
		return true
	}
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// RepoContext identifies a git repository and provides scoped path resolution.
type RepoContext struct {
	Root         string // operational root: normally the main worktree; a bare repo's linked worktree
	IdentityRoot string // path hashed for ID: Root normally, the bare repository directory for #3358
	ID           string // first 12 hex chars of SHA-256(IdentityRoot)
	legacyRoot   string // pre-#3358 parent identity for a bare linked worktree
}

// WorkspacePath returns the checkout on which workspace operations should run.
// Root already carries that contract; the method makes identity-sensitive call
// sites state which role they need.
func (r *RepoContext) WorkspacePath() string {
	if r == nil {
		return ""
	}
	return r.Root
}

// IdentityPath returns the canonical path from which repo.ID is derived.
// Hand-built RepoContexts predate IdentityRoot and fall back to Root.
func (r *RepoContext) IdentityPath() string {
	if r != nil && r.IdentityRoot != "" {
		return r.IdentityRoot
	}
	if r == nil {
		return ""
	}
	return r.Root
}

// LegacyBareRepoIdentity reports the incorrect parent-derived identity used by
// releases before #3358. It is empty for every other repository shape.
func (r *RepoContext) LegacyBareRepoIdentity() (root, id string) {
	if r == nil || r.legacyRoot == "" {
		return "", ""
	}
	return r.legacyRoot, RepoIDFromRoot(r.legacyRoot)
}

// CurrentRepo returns the RepoContext for the git repository containing cwd.
// An ordinary linked worktree resolves to its main worktree. A bare repository
// has no main worktree, so Root remains the requesting checkout and
// IdentityRoot names the bare common directory.
func CurrentRepo() (*RepoContext, error) {
	resolved, err := resolveRepoRoots()
	if err != nil {
		return nil, err
	}
	return repoContextFromResolution(resolved), nil
}

// RepoFromPath returns the RepoContext for the git repository at the given path.
// An ordinary linked worktree resolves to its main worktree. A bare repository
// keeps the requesting checkout as Root and the bare directory as IdentityRoot.
func RepoFromPath(path string) (*RepoContext, error) {
	return RepoFromPathContext(context.Background(), path)
}

// RepoFromPathContext is RepoFromPath with caller-owned cancellation. Polling
// and registry scans use it so one unreachable checkout cannot indefinitely
// block an unrelated live project; admission paths retain RepoFromPath's full
// error contract and unbounded caller lifetime.
func RepoFromPathContext(ctx context.Context, path string) (*RepoContext, error) {
	resolved, err := resolveRepoRootsContext(ctx, "-C", path)
	if err != nil {
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, err)
	}
	return repoContextFromResolution(resolved), nil
}

// ResolveMainRepoRoot returns the repository's identity root. For an ordinary
// linked worktree that is the main working tree; for a bare repository it is
// the bare directory. All worktrees of one repository therefore share it.
func ResolveMainRepoRoot(path string) (string, error) {
	return resolveMainRepoRoot("-C", path)
}

// RepoIDFromRoot computes a repo ID from an absolute repo root path.
func RepoIDFromRoot(root string) string {
	hash := sha256.Sum256([]byte(root))
	return hex.EncodeToString(hash[:6])
}

// RepoIDForPath resolves an available path to its repository identity and
// falls back to the historical raw-path hash when it cannot be resolved. It is
// for compatibility/display paths that must keep unavailable records
// addressable; admission decisions that need a proven repository use
// RepoFromPath directly and surface its error.
func RepoIDForPath(path string) string {
	if repo, err := RepoFromPath(path); err == nil {
		return repo.ID
	}
	return RepoIDFromRoot(path)
}

// derivedRepoIDPrefix marks an identity af INVENTED for a path, keeping it out
// of the value space RepoIDFromRoot produces. It is a legal repoID segment
// (ValidateRepoID admits [a-zA-Z0-9_-]) so a derived id can still key state and
// appear in paths and errors.
const derivedRepoIDPrefix = "d-"

// DerivedRepoIDForUnresolvedRoot invents an identity for a recorded root that
// has NEVER been seen to resolve, and it is deliberately a value no real
// repository can hold (#3530).
//
// One function used to serve two jobs — "hash a path already known to be the
// identity root", where the answer MUST be the real id, and "invent an identity
// for a path that did not resolve", where it must never be. Sharing a value
// space made a project recorded at a path and a repository later main-rooted
// there bit-identical, so seven separate consumers each needed a guard to tell
// them apart after the fact (PR #3334). Splitting the roles makes the question
// unaskable: this range and RepoIDFromRoot's are disjoint by construction.
//
// It is a LAST RESORT, not the normal path. A registered project records the
// real identity it resolved to (Project.RepoID), so an absent path is still
// addressed by its own repository's id; this covers only a record written
// before that field existed and not yet re-resolved. See
// ReconciledRepoIDForProject.
func DerivedRepoIDForUnresolvedRoot(recorded string) string {
	return derivedRepoIDPrefix + RepoIDFromRoot(filepath.Clean(recorded))
}

// IsDerivedRepoID reports whether id was invented for an unresolved path rather
// than resolved from a repository.
func IsDerivedRepoID(id string) bool {
	return strings.HasPrefix(id, derivedRepoIDPrefix)
}

func repoContextFromRoot(root string) *RepoContext {
	return &RepoContext{
		Root:         root,
		IdentityRoot: root,
		ID:           RepoIDFromRoot(root),
	}
}

func repoContextFromResolution(resolved repoRootResolution) *RepoContext {
	root := resolved.identityRoot
	if resolved.legacyIdentityRoot != "" {
		// A bare repository has no main working tree. Its linked worktree is
		// therefore the operational root even though the bare common directory
		// remains the stable identity shared by all of its worktrees.
		root = resolved.workspaceRoot
	}
	return &RepoContext{
		Root:         root,
		IdentityRoot: resolved.identityRoot,
		ID:           RepoIDFromRoot(resolved.identityRoot),
		legacyRoot:   resolved.legacyIdentityRoot,
	}
}

// ResolvedProject is a recorded project path resolved to its owning repository.
// Root is "" when nothing could be resolved, which is what makes an invented
// identity distinguishable from a real one.
type ResolvedProject struct {
	ID   string
	Root string
}

// ResolveProjectPath maps a recorded project path to its owning repository.
//
// An EXISTING path — including a subdirectory or a linked worktree — resolves
// through git to the repository identity, which is why identity matching
// (rather than path-string equality) sees a task in its own project no matter
// which directory it was created from.
//
// A path that no longer exists is the hard case. Hashing the stale leaf invents
// an ID that equals nothing: the surviving project's ID is sha256 of ITS root,
// never of a deleted child. That strands the record — hidden from its project
// and unaddressable. So walk up to the nearest ANCESTOR that still resolves: a
// deleted subdirectory of a surviving project resolves back to that project.
//
// The walk answers what git itself would say about the path if it existed, so
// it cannot be more wrong than the path is. When nothing up the chain resolves,
// fall back to the leaf hash — path equality, which at least keeps an orphan
// addressable at its own recorded path — and report Root "" so callers can tell
// this identity is derived rather than real.
//
// This is the ONE path→project-identity mechanism. It backs both the CLI's
// project scoping (api/scope.go, #1893) and the TUI task pane's repo filter
// (task.LoadTasksForRepo, #2098); they were separate rules until the latter's
// raw path equality hid subdirectory-created tasks from their own pane.
func ResolveProjectPath(projectPath string) ResolvedProject {
	if repo, err := RepoFromPath(projectPath); err == nil {
		return ResolvedProject{ID: repo.ID, Root: repo.Root}
	}
	cleaned := filepath.Clean(projectPath)
	// Only walk an ABSOLUTE path. A relative one has no meaning independent of
	// the current directory, so climbing it reaches "." and resolves to whatever
	// repository the caller happens to be standing in — adopting a record into
	// the current project on no evidence at all. That is the same invented
	// identity the leaf hash produced, just harder to spot, so a relative path
	// degrades to path equality instead. No supported writer records one (the
	// CLI stores RepoContext.Root, the TUI an absolute path), so this only guards
	// hand-edited rows.
	if filepath.IsAbs(cleaned) {
		for dir := filepath.Dir(cleaned); ; dir = filepath.Dir(dir) {
			if repo, err := RepoFromPath(dir); err == nil {
				return ResolvedProject{ID: repo.ID, Root: repo.Root}
			}
			if parent := filepath.Dir(dir); parent == dir {
				break // reached the filesystem root
			}
		}
	}
	return ResolvedProject{ID: RepoIDFromRoot(cleaned)}
}
