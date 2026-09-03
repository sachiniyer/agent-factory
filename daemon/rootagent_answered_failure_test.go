package daemon

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The #3794 regression suite: the review note from #3786, and the last slice of
// the #3782 series.
//
// #3786 gave legacyRepoIDSet a rule — AN UNKNOWN MUST NEVER READ AS ABSENT —
// because a repo missing from the singleton sweep's dedup set gets its root
// started with a NIL legacy layer, i.e. without the root_agents entry the user
// wrote. It then implemented that rule with the wrong predicate. The drop
// branch tested "not unanswered", which is every ANSWERED failure:
//
//   - a checkout whose .git is present but unreadable;
//   - a safe.directory / dubious-ownership rejection;
//   - an invalid .git file.
//
// git ran and exited non-zero for each, so none is unanswered — and none says
// the path is not a repository either. config.ErrNotGitRepository exists for
// the one that does, and its own doc says the rest "must remain visible rather
// than being mistaken for 'outside git'". So the same #3315 double-visit was
// reachable through an operational failure rather than a timeout: a mount that
// came back read-only starts the root under the wrong layer stack.
//
// #3771 drew this exact line for the app poll — resolved / answered: not a
// repository / git ran and failed without a verdict / unanswered — so this is
// applying a settled taxonomy to a set membership and a log line, not inventing
// one.

// answeredFailureRepo returns a path git ANSWERS about with a completed
// non-zero exit that is NOT "not a repository": a directory whose .git is a
// gitfile pointing nowhere.
//
// Chosen for portability, deliberately. chmod 000 needs a non-root runner and
// has no Windows meaning, and a safe.directory rejection needs a foreign owner;
// this is the member of the class that runs everywhere, the way
// breakPersonalRootAgentToml is for #3241. The returned restore puts the real
// repository back.
func answeredFailureRepo(t *testing.T, repoPath string) (restore func()) {
	t.Helper()
	gitDir := filepath.Join(repoPath, ".git")
	aside := filepath.Join(repoPath, ".git-aside")
	if err := os.Rename(gitDir, aside); err != nil {
		t.Fatalf("set .git aside: %v", err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: /nonexistent-target\n"), 0o644); err != nil {
		t.Fatalf("write invalid gitfile: %v", err)
	}
	// Prove the fixture really produces the state under test, rather than
	// assuming it: git must ANSWER, and its answer must not be the
	// not-a-repository verdict.
	_, err := config.RepoFromPath(repoPath)
	if err == nil {
		t.Fatal("fixture: the broken checkout still resolves")
	}
	if config.RepoProbeUnanswered(err) {
		t.Fatalf("fixture: this failure must be ANSWERED, got an unanswered probe: %v", err)
	}
	if errors.Is(err, config.ErrNotGitRepository) {
		t.Fatalf("fixture: this failure must NOT be the not-a-repository verdict, got: %v", err)
	}
	return func() {
		if err := os.Remove(gitDir); err != nil {
			t.Fatalf("remove invalid gitfile: %v", err)
		}
		if err := os.Rename(aside, gitDir); err != nil {
			t.Fatalf("restore .git: %v", err)
		}
	}
}

// TestAnsweredFailureKeepsThePreviousRepoID is the classification, at the
// function that makes it, across all four outcomes #3771 names.
func TestAnsweredFailureKeepsThePreviousRepoID(t *testing.T) {
	const path = "/repos/alpha"
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{path: {}}
	carried := legacyRepoDedup{byPath: map[string]string{path: "aaaaaaaaaaaa"}}

	fail := func(err error) legacyRepoResolver {
		return func(string) (*config.RepoContext, error) { return nil, err }
	}

	cases := []struct {
		name       string
		err        error
		wantIDs    map[string]bool
		wantByPath map[string]string
	}{{
		// The only outcome that is a claim about the PATH.
		name:       "not a repository drops the entry",
		err:        fmt.Errorf("failed to get git repo root: %w: fatal: not a git repository", config.ErrNotGitRepository),
		wantIDs:    map[string]bool{},
		wantByPath: map[string]string{},
	}, {
		name:       "an unreadable or invalid .git keeps it",
		err:        errors.New("failed to get git repo root for /repos/alpha: exit status 128: fatal: not a git repository: /nonexistent-target"),
		wantIDs:    map[string]bool{"aaaaaaaaaaaa": true},
		wantByPath: map[string]string{path: "aaaaaaaaaaaa"},
	}, {
		name:       "a dubious-ownership rejection keeps it",
		err:        errors.New("failed to get git repo root for /repos/alpha: exit status 128: fatal: detected dubious ownership in repository"),
		wantIDs:    map[string]bool{"aaaaaaaaaaaa": true},
		wantByPath: map[string]string{path: "aaaaaaaaaaaa"},
	}, {
		name:       "an unanswered probe keeps it, as before",
		err:        fmt.Errorf("failed to get git repo root: %w", config.ErrRepoProbeUnanswered),
		wantIDs:    map[string]bool{"aaaaaaaaaaaa": true},
		wantByPath: map[string]string{path: "aaaaaaaaaaaa"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyRepoIDSet(cfg, fail(tc.err), carried)
			if !maps.Equal(got.ids, tc.wantIDs) {
				t.Fatalf("dedup set = %v, want %v", got.ids, tc.wantIDs)
			}
			if !maps.Equal(got.byPath, tc.wantByPath) {
				t.Fatalf("per-path resolutions = %v, want %v", got.byPath, tc.wantByPath)
			}
		})
	}
}

// TestAnsweredFailureDoesNotDoubleVisitTheRepo is the same claim end to end,
// through the real heal pass, both real sweeps and a real broken checkout: the
// blast radius an operational failure could reach.
func TestAnsweredFailureDoesNotDoubleVisitTheRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	const legacyProgram = "/opt/legacy-root --model opus"
	manager := degradedRootAgentManager(t, repoPath, config.RootAgentConfig{Program: legacyProgram})
	if !manager.rootAgentLayers.Load().legacy.ids[rid] {
		t.Fatal("fixture: the boot snapshot must already dedup the legacy path, or there is nothing to carry forward")
	}

	// The mount comes back read-only, so to speak: git answers, and its answer
	// is an operational failure rather than a verdict about the path.
	restore := answeredFailureRepo(t, repoPath)

	manager.ensureRootAgentsAndWait()

	layers := manager.rootAgentLayers.Load()
	if len(layers.personalUnreadable) != 0 {
		t.Fatalf("fixture: the personal config must have healed so the recompute actually ran, got %d still unreadable", len(layers.personalUnreadable))
	}
	if !layers.legacy.ids[rid] {
		t.Fatal("an ANSWERED operational failure dropped the repo from the dedup set: git ran and failed without a verdict, " +
			"which says nothing about whether the path is a repository, so this is #3315's double-visit reached through an " +
			"operational failure rather than a timeout (#3794)")
	}
	if len(*seen) != 0 {
		t.Fatalf("nothing may be created behind a failure that established nothing — the singleton sweep started the root without the legacy layer, got %d creates", len(*seen))
	}

	// And once git answers properly again, the legacy layer materializes it.
	restore()
	manager.mu.Lock()
	if st := manager.rootEnsureStates[repoPath]; st != nil {
		st.nextAttempt = time.Time{}
	}
	manager.mu.Unlock()
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the recovered pass must create exactly one root, got %d creates", len(*seen))
	}
	if got := (*seen)[0].Program; got != legacyProgram {
		t.Fatalf("the root must run the legacy entry's program, got %q", got)
	}
}

// TestRepoResolveClaimSeparatesAllThreeStates pins the wording, which carries
// the same overclaim in the log line every operator reads.
func TestRepoResolveClaimSeparatesAllThreeStates(t *testing.T) {
	const subject = "root_agents entry"
	const path = "/repos/alpha"

	unanswered := repoResolveClaim(subject, path, fmt.Errorf("boom: %w", config.ErrRepoProbeUnanswered))
	if !strings.Contains(unanswered, "git never answered the probe") {
		t.Fatalf("an unanswered probe must keep #3500's wording, got %q", unanswered)
	}

	verdict := repoResolveClaim(subject, path, fmt.Errorf("boom: %w", config.ErrNotGitRepository))
	if !strings.Contains(verdict, "does not resolve to a git repository") {
		t.Fatalf("an answered not-a-repository verdict must still say so, got %q", verdict)
	}

	failed := repoResolveClaim(subject, path, errors.New("exit status 128: fatal: detected dubious ownership in repository"))
	if strings.Contains(failed, "does not resolve to a git repository") {
		t.Fatalf("git running and failing without a verdict must not be reported as a claim about the path (#3771), got %q", failed)
	}
	if !strings.Contains(failed, "git ran and failed without a verdict") {
		t.Fatalf("the third state must be named in #3771's words, got %q", failed)
	}
}
