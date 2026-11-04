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
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// The registry PROBE layer (#1145 split): turning a caller's path into a
// projectBinding — git probes, symlink normalization, the identity re-check —
// plus the checkout-marker reads and mints that binding identity rides on.
// The mutations that consume these (RegisterProject, RebindProjectIfRoot)
// stay in project_registry.go, as do the record writes and path predicates
// the family shares.

// resolveProjectBinding resolves a path to its project binding with no deadline
// of its own — the shape the admission paths (register, rebind, selector
// resolution) want, exactly as RepoFromPath is for repository identity.
func resolveProjectBinding(path string) (projectBinding, error) {
	return resolveProjectBindingContext(context.Background(), path)
}

// resolveProjectBindingContext is resolveProjectBinding for callers that
// promised a budget (#3599). Resolving a binding forks git several times — two
// probes of its own, plus every probe the identity-root resolution runs — and
// the daemon's re-attribution probe runs it against a path that may be wedged
// on a stale mount while a repository it has already gated waits on the answer.
// Those callers need the probes to end when their deadline does.
//
// Each probe's drain allowance comes from that same deadline (#3594) rather
// than from a second constant here: a caller that promised 400ms may be late by
// another 400ms, never dragged toward the 2s default.
func resolveProjectBindingContext(ctx context.Context, path string) (projectBinding, error) {
	abs, err := ResolveUserPath(path)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve project path %q: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve project path %q: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return projectBinding{}, fmt.Errorf("inspect project path %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return projectBinding{}, fmt.Errorf("project path %q is not a directory", resolved)
	}

	commonCmd := exec.CommandContext(ctx, "git", "-C", resolved, "rev-parse", "--show-toplevel", "--git-common-dir")
	commonCmd.WaitDelay = repoProbeWaitDelay(ctx)
	commonOut, err := commonCmd.Output()
	if err != nil {
		// Classified where the child ran (#3504): this resolver runs its own
		// probes, so without this the sentinel never reaches
		// ResolveProjectSelector and a killed git is still reported to the user
		// as "not inside a git repository". The caller's ctx, not a fresh
		// Background: a probe its deadline killed says so in the chain instead
		// of surfacing as an opaque "signal: killed" (#3517, #3599).
		return projectBinding{}, fmt.Errorf("resolve git common directory: %w", markUnansweredProbe(ctx, err))
	}
	commonParts := strings.SplitN(trimGitOutputLine(commonOut), "\n", 2)
	if len(commonParts) != 2 {
		return projectBinding{}, fmt.Errorf("resolve git common directory: unexpected git output %q", strings.TrimSpace(string(commonOut)))
	}
	worktreeRoot, err := filepath.EvalSymlinks(commonParts[0])
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve git worktree root: %w", err)
	}
	commonDir := commonParts[1]
	if !filepath.IsAbs(commonDir) {
		// git reports --git-common-dir relative to the -C working directory,
		// not relative to --show-toplevel (from a nested path it may be
		// "../../.git").
		commonDir = filepath.Join(resolved, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve git common directory: %w", err)
	}
	bareCmd := exec.CommandContext(ctx, "git", "--git-dir", commonDir, "rev-parse", "--is-bare-repository")
	bareCmd.WaitDelay = repoProbeWaitDelay(ctx)
	bareOut, err := bareCmd.Output()
	if err != nil {
		return projectBinding{}, fmt.Errorf("inspect git common directory: %w", markUnansweredProbe(ctx, err))
	}
	bare := strings.TrimSpace(string(bareOut))
	if bare != "true" && bare != "false" {
		return projectBinding{}, fmt.Errorf("inspect git common directory: unexpected git output %q", bare)
	}
	markerName, err := checkoutMarkerName()
	if err != nil {
		return projectBinding{}, err
	}
	checkoutRoot := worktreeRoot
	// The identity root the probes above have ESTABLISHED, which is a
	// different question from the workspace root next to it: a bare
	// repository's identity is the bare common directory shared by all of its
	// worktrees, while the checkout being registered is the linked worktree.
	// repoContextFromResolution draws the same line; this names it so the
	// resolution below can be checked against it.
	identityRoot := commonDir
	if bare == "false" {
		checkoutRoot, err = resolveMainRepoRootContext(ctx, "-C", resolved)
		if err != nil {
			return projectBinding{}, fmt.Errorf("project path %q is not inside a git checkout: %w", resolved, err)
		}
		checkoutRoot, err = filepath.EvalSymlinks(checkoutRoot)
		if err != nil {
			return projectBinding{}, fmt.Errorf("resolve git checkout root: %w", err)
		}
		identityRoot = checkoutRoot
	}
	if projectBindingIdentityRaceHookForTest != nil {
		projectBindingIdentityRaceHookForTest()
	}
	// A binding that cannot state its identity must not be committed: the
	// record would be written legacy-shaped, and if the path disappeared
	// before anything re-resolved it, the project would be addressed by a
	// provisional id and miss state already stored under the real one (#3530
	// review id 3914971775). Every other probe here has already succeeded, so
	// a failure at this one is transient — surface it and let the caller retry.
	identityRepo, identityErr := RepoFromPath(resolved)
	if identityErr != nil {
		return projectBinding{}, fmt.Errorf("resolve repository identity for %q: %w", resolved, identityErr)
	}
	// One binding, one repository. The resolution above is a SECOND probe of
	// the same path, and a path that changed repositories in between answers
	// both of them successfully — so registration would record repository A's
	// root, checkout root and marker with repository B's id, permanently, and
	// a later delete or policy lookup would target B's state (#3530 review id
	// 3915722459).
	//
	// The id itself still comes from RepoFromPath rather than from
	// RepoIDFromRoot(identityRoot) on purpose: that is the value every runtime
	// consumer computes for this path, and re-deriving it here from a
	// differently-normalised spelling would write down an identity nothing
	// else agrees with — a silent split, which is worse than the collision.
	// What changes is that it must now AGREE with what the earlier probes
	// established, and a disagreement refuses the binding instead of
	// committing a record that describes two repositories at once. Comparison
	// is by identity of the directory, not by spelling, so the normalisation
	// difference above is not itself a disagreement.
	if !sameProjectPath(identityRepo.IdentityPath(), identityRoot) {
		return projectBinding{}, fmt.Errorf("project path %q changed repositories while af was resolving it: its checkout root and marker describe the repository rooted at %s, but its identity now resolves to %s — nothing was registered; retry once the path is stable", resolved, identityRoot, identityRepo.IdentityPath())
	}
	identity := identityRepo.ID
	return projectBinding{
		repoID:             identity,
		root:               filepath.Clean(checkoutRoot),
		checkoutRoot:       filepath.Clean(checkoutRoot),
		relativeRoot:       ".",
		gitCommonDir:       filepath.Clean(commonDir),
		checkoutMarkerPath: filepath.Join(commonDir, checkoutMarkerDirName, markerName),
	}, nil
}

// checkoutMarkerName scopes a checkout identity to one canonical AF home. The
// marker still moves with the Git common directory, while a factory reset of a
// different home has a different file to remove. Hashing keeps an absolute
// home path out of the repository's machine-local metadata.
func checkoutMarkerName() (string, error) {
	home, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve AF home for checkout marker: %w", err)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve absolute AF home for checkout marker: %w", err)
	}
	digest := sha256.Sum256([]byte(pathutil.ResolveForCompare(absHome)))
	return checkoutMarkerFilePrefix + hex.EncodeToString(digest[:opaqueIDBytes]), nil
}

func ensureCheckoutID(markerPath string) (string, error) {
	checkoutID := ""
	err := WithFileLock(markerPath, func() error {
		data, err := os.ReadFile(markerPath)
		if err == nil {
			checkoutID = strings.TrimSpace(string(data))
			if !checkoutIDPattern.MatchString(checkoutID) {
				return fmt.Errorf("checkout marker %s contains invalid id %q", markerPath, checkoutID)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read checkout marker %s: %w", markerPath, err)
		}
		checkoutID, err = newOpaqueID(checkoutIDPrefix)
		if err != nil {
			return err
		}
		if !checkoutIDPattern.MatchString(checkoutID) {
			return fmt.Errorf("invalid checkout id %q", checkoutID)
		}
		if err := AtomicWriteFile(markerPath, []byte(checkoutID+"\n"), 0o644); err != nil {
			return fmt.Errorf("write checkout marker %s: %w", markerPath, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return checkoutID, nil
}

func readCheckoutID(markerPath string) (id string, exists bool, err error) {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read checkout marker %s: %w", markerPath, err)
	}
	id = strings.TrimSpace(string(data))
	if !checkoutIDPattern.MatchString(id) {
		return "", false, fmt.Errorf("checkout marker %s contains invalid id %q", markerPath, id)
	}
	return id, true, nil
}

func projectRootHasCheckoutID(root, checkoutID string) (bool, error) {
	return projectRootHasCheckoutIDContext(context.Background(), root, checkoutID)
}

func projectRootHasCheckoutIDContext(ctx context.Context, root, checkoutID string) (bool, error) {
	info, statErr := os.Stat(root)
	// Same rule as storedProjectMarkerPath above: a root that resolves to nothing
	// carries no marker, and saying so is what lets a moved checkout re-register
	// against a dead old path (#2949 review). Ambiguous failures still error.
	if determinatelyAbsent(statErr) {
		return false, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("inspect last-known project root %s: %w", root, statErr)
	}
	if !info.IsDir() {
		return false, nil
	}
	binding, err := resolveProjectBindingContext(ctx, root)
	if err != nil {
		// A caller-owned directory may legitimately reuse a moved checkout's
		// old path. With no .git entry it cannot carry this checkout marker.
		//
		// This shortcut survives an UNANSWERED probe (#3599) because it does not
		// rest on one: the Lstat is independent evidence, and a registered
		// project's root always has a .git entry — resolveProjectBinding refuses
		// a bare repository, which is the one repository shape without one. Every
		// other failure, a killed probe included, propagates as an error, so a
		// caller cannot read "we could not ask git" as "the marker is not there".
		if _, statErr := os.Lstat(filepath.Join(root, ".git")); errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect existing Git metadata at last-known project root %s: %w", root, err)
	}
	id, exists, err := readCheckoutID(binding.checkoutMarkerPath)
	if err != nil {
		return false, err
	}
	return exists && id == checkoutID, nil
}

func projectRootUsesGitCommonDir(root, commonDir string) bool {
	binding, err := resolveProjectBinding(root)
	return err == nil && sameProjectPath(binding.gitCommonDir, commonDir)
}

// projectBindingIdentityRaceHookForTest, when non-nil, runs between the probes
// that establish a path's checkout root, common directory and marker and the
// resolution that names its repository identity. That window is #3530 review
// id 3915722459's entire subject and nothing else can hold it open: a real
// mount flip or worktree re-point lands inside microseconds, so a test that
// races it pins nothing.
var projectBindingIdentityRaceHookForTest func()
