package session

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestInjectSystemPrompt_OpencodeExplicitPassthroughConfigWins pins #5981: an
// OPENCODE_CONFIG the operator named in session_env_passthrough reaches the pane
// even for a spelling the credential grant refuses (`./opencode`), so af must
// not shadow it with its own command-prefix assignment. The exported form for an
// attributable command is the control (the allowlist delivers it), and an
// unlisted unattributable spelling still gets af's config because the pane
// would otherwise see neither.
func TestInjectSystemPrompt_OpencodeExplicitPassthroughConfigWins(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	t.Setenv("OPENCODE_CONFIG", "/home/me/mine.jsonc")
	listed := launchEnv{passthrough: []string{"OPENCODE_CONFIG"}}

	for _, resolved := range []string{"./opencode", "ionice -c 3 opencode", "opencode --model x"} {
		if got := injectSystemPromptWithEnv(resolved, skillTarget{}, fullGuidanceCaps, listed); got != resolved {
			t.Errorf("OPENCODE_CONFIG is in the pass-through list: %q must come back unchanged, af shadowed it to %q", resolved, got)
		}
		// The same exported value WITHOUT the list is the pre-existing split:
		// attributable commands honor it, unattributable ones get af's config.
		got := injectSystemPrompt(resolved, skillTarget{}, fullGuidanceCaps)
		attributable := resolved == "opencode --model x"
		if attributable == strings.HasPrefix(got, "OPENCODE_CONFIG=") {
			t.Errorf("without the pass-through list, %q: attributable=%v but got %q", resolved, attributable, got)
		}
	}

	// A pass-through list that does not name OPENCODE_CONFIG changes nothing.
	other := launchEnv{passthrough: []string{"SOMETHING_ELSE"}}
	if got := injectSystemPromptWithEnv("./opencode", skillTarget{}, fullGuidanceCaps, other); !strings.HasPrefix(got, "OPENCODE_CONFIG=") {
		t.Errorf("an unrelated pass-through name must not suppress injection; got %q", got)
	}
}

// TestInjectSystemPrompt_OpencodeExportedConfigUsesRealWorkDir pins the related
// #5981 finding: an absolute executable inside the session worktree is
// attributed under an empty workDir but refused by the pane's own grant, which
// filters the exported value out. With the real workDir the guard agrees with
// the pane and af injects its config instead of leaving the pane with neither.
func TestInjectSystemPrompt_OpencodeExportedConfigUsesRealWorkDir(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG", "/home/me/mine.jsonc")
	workDir := t.TempDir()
	resolved := filepath.Join(workDir, "bin", "opencode")

	got := injectSystemPromptWithEnv(resolved, skillTarget{}, fullGuidanceCaps, launchEnv{workDir: workDir})
	if !strings.HasPrefix(got, "OPENCODE_CONFIG=") {
		t.Errorf("in-worktree executable: the pane filters the export out, so af must inject; got %q", got)
	}
	// Listing the name restores the user's value at the pane, so it wins.
	got = injectSystemPromptWithEnv(resolved, skillTarget{}, fullGuidanceCaps, launchEnv{passthrough: []string{"OPENCODE_CONFIG"}, workDir: workDir})
	if got != resolved {
		t.Errorf("in-worktree executable with OPENCODE_CONFIG listed: expected unchanged, got %q", got)
	}
}
