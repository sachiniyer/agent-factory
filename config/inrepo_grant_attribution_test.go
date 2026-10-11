package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInRepoProgramOverrideCannotSelectExecutableInsideRepo covers the
// absolute-path sibling of #5265. A checked-in program_overrides value whose
// executable is an absolute path INSIDE this repository is repository content
// spelled in the operator-attributable form the credential grant accepts, so
// `<repo>/codex` would hand the repo's own committed binary the operator's
// model credentials exactly as `./codex` did before the launch guard. The
// checked-in layer refuses the value outright — the same line the
// cloud-selector refusal draws: the file may choose which program runs, not
// aim a credential grant at content it ships.
func TestInRepoProgramOverrideCannotSelectExecutableInsideRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for _, agent := range []string{"codex", "claude", "gemini", "aider", "amp", "opencode", "devin"} {
		t.Run(agent, func(t *testing.T) {
			repoRoot := t.TempDir()
			inside := filepath.Join(repoRoot, agent)
			path := writeInRepoConfig(t, repoRoot,
				fmt.Sprintf(`{"program_overrides": {%q: %q}}`, agent, inside))

			cfg, raw, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err,
				"a checked-in program_overrides whose executable lives inside the repository must be "+
					"refused: the launch grant accepts absolute paths, so the repo's own file would "+
					"inherit the operator's credentials (#5265)")
			assert.Nil(t, cfg, "a refused in-repo config must not be returned as usable config")
			assert.Nil(t, raw)

			msg := err.Error()
			for _, want := range []string{path, "program_overrides", agent, "inside this repository"} {
				assert.Contains(t, msg, want, "the refusal must say %q", want)
			}
		})
	}

	// The spellings af parses around the executable — an env wrapper and an
	// exec prefix — select the same in-tree file, so they meet the same
	// refusal. A guard keyed on the raw string would miss both.
	t.Run("through env wrapper", func(t *testing.T) {
		repoRoot := t.TempDir()
		inside := filepath.Join(repoRoot, "codex")
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, "env TERM=xterm "+inside))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inside this repository")
	})

	t.Run("through exec prefix", func(t *testing.T) {
		repoRoot := t.TempDir()
		inside := filepath.Join(repoRoot, "codex")
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, "exec -- "+inside))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inside this repository")
	})

	// A spelling inside the repository is repository content even when the
	// file it selects resolves out: an in-tree symlink to an outside binary
	// can be repointed by a later commit before exec, so the lexical spelling
	// is refused on the same line as a regular in-tree file (#5265 review).
	t.Run("in-tree symlink resolving outside", func(t *testing.T) {
		repoRoot := t.TempDir()
		outside := filepath.Join(t.TempDir(), "codex")
		link := filepath.Join(repoRoot, "codex")
		require.NoError(t, os.Symlink(outside, link))
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"codex": %q}}`, link))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inside this repository")
	})

	t.Run("nested deeper inside repo", func(t *testing.T) {
		repoRoot := t.TempDir()
		inside := filepath.Join(repoRoot, "tools", "bin", "claude")
		writeInRepoConfig(t, repoRoot,
			fmt.Sprintf(`{"program_overrides": {"claude": %q}}`, inside))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inside this repository")
	})
}

// TestInRepoProgramOverrideStillLoadsUnattributableCommands pins what the
// refusal must NOT cost: a relative override still launches (it simply earns
// no grant under the shared predicate), and an absolute path OUTSIDE the
// repository is the documented operator-override form the in-repo file may
// keep choosing.
func TestInRepoProgramOverrideStillLoadsUnattributableCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := []struct {
		name     string
		override string
	}{
		{"relative dot executable", `{"program_overrides": {"codex": "./codex"}}`},
		{"relative subdir executable", `{"program_overrides": {"claude": "bin/claude --verbose"}}`},
		{"absolute path outside repo", `{"program_overrides": {"codex": "/opt/tools/codex --fast"}}`},
		{"bare name", `{"program_overrides": {"gemini": "gemini --model flash"}}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeInRepoConfig(t, repoRoot, c.override)

			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err, "this value cannot aim the grant at repository content and must still load")
			require.NotNil(t, cfg)
		})
	}
}
