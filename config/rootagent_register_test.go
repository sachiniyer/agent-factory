package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestDeregisterRootAgentsForRepoRemovesMatchAndPreservesOthers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Two opt-ins for gone repos (RepoFromPath won't resolve, so matching falls
	// back to hashing the cleaned path) plus an unrelated config key to prove the
	// write preserves it.
	seed := DefaultConfig()
	seed.DefaultProgram = "codex"
	seed.RootAgents = map[string]RootAgentConfig{"/repos/gone": {}, "/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/gone"))
	require.NoError(t, err)
	assert.Equal(t, []string{"/repos/gone"}, removed)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, cfg.RootAgents, "/repos/gone", "the matched opt-in must be removed")
	assert.Contains(t, cfg.RootAgents, "/repos/keep", "an unrelated opt-in must survive")
	assert.Equal(t, "codex", cfg.DefaultProgram, "an unrelated config key must survive the write")
}

func TestDeregisterRootAgentsForRepoUnknownIsNoOp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.RootAgents = map[string]RootAgentConfig{"/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/never-registered"))
	require.NoError(t, err)
	assert.Nil(t, removed, "deregistering an unknown repo removes nothing")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, cfg.RootAgents, "/repos/keep")
}

// TestDeregisterRootAgentsForRepoSweepsAllIdentitiesInOneWrite pins the #3299
// round-7 review: a re-attributed project carries two identities (real and
// derived recorded-path), and the delete's nothing-was-changed guarantee only
// holds if both are swept in ONE read-modify-write — two writes could remove
// one opt-in and then fail.
func TestDeregisterRootAgentsForRepoSweepsAllIdentitiesInOneWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.RootAgents = map[string]RootAgentConfig{"/repos/real": {}, "/repos/derived": {}, "/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/real"), RepoIDFromRoot("/repos/derived"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"/repos/real", "/repos/derived"}, removed)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, cfg.RootAgents, "/repos/real")
	assert.NotContains(t, cfg.RootAgents, "/repos/derived")
	assert.Contains(t, cfg.RootAgents, "/repos/keep")
}

// TestDeregisterRootAgentsMatchesASymlinkSpelledKey pins #3530 review id
// 3918120733.
//
// A root_agents key is written by a human, through whatever symlink they had,
// while the id a delete supplies is derived from the path the registry
// RESOLVED. Comparing the two lexically makes them unequal wherever an ancestor
// is a symlink — macOS `/var` -> `/private/var` every time — so the durable
// opt-in survives a delete that reported success and recreates the root when
// the checkout returns.
func TestDeregisterRootAgentsMatchesASymlinkSpelledKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	base := testguard.CanonicalTempDir(t)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	recorded := filepath.Join(real, "checkout")
	spelled := filepath.Join(link, "checkout")
	// The checkout is GONE — which is the only state in which the matcher's
	// hash fallback runs at all.
	if RepoIDFromRoot(spelled) == RepoIDFromRoot(recorded) {
		t.Fatalf("fixture must produce two spellings that hash differently")
	}

	cfg := DefaultConfig()
	cfg.RootAgents = map[string]RootAgentConfig{spelled: {}}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot(recorded))
	if err != nil {
		t.Fatalf("DeregisterRootAgentsForRepo: %v", err)
	}
	if len(removed) != 1 || removed[0] != spelled {
		t.Fatalf("a key spelled through a symlink must still be swept for the identity its resolved path derives: removed %v", removed)
	}
}

// TestRecordedRootOptInWithheldWhenGitNeverAnswers pins #3530 review id
// 3918379034. A killed or unstartable git establishes nothing about what
// occupies the recorded path, and a repository that IS there owns the key — so
// reporting the stale project's opt-in would promise a root the ensure sweep
// creates only for the occupant. Uncertainty withholds the fallback.
func TestRecordedRootOptInWithheldWhenGitNeverAnswers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base := testguard.CanonicalTempDir(t)
	recorded := filepath.Join(base, "recorded")
	if err := os.MkdirAll(recorded, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := DefaultConfig()
	cfg.RootAgents = map[string]RootAgentConfig{recorded: {}}

	// With git answering, the entry is this project's: the fixture proves the
	// lookup fires at all before the unanswerable probe is installed.
	if entry, key := LegacyRootAgentForRecordedRoot(cfg, recorded); entry == nil || key != recorded {
		t.Fatalf("fixture precondition: an answered negative must yield the entry, got key %q", key)
	}

	// A git shim that kills itself: the process dies on a signal, so nothing
	// proves it ever answered — the #3500 shape, deterministically.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	binDir := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\nkill -9 $$\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if entry, key := LegacyRootAgentForRecordedRoot(cfg, recorded); entry != nil {
		t.Fatalf("an unanswered probe is not evidence that the path is free, but the opt-in was returned under key %q", key)
	}
}

// TestRecordedRootOptInWithheldOnAnOperationalFailure pins #3530 review id
// 3919346216. Git exiting is not git ANSWERING that the path is free: dubious
// ownership, an unreadable .git and permission errors all complete, and a live
// repository at that path owns the key through any of them.
func TestRecordedRootOptInWithheldOnAnOperationalFailure(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base := testguard.CanonicalTempDir(t)
	recorded := filepath.Join(base, "recorded")
	if err := os.MkdirAll(recorded, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := DefaultConfig()
	cfg.RootAgents = map[string]RootAgentConfig{recorded: {}}
	if entry, _ := LegacyRootAgentForRecordedRoot(cfg, recorded); entry == nil {
		t.Fatalf("fixture precondition: an answered negative must yield the entry")
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	binDir := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if entry, key := LegacyRootAgentForRecordedRoot(cfg, recorded); entry != nil {
		t.Fatalf("an operational git failure is not evidence that the path is free, but the opt-in was returned under key %q", key)
	}
}

// TestRebindRefusesARepointedPathAtCommitTime pins #3530 review id 3919346210:
// RebindProject publishes binding data too, so it needs the same commit-time
// identity re-check registration got.
func TestRebindRefusesARepointedPathAtCommitTime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base := testguard.CanonicalTempDir(t)
	source := filepath.Join(base, "source")
	initRepoWithCommit(t, source)
	origin := filepath.Join(base, "origin")
	initRepoWithCommit(t, origin)
	repoA := filepath.Join(base, "A.git")
	repoB := filepath.Join(base, "B.git")
	for _, bare := range []string{repoA, repoB} {
		if err := exec.Command("git", "clone", "--quiet", "--bare", source, bare).Run(); err != nil {
			t.Fatalf("git clone --bare %s: %v", bare, err)
		}
	}
	project, err := RegisterProject(origin)
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	live := filepath.Join(base, "live")
	if err := exec.Command("git", "-C", repoA, "worktree", "add", "--detach", live).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	flipped := false
	projectRegistryCommitRaceHookForTest = func() {
		if flipped {
			return
		}
		flipped = true
		if err := exec.Command("git", "-C", repoA, "worktree", "remove", "--force", live).Run(); err != nil {
			t.Fatalf("detach the worktree from A: %v", err)
		}
		if err := exec.Command("git", "-C", repoB, "worktree", "add", "--detach", live).Run(); err != nil {
			t.Fatalf("attach the path to B: %v", err)
		}
	}
	t.Cleanup(func() { projectRegistryCommitRaceHookForTest = nil })

	rebound, err := RebindProject(project.ID, live)
	if !flipped {
		t.Fatalf("fixture never ran the flip; the seam is not on the path to the rebind's write")
	}
	if err != nil {
		if !strings.Contains(err.Error(), "changed repositories") {
			t.Fatalf("refusing is correct, but the message must say what happened: %v", err)
		}
		return
	}
	rootRepo, rootErr := RepoFromPath(rebound.Root)
	if rootErr != nil {
		t.Fatalf("RepoFromPath(%s): %v", rebound.Root, rootErr)
	}
	if rebound.RepoID != rootRepo.ID {
		t.Fatalf("the rebind committed root %s (repository %s) with identity %s", rebound.Root, rootRepo.ID, rebound.RepoID)
	}
}
