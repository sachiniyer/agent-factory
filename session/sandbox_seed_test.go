package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingShell struct {
	scripts []string
	stdin   []string
}

func (r *recordingShell) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	r.scripts = append(r.scripts, script)
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.stdin = append(r.stdin, body)
	return nil, nil
}

func TestSeedFromBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "x.bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("BUNDLE"), 0o600))
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/feat", "refs/af-move/k/snapshot"},
	}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Len(t, sh.scripts, 2)
	require.Equal(t, "BUNDLE", sh.stdin[0])
	require.Contains(t, sh.scripts[0], "/s/seed.bundle")
	fetch := sh.scripts[1]
	require.Contains(t, fetch, "git -C '/s/workspace' fetch '/s/seed.bundle'")
	require.Contains(t, fetch, "checkout -q --detach")
	require.Contains(t, fetch, "'refs/heads/feat:refs/heads/feat'")
	require.Contains(t, fetch, "'refs/af-move/k/snapshot:refs/af-move/k/snapshot'")
	require.True(t, strings.HasSuffix(fetch, "rm -f '/s/seed.bundle'"))
}

func TestSeedFromBundleNoopWithoutBundle(t *testing.T) {
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, spec: ProvisionSpec{}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Empty(t, sh.scripts)
	w.spec.SeedBundlePath = "/nonexistent"
	require.Error(t, w.seedFromBundle(time.Second))
	w.spec.SeedBundlePath = os.DevNull
	require.ErrorContains(t, w.seedFromBundle(time.Second), "no refs")
}

// localShell runs scripts for real, so the generated git command is exercised.
type localShell struct{}

func (localShell) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// #5685: the seed carries the branch the fresh clone has checked out.
func TestSeedFromBundleIntoCheckedOutBranch(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	gitIn(t, origin, "commit", "-q", "--allow-empty", "-m", "base")

	// The session worked directly on main: one extra commit, bundled.
	src := t.TempDir()
	gitIn(t, src, "clone", "-q", origin, ".")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "session work")
	bundle := filepath.Join(t.TempDir(), "s.bundle")
	gitIn(t, src, "bundle", "create", bundle, "refs/heads/main")
	want := gitIn(t, src, "rev-parse", "main")

	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir, spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/main"},
	}}
	require.NoError(t, w.seedFromBundle(time.Minute))
	ws := filepath.Join(sessionDir, "workspace")
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"))
	require.NoFileExists(t, w.SeedBundlePath())
}

// An empty origin leaves HEAD unborn; the seed must still land.
func TestSeedFromBundleIntoUnbornHead(t *testing.T) {
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "work")
	bundle := filepath.Join(t.TempDir(), "s.bundle")
	gitIn(t, src, "bundle", "create", bundle, "refs/heads/main")

	sessionDir := t.TempDir()
	ws := filepath.Join(sessionDir, "workspace")
	gitIn(t, sessionDir, "init", "-q", "-b", "main", ws)
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir, spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/main"},
	}}
	require.NoError(t, w.seedFromBundle(time.Minute))
	require.Equal(t, gitIn(t, src, "rev-parse", "main"), gitIn(t, ws, "rev-parse", "refs/heads/main"))
}
