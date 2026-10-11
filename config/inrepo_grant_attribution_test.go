package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInRepoProgramOverrideRefusesAbsoluteExecutables covers the
// absolute-path sibling of #5265 — and its two review follow-ups. A checked-in
// program_overrides value that selects an ABSOLUTE executable is not trusted
// provenance for a file outside the repository: the file may live in another
// checkout of the same repo (a linked-worktree session's own-tree check
// cannot see it), or be a payload post_worktree_commands delivers at setup.
// The grant cannot tell either apart from the operator's /usr/local/bin
// install, so the checked-in layer refuses the spelling outright — the same
// line the cloud-selector refusal draws. Absolute overrides keep working in
// the operator-owned layers: the global config and the machine-local
// personal project file.
func TestInRepoProgramOverrideRefusesAbsoluteExecutables(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for _, agent := range []string{"codex", "claude", "gemini", "aider", "amp", "opencode", "devin"} {
		t.Run(agent, func(t *testing.T) {
			repoRoot := t.TempDir()
			inside := filepath.Join(repoRoot, agent)
			path := writeInRepoConfig(t, repoRoot,
				fmt.Sprintf(`{"program_overrides": {%q: %q}}`, agent, inside))

			cfg, raw, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err,
				"a checked-in program_overrides selecting an absolute executable must be refused: "+
					"the launch grant accepts absolute paths as operator-attributable, so any file "+
					"the repository can aim the spelling at inherits the operator's credentials (#5265)")
			assert.Nil(t, cfg, "a refused in-repo config must not be returned as usable config")
			assert.Nil(t, raw)

			msg := err.Error()
			for _, want := range []string{path, "program_overrides", agent, "absolute executable"} {
				assert.Contains(t, msg, want, "the refusal must say %q", want)
			}
		})
	}

	// An absolute path OUTSIDE this checkout is refused on the same line: a
	// sibling worktree or a staged payload escapes the pane's own-tree
	// containment exactly the way a committed binary does.
	t.Run("absolute path outside repo", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot,
			`{"program_overrides": {"codex": "/opt/tools/codex --fast"}}`)

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute executable")
	})

	// The spellings af parses around the executable — an env wrapper and an
	// exec prefix — select the same absolute file, so they meet the same
	// refusal. A guard keyed on the raw string would miss both.
	t.Run("through env wrapper", func(t *testing.T) {
		repoRoot := t.TempDir()
		inside := filepath.Join(repoRoot, "codex")
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, "env TERM=xterm "+inside))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute executable")
	})

	t.Run("through exec prefix", func(t *testing.T) {
		repoRoot := t.TempDir()
		inside := filepath.Join(repoRoot, "codex")
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, "exec -- "+inside))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute executable")
	})

	// A spelling inside the repository is repository content even when the
	// file it selects resolves out: an in-tree symlink to an outside binary
	// can be repointed by a later commit before exec (#5265 review).
	t.Run("in-tree symlink resolving outside", func(t *testing.T) {
		repoRoot := t.TempDir()
		outside := filepath.Join(t.TempDir(), "codex")
		link := filepath.Join(repoRoot, "codex")
		require.NoError(t, os.Symlink(outside, link))
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, link))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute executable")
	})
}

// TestInRepoProgramOverrideStillLoadsUnattributableCommands pins what the
// refusal must NOT cost: a relative override still launches (it simply earns
// no grant under the shared predicate), and a bare agent name keeps its PATH
// resolution.
func TestInRepoProgramOverrideStillLoadsUnattributableCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := []struct {
		name     string
		override string
	}{
		{"relative dot executable", `{"program_overrides": {"codex": "./codex"}}`},
		{"relative subdir executable", `{"program_overrides": {"claude": "bin/claude --verbose"}}`},
		{"bare name", `{"program_overrides": {"gemini": "gemini --model flash"}}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeInRepoConfig(t, repoRoot, c.override)

			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err, "this value cannot aim the grant at an attributable file and must still load")
			require.NotNil(t, cfg)
		})
	}
}
