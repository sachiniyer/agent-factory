package move

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	snap, err := TakeSnapshot(context.Background(), dir, "feat", "sess-1", t.TempDir())
	require.NoError(t, err)
	defer snap.Cleanup()
	require.True(t, snap.Dirty)
	require.NotEqual(t, snap.Head, snap.Commit)

	require.Equal(t, statusBefore, run(t, dir, "status", "--porcelain"))
	require.Equal(t, refsBefore, run(t, dir, "for-each-ref"), "no ref may leak into the source repo")
	require.Equal(t, "refs/af-move/sess-1/snapshot", snap.Ref)

	// Seed a fresh repo from the bundle and overlay: dirty work arrives
	// uncommitted, ignored files do not travel.
	dst := t.TempDir()
	run(t, dst, "init", "-q", "-b", "main")
	run(t, dst, "fetch", "-q", snap.BundlePath, "refs/heads/feat:refs/heads/feat", snap.Ref+":"+snap.Ref)
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
	snap, err := TakeSnapshot(context.Background(), dir, "feat", "sess-1", t.TempDir())
	require.NoError(t, err)
	defer snap.Cleanup()
	require.False(t, snap.Dirty)
	require.Equal(t, snap.Head, snap.Commit)
	require.Equal(t, "true", snap.OverlayCommand())
}

func TestSnapshotRefIsPerSessionAndNeverClobbers(t *testing.T) {
	dir := repoWithWork(t)
	run(t, dir, "update-ref", SnapshotRef("busy"), "HEAD")
	_, err := TakeSnapshot(context.Background(), dir, "feat", "busy", t.TempDir())
	require.ErrorContains(t, err, "already exists")
	require.Equal(t, run(t, dir, "rev-parse", "HEAD"), run(t, dir, "rev-parse", SnapshotRef("busy")), "foreign ref untouched")
	other, err := TakeSnapshot(context.Background(), dir, "feat", "other", t.TempDir())
	require.NoError(t, err)
	other.Cleanup()
	require.NotContains(t, run(t, dir, "for-each-ref", "refs/af-move/other"), "snapshot", "own ref deleted after the snapshot")
	_, err = TakeSnapshot(context.Background(), dir, "feat", "bad/key", t.TempDir())
	require.Error(t, err)
}

func TestRunScriptContract(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	spec := ScriptSpec{Title: "t", Branch: "b", Host: "h", SourceWorktree: dir, DestWorkspace: "/w"}
	dump := `cat > ` + out + `.json; printf '%s|%s|%s' "$AF_SSH" "$AF_SSH_DEST" "$AF_SSH_FULL" > ` + out + `.env`

	// Split shape: AF_SSH excludes the destination.
	require.NoError(t, RunScript(context.Background(), dump, spec, Host{SSH: "ssh -p 2", SSHDest: "u@box"}))
	e, _ := os.ReadFile(out + ".env")
	require.Equal(t, "ssh -p 2|u@box|ssh -p 2 u@box", string(e))
	j, _ := os.ReadFile(out + ".json")
	require.Contains(t, string(j), `"dest_workspace":"/w"`)
	require.Contains(t, string(j), `"ssh":"ssh -p 2"`)
	require.Contains(t, string(j), `"ssh_dest":"u@box"`)
	require.Contains(t, string(j), `"ssh_full":"ssh -p 2 u@box"`)

	// Free-form shape: only the full command exists.
	require.NoError(t, RunScript(context.Background(), dump, spec, Host{SSHFullCmd: "ssh -J jump box"}))
	e, _ = os.ReadFile(out + ".env")
	require.Equal(t, "||ssh -J jump box", string(e))

	err := RunScript(context.Background(), `echo boom >&2; exit 3`, spec, Host{})
	require.ErrorContains(t, err, "boom")
	require.NoError(t, RunScript(context.Background(), "  ", spec, Host{}))
}

type fakeSteps struct {
	calls   []string
	failAt  string
	reaped  int
	reapErr error
	// destTree, when set, is what the destination reports it holds.
	destTree string
	// verify, when set, computes the destination's tree from the snapshot.
	verify func(*Snapshot) (string, error)
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
func (f *fakeSteps) VerifyDestination(_ context.Context, _ Session, _ Host, snap *Snapshot) (string, error) {
	if err := f.step("verify"); err != nil {
		return "", err
	}
	if f.verify != nil {
		return f.verify(snap)
	}
	if f.destTree != "" {
		return f.destTree, nil
	}
	return snap.Tree, nil
}
func (f *fakeSteps) SyncAccount(context.Context, Session, Host) error { return f.step("account") }
func (f *fakeSteps) StartAgent(context.Context, Session, Host) error  { return f.step("start") }
func (f *fakeSteps) SwitchRow(context.Context, Session, Host) error   { return f.step("switch") }
func (f *fakeSteps) ArchiveLocal(context.Context, Session) error      { return f.step("archive") }

func TestRunOrderingAndAbortSemantics(t *testing.T) {
	dir := repoWithWork(t)
	s := Session{ID: "sess-1", Title: "t", Branch: "feat", Worktree: dir}
	h := Host{Name: "h", SSH: "ssh", SSHDest: "h", MoveCmd: "true"}
	opt := Options{TempDir: t.TempDir()}

	f := &fakeSteps{}
	require.NoError(t, Run(context.Background(), f, s, h, opt))
	require.Equal(t, []string{"quiesce", "provision", "verify", "account", "start", "switch", "archive"}, f.calls)
	require.Zero(t, f.reaped)

	for _, failAt := range []string{"quiesce", "provision", "verify", "account", "start", "switch"} {
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
	require.Equal(t, []string{"AF_SSH", "AF_SSH_DEST", "AF_SSH_FULL"}, p.ScriptEnvNames)
}

func TestScriptKillsDescendantsAndBoundsOutput(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "late")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := RunScript(ctx, `(sleep 1; touch `+marker+`) & wait`, ScriptSpec{SourceWorktree: dir}, Host{})
	require.Error(t, err)
	time.Sleep(1500 * time.Millisecond)
	require.NoFileExists(t, marker, "descendant outlived the aborted script")

	err = RunScript(context.Background(), `yes x | head -c 5000000; exit 1`, ScriptSpec{SourceWorktree: dir}, Host{})
	require.Error(t, err)
	require.Less(t, len(err.Error()), 3000)
}

func TestSnapshotRefusesDirtySubmodule(t *testing.T) {
	sub := t.TempDir()
	run(t, sub, "init", "-q", "-b", "main")
	run(t, sub, "commit", "-q", "--allow-empty", "-m", "s")
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "feat")
	run(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sm")
	run(t, dir, "commit", "-q", "-m", "add sub")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sm", "x"), []byte("x"), 0o644))
	_, err := TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
	require.ErrorContains(t, err, "submodule")
}
