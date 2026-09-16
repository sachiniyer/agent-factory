package session

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestPackageSandboxKeepsAgentRootsOffTheRealHome is #4469's red. It needs no
// setup of its own: the package TestMain's testguard.SandboxHome is the thing
// under test, and a test that never mentions CODEX_HOME is exactly the shape
// that used to snapshot and poll the developer's real ~/.codex.
//
// Every resolver here is a pure path computation. None of them is called for
// its side effect, so the red run on an unfixed tree reads nothing from the real
// store. ensureDevinSkillDir is deliberately absent: it has no separate resolver,
// and calling it under the sandbox's default config runs the declined-consent
// cleanup against whatever root it resolves. Before the fix, that is the
// developer's real ~/.config/devin/skills.
//
// The oracle is the passwd home rather than $HOME, because $HOME is the value
// the sandbox is supposed to have replaced.
func TestPackageSandboxKeepsAgentRootsOffTheRealHome(t *testing.T) {
	account, err := user.Current()
	require.NoError(t, err)
	realHome := filepath.Clean(account.HomeDir)
	if realHome == "" || realHome == "/" {
		t.Skipf("passwd home %q cannot tell a real agent root from a sandboxed one", account.HomeDir)
	}
	if pathWithin(os.TempDir(), realHome) {
		t.Skipf("the temp dir %s is inside the real home %s, so the sandbox itself lives there", os.TempDir(), realHome)
	}

	// Each resolver's fallback reads the ambient environment. The skill bases
	// are deliberately not called here: #4501 moves them onto the launch command,
	// and a zero skillTarget no longer resolves anything. The command resolver
	// covers that fallback instead. The fallbacks that read HOME alone (gemini,
	// amp, devin) are covered by os.UserHomeDir.
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	launchCodexStore, err := tmux.CodexHomeFromCommand("codex", t.TempDir())
	require.NoError(t, err)
	ampSkills, err := ampSkillsBaseDir()
	require.NoError(t, err)
	claudeStore, _, err := claudeTranscriptLaunchContext("claude", t.TempDir())
	require.NoError(t, err)

	for name, root := range map[string]string{
		"HOME (os.UserHomeDir)":                                home,
		"codex conversation capture (codexHomeDir)":            codexHomeDir(),
		"codex store for a bare launch (CodexHomeFromCommand)": launchCodexStore,
		"amp skills (ampSkillsBaseDir)":                        ampSkills,
		"claude transcripts (claudeTranscriptLaunchContext)":   claudeStore,
	} {
		require.NotEmpty(t, root, name)
		require.Falsef(t, pathWithin(root, realHome),
			"%s resolved to %s, inside the real home %s: the package sandbox did not relocate it (#4469)", name, root, realHome)
	}
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
