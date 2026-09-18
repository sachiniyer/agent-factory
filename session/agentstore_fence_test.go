package session

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestAgentStoreFenceMatchesWhereLaunchesWriteSkills holds testguard's
// agent-store tripwire to production (#4508 review). testguard cannot import
// session, so its list of guarded skill files is a hand-kept copy of the skill
// bases in agentskill.go and ampskill.go. If production moves a base and the
// copy does not follow, the tripwire guards a path nothing writes, and tests
// keep reporting an isolation they no longer have.
//
// So this compares effects rather than path helpers. Every supported agent is
// launched through the real create path with global_agent_skills granted, and
// every af skill file that lands under the roots this test controls is
// collected. Two checks follow:
//   - every file written is in testguard.FencedAgentStoreFiles (no unfenced
//     write);
//   - every file on that list is written by some agent (no stale fence).
//
// It runs twice: with CODEX_HOME and GEMINI_CLI_HOME unset, so every root comes
// from HOME, and with both set. In the second pass the list also keeps the
// HOME-derived entries, which cover a test that clears the variable, so only the
// entries the variables add must be written there.
func TestAgentStoreFenceMatchesWhereLaunchesWriteSkills(t *testing.T) {
	home := agentHome(t)
	homeOnly := fenceUnder(t, map[string]string{})
	fenceLaunchAll(t, home, nil, homeOnly, homeOnly)

	overrides := map[string]string{
		"CODEX_HOME":      t.TempDir(),
		"GEMINI_CLI_HOME": t.TempDir(),
	}
	withOverrides := fenceUnder(t, overrides)
	var added []string
	for _, file := range withOverrides {
		if !slices.Contains(homeOnly, file) {
			added = append(added, file)
		}
	}
	require.NotEmpty(t, added, "setting CODEX_HOME and GEMINI_CLI_HOME added nothing to the fence")
	fenceLaunchAll(t, home, overrides, withOverrides, added)
}

// fenceUnder sets exactly the given root variables, unsetting the rest, and
// returns the fence testguard resolves under them.
func fenceUnder(t *testing.T, vars map[string]string) []string {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "GEMINI_CLI_HOME"} {
		if value, ok := vars[name]; ok {
			t.Setenv(name, value)
			continue
		}
		t.Setenv(name, "placeholder")
		require.NoError(t, os.Unsetenv(name))
	}
	fence := testguard.FencedAgentStoreFiles()
	sort.Strings(fence)
	return fence
}

// fenceLaunchAll launches every supported agent under the current environment
// and checks both directions. mustWrite is the part of the fence this pass
// has to produce.
func fenceLaunchAll(t *testing.T, home string, overrides map[string]string, fence, mustWrite []string) {
	t.Helper()
	roots := []string{home}
	for _, root := range overrides {
		roots = append(roots, root)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.GlobalAgentSkills = true
	cfg.ProgramOverrides = make(map[string]string)
	bin := t.TempDir()
	for _, agent := range tmux.SupportedPrograms {
		stub := filepath.Join(bin, agent)
		require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755))
		cfg.ProgramOverrides[agent] = stub
	}
	require.NoError(t, config.SaveConfig(cfg))

	written := map[string][]string{}
	for _, agent := range tmux.SupportedPrograms {
		work := t.TempDir()
		inst, err := NewInstance(InstanceOptions{Title: "fence-" + agent, Path: work, Program: agent})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(work, work, inst.Title, "main", "", true, false)
		require.NoError(t, err)
		inst.SetGitWorktreeForTest(gw)
		_, err = (&LocalBackend{}).prepareCreateLaunch(inst)
		require.NoErrorf(t, err, "create launch for %s", agent)
		for _, file := range afSkillFilesUnder(t, roots) {
			if !slices.Contains(written[file], agent) {
				written[file] = append(written[file], agent)
			}
		}
	}

	var unfenced []string
	for file, agents := range written {
		if !slices.Contains(fence, file) {
			unfenced = append(unfenced, file+" (written for "+agents[0]+")")
		}
	}
	sort.Strings(unfenced)
	require.Empty(t, unfenced,
		"af wrote a skill file that testguard's agent-store tripwire does not guard: "+
			"update ambientAgentStoreFiles in internal/testguard/agentstore.go to match session's skill bases")

	var stale []string
	for _, file := range mustWrite {
		if _, ok := written[file]; !ok {
			stale = append(stale, file)
		}
	}
	require.Empty(t, stale,
		"testguard's agent-store tripwire guards a skill file no supported agent writes any more: "+
			"update ambientAgentStoreFiles in internal/testguard/agentstore.go to match session's skill bases")
}

// afSkillFilesUnder lists every agent-factory/SKILL.md beneath roots.
func afSkillFilesUnder(t *testing.T, roots []string) []string {
	t.Helper()
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() == "SKILL.md" && filepath.Base(filepath.Dir(path)) == afSkillDirName {
				files = append(files, path)
			}
			return nil
		})
		require.NoError(t, err)
	}
	return files
}
