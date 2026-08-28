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

// maxRepoIDLength caps the size of an accepted repoID. Legitimate IDs are
// 12 chars; the cap is loose enough to accommodate future schemes while
// preventing unbounded allocation in path joins or error messages.
const maxRepoIDLength = 128

// repoGitWaitDelay bounds Output waiting on a pipe inherited by a helper that
// outlives a cancelled git process. The normal rev-parse path has no helpers;
// this is the fail-closed edge for hooks/wrappers around git on a stale mount.
const repoGitWaitDelay = 100 * time.Millisecond

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
	topCmd.WaitDelay = repoGitWaitDelay
	// The outside-repository classification below parses Git's diagnostic. Force
	// that one command to the locale the parser expects so a translated stderr
	// cannot turn ordinary absence into a fatal scope-resolution error (#3134).
	topCmd.Env = append(os.Environ(), "LC_ALL=C")
	topOut, err := topCmd.Output()
	if err != nil {
		// A bare repository has no toplevel, but it is still a valid identity
		// root. Accept that direct shape so callers holding the resolved identity
		// can continue to address repo-scoped state.
		if bareRoot, bareErr := resolveDirectBareRepoRootContext(ctx, pathArgs...); bareErr == nil && bareRoot != "" {
			return repoRootResolution{identityRoot: bareRoot, workspaceRoot: bareRoot}, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) &&
			(strings.Contains(string(exitErr.Stderr), "not a git repository (or any of the parent directories)") ||
				strings.Contains(string(exitErr.Stderr), "not a git repository (or any parent up to mount point")) &&
			!gitMetadataMayExist(pathArgs...) {
			return repoRootResolution{}, fmt.Errorf("%w: %s", ErrNotGitRepository, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return repoRootResolution{}, fmt.Errorf("failed to get git repo root: %w", err)
	}
	toplevel := trimGitOutputLine(topOut)

	// Get git-dir and git-common-dir to detect if we're in a linked worktree.
	// In the main repo: git-dir == ".git", git-common-dir == ".git"
	// In a worktree:    git-dir == "<main>/.git/worktrees/<name>",
	//                   git-common-dir == "<main>/.git"
	infoCmd := exec.CommandContext(ctx, "git", "-C", toplevel, "rev-parse", "--git-dir", "--git-common-dir")
	infoCmd.WaitDelay = repoGitWaitDelay
	infoOut, err := infoCmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return repoRootResolution{}, ctx.Err()
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
	wtCmd.WaitDelay = repoGitWaitDelay
	wtOut, err := wtCmd.Output()
	if err == nil {
		worktree := trimGitOutputLine(wtOut)
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(commonDir, worktree)
		}
		return repoRootResolution{identityRoot: filepath.Clean(worktree), workspaceRoot: toplevel}, nil
	}
	if ctx.Err() != nil {
		return repoRootResolution{}, ctx.Err()
	}
	// Fallback: parent of .git directory (correct for non-submodule repos)
	return repoRootResolution{identityRoot: filepath.Dir(commonDir), workspaceRoot: toplevel}, nil
}

func resolveDirectBareRepoRootContext(ctx context.Context, pathArgs ...string) (string, error) {
	bareCmd := exec.CommandContext(ctx, "git", append(pathArgs, "rev-parse", "--is-bare-repository")...)
	bareCmd.WaitDelay = repoGitWaitDelay
	bareOut, err := bareCmd.Output()
	if err != nil || strings.TrimSpace(string(bareOut)) != "true" {
		return "", err
	}
	dirCmd := exec.CommandContext(ctx, "git", append(pathArgs, "rev-parse", "--absolute-git-dir")...)
	dirCmd.WaitDelay = repoGitWaitDelay
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
	cmd.WaitDelay = repoGitWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to inspect git common directory %s: %w", gitDir, err)
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

// RepoIDForRecordedRoot is the identity fallback for a RECORDED repo root
// whose path does not currently resolve through git: the ID a checkout at that
// recorded spelling gets. Every consumer that matches or attributes state
// against a possibly-absent recorded root must derive the fallback here —
// attribution (the daemon's root-agent snapshot) and matching
// (rootAgentKeyMatchesRepo, delete-project normalization) only agree while
// their fallbacks stay bit-identical, so strengthening the canonicalization in
// one copy would silently split identity between them.
func RepoIDForRecordedRoot(recorded string) string {
	return RepoIDFromRoot(filepath.Clean(recorded))
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
