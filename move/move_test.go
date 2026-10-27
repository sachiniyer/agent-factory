package move

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func repoWithWork(t *testing.T) string {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "feat")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644))
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644))         // modified
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked\n"), 0o644)) // untracked
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x\n"), 0o644))
	return dir
}

func TestSnapshotCapturesDirtyTreeWithoutTouchingTheSource(t *testing.T) {
	dir := repoWithWork(t)
	statusBefore := run(t, dir, "status", "--porcelain")
	refsBefore := run(t, dir, "for-each-ref")

	snap, err := TakeSnapshot(context.Background(), dir, "feat", t.TempDir())
	require.NoError(t, err)
	defer snap.Cleanup()
	require.True(t, snap.Dirty)
	require.NotEqual(t, snap.Head, snap.Commit)

	require.Equal(t, statusBefore, run(t, dir, "status", "--porcelain"))
	require.Equal(t, refsBefore, run(t, dir, "for-each-ref"), "no ref may leak into the source repo")

	// Seed a fresh repo from the bundle and overlay: dirty work arrives
	// uncommitted, ignored files do not travel.
	dst := t.TempDir()
	run(t, dst, "init", "-q", "-b", "main")
	run(t, dst, "fetch", "-q", snap.BundlePath, "refs/heads/feat:refs/heads/feat", SnapshotRef+":"+SnapshotRef)
	run(t, dst, "checkout", "-q", "feat")
	require.Equal(t, snap.Head, run(t, dst, "rev-parse", "HEAD"))
	cmd := exec.Command("sh", "-c", snap.OverlayCommand())
	cmd.Dir = dst
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, snap.Head, run(t, dst, "rev-parse", "HEAD"))
	b, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	require.Equal(t, "two\n", string(b))
	b, _ = os.ReadFile(filepath.Join(dst, "new.txt"))
	require.Equal(t, "untracked\n", string(b))
	require.NoFileExists(t, filepath.Join(dst, "ignored.txt"))
	require.Contains(t, run(t, dst, "status", "--porcelain"), "?? new.txt")
	require.Contains(t, run(t, dst, "status", "--porcelain"), "M a.txt")
}

func TestSnapshotCleanTree(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "feat")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
	snap, err := TakeSnapshot(context.Background(), dir, "feat", t.TempDir())
	require.NoError(t, err)
	defer snap.Cleanup()
	require.False(t, snap.Dirty)
	require.Equal(t, snap.Head, snap.Commit)
	require.Equal(t, "true", snap.OverlayCommand())
}

func TestRunScriptContract(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	spec := ScriptSpec{Title: "t", Branch: "b", Host: "h", SourceWorktree: dir, DestWorkspace: "/w"}
	require.NoError(t, RunScript(context.Background(), `cat > `+out+`.json; printf %s "$AF_SSH" > `+out+`.ssh`, spec, "ssh -p 2 box"))
	j, _ := os.ReadFile(out + ".json")
	require.Contains(t, string(j), `"dest_workspace":"/w"`)
	s, _ := os.ReadFile(out + ".ssh")
	require.Equal(t, "ssh -p 2 box", string(s))
	err := RunScript(context.Background(), `echo boom >&2; exit 3`, spec, "ssh x")
	require.ErrorContains(t, err, "boom")
	require.NoError(t, RunScript(context.Background(), "  ", spec, "ssh x"))
}

type fakeSteps struct {
	calls   []string
	failAt  string
	reaped  int
	reapErr error
}

func (f *fakeSteps) step(name string) error {
	f.calls = append(f.calls, name)
	if f.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}
func (f *fakeSteps) Quiesce(context.Context, Session, bool) error { return f.step("quiesce") }
func (f *fakeSteps) Provision(context.Context, Session, Host, *Snapshot) (string, func() error, error) {
	return "/ws", func() error { f.reaped++; return f.reapErr }, f.step("provision")
}
func (f *fakeSteps) SyncAccount(context.Context, Session, Host) error { return f.step("account") }
func (f *fakeSteps) StartAgent(context.Context, Session, Host) error  { return f.step("start") }
func (f *fakeSteps) SwitchRow(context.Context, Session, Host) error   { return f.step("switch") }
func (f *fakeSteps) ArchiveLocal(context.Context, Session) error      { return f.step("archive") }

func TestRunOrderingAndAbortSemantics(t *testing.T) {
	dir := repoWithWork(t)
	s := Session{Title: "t", Branch: "feat", Worktree: dir}
	h := Host{Name: "h", SSHCmd: "ssh h", MoveCmd: "true"}
	opt := Options{TempDir: t.TempDir()}

	f := &fakeSteps{}
	require.NoError(t, Run(context.Background(), f, s, h, opt))
	require.Equal(t, []string{"quiesce", "provision", "account", "start", "switch", "archive"}, f.calls)
	require.Zero(t, f.reaped)

	for _, failAt := range []string{"quiesce", "provision", "account", "start", "switch"} {
		f := &fakeSteps{failAt: failAt}
		require.ErrorContains(t, Run(context.Background(), f, s, h, opt), failAt+" failed")
		require.NotContains(t, f.calls, "archive", failAt)
		if failAt != "quiesce" {
			require.Equal(t, 1, f.reaped, "sandbox must be reaped on %s failure", failAt)
		}
	}

	// A failing script aborts before the agent starts and reaps.
	f = &fakeSteps{}
	h.MoveCmd = "exit 1"
	require.Error(t, Run(context.Background(), f, s, h, opt))
	require.NotContains(t, f.calls, "start")
	require.Equal(t, 1, f.reaped)

	// Archive failure after the switch is reported but never reaps the sandbox.
	f = &fakeSteps{failAt: "archive"}
	h.MoveCmd = ""
	require.ErrorContains(t, Run(context.Background(), f, s, h, opt), "moved to h")
	require.Zero(t, f.reaped)
}

func TestDescribe(t *testing.T) {
	dir := repoWithWork(t)
	p, err := Describe(context.Background(), Session{Title: "t", Branch: "feat", Worktree: dir}, Host{Name: "h"}, Options{DefaultMoveCmd: "./m.sh"})
	require.NoError(t, err)
	require.True(t, p.Dirty)
	require.Equal(t, "./m.sh", p.MoveScript)
	require.Equal(t, []string{"AF_SSH"}, p.ScriptEnvNames)
}
