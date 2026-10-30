package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default session root is unchanged: no listed host (or one without a
// workspace_root) lands under $HOME/.af-sessions (#5742).
func TestSessionDirScriptDefaultRoot(t *testing.T) {
	got := sessionDirScript(ProvisionSpec{Title: "My Task"})
	assert.Equal(t, `mkdir -p "$HOME/.af-sessions" && mktemp -d "$HOME/.af-sessions/`+Slugify("My Task")+`.XXXXXX"`, got)
	assert.Equal(t, `"$HOME/.af-sessions"`, sessionRootDir(ProvisionSpec{}))
}

// A listed host's workspace_root decides where the per-session dir is created,
// and the script really runs there: executed against a real shell with $HOME
// pointed elsewhere, the dir lands under the root and the default is untouched.
func TestSessionDirScriptUsesWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "af work's root") // a space and a quote: quoting must hold
	spec := ProvisionSpec{Title: "My Task", WorkspaceRoot: root}

	cmd := exec.Command("sh", "-c", sessionDirScript(spec))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	dir := strings.TrimSpace(string(out))
	assert.Equal(t, root, filepath.Dir(dir), "the session dir is created directly under workspace_root")
	assert.True(t, strings.HasPrefix(filepath.Base(dir), Slugify("My Task")+"."))
	_, statErr := os.Stat(filepath.Join(home, sshSessionRoot))
	assert.True(t, os.IsNotExist(statErr), "the default ~/.af-sessions must not be created when a root is set")
	assert.Equal(t, shellQuote(root), sessionRootDir(spec), "the capacity probe checks the same root")
}
