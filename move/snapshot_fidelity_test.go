package move

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A move snapshot captures EXACTLY the source worktree's state and is verified
// complete before the source is released (#5975 #5976 #5977 #5978 #5979).

func snapRefs(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "for-each-ref", "refs/af-move")
}

// #5976: a modified file hidden by assume-unchanged or skip-worktree is still work.
func TestSnapshotIncludesFilesFlaggedAssumeUnchangedOrSkipWorktree(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			dir := repoWithWork(t)
			run(t, dir, "update-index", flag, "a.txt")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed under a flag\n"), 0o644))

			snap, err := TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
			require.NoError(t, err)
			assert.True(t, snap.Dirty, "the modification is work")
			assert.Equal(t, "changed under a flag", run(t, dir, "show", snap.Commit+":a.txt"))
			// The source's own index is untouched: its flags are still set.
			assert.Contains(t, run(t, dir, "ls-files", "-v", "a.txt"), "a.txt")
			assert.NotEqual(t, "H", run(t, dir, "ls-files", "-v", "a.txt")[:1])
		})
	}
}

// A sparse checkout's absent skip-worktree files are not deleted by the snapshot.
func TestSnapshotKeepsAbsentSkipWorktreeFiles(t *testing.T) {
	dir := repoWithWork(t)
	run(t, dir, "update-index", "--skip-worktree", "a.txt")
	require.NoError(t, os.Remove(filepath.Join(dir, "a.txt")))

	snap, err := TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "one", run(t, dir, "show", snap.Commit+":a.txt"), "not staged as a deletion")
}

// #5979: an untracked nested repository would become a bare gitlink.
func TestSnapshotRefusesAnUntrackedEmbeddedRepo(t *testing.T) {
	dir := repoWithWork(t)
	nested := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	run(t, nested, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "n.txt"), []byte("n"), 0o644))
	run(t, nested, "add", "-A")
	run(t, nested, "commit", "-q", "-m", "nested")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "untracked.txt"), []byte("u"), 0o644))

	_, err := TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
	require.ErrorContains(t, err, "nested")
	require.ErrorContains(t, err, "cannot carry")
	assert.Empty(t, snapRefs(t, dir), "a refused snapshot leaves no ref")
}

// #5978: a shallow source never yields a "successful" unusable bundle.
func TestSnapshotRefusesAShallowRepo(t *testing.T) {
	origin := repoWithWork(t)
	require.NoError(t, os.WriteFile(filepath.Join(origin, "a.txt"), []byte("two\n"), 0o644))
	run(t, origin, "commit", "-q", "-am", "second")
	shallow := filepath.Join(t.TempDir(), "shallow")
	out, err := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+origin, shallow).CombinedOutput()
	require.NoError(t, err, string(out))
	run(t, shallow, "checkout", "-q", "-B", "feat")

	_, err = TakeSnapshot(context.Background(), shallow, "feat", "s", t.TempDir())
	require.ErrorContains(t, err, "shallow")
	require.ErrorContains(t, err, "git fetch --unshallow")
	require.Error(t, CheckShallow(context.Background(), shallow), "the dry run refuses too")
	_, err = Describe(context.Background(), Session{Title: "t", Branch: "feat", Worktree: shallow}, Host{Name: "h"}, Options{})
	require.ErrorContains(t, err, "shallow")
}

// #5975: cancelling mid-snapshot removes the ref, so the next move is not blocked.
func TestCancelledSnapshotRemovesItsRef(t *testing.T) {
	dir := repoWithWork(t)
	ctx, cancel := context.WithCancel(context.Background())
	snapshotAfterRef = cancel
	defer func() { snapshotAfterRef = func() {} }()

	_, err := TakeSnapshot(ctx, dir, "feat", "s", t.TempDir())
	require.Error(t, err)
	assert.Empty(t, snapRefs(t, dir), "the cancelled snapshot's ref is gone")

	snapshotAfterRef = func() {}
	_, err = TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
	require.NoError(t, err, "and a retry is not blocked by the existing-ref guard")
}

// The destination check is real: seed a clone from the bundle, overlay, and the
// verify command prints exactly the snapshot's tree.
func TestVerifyCommandMatchesTheOverlaidDestination(t *testing.T) {
	dir := repoWithWork(t)
	snap, err := TakeSnapshot(context.Background(), dir, "feat", "s", t.TempDir())
	require.NoError(t, err)
	require.True(t, snap.Dirty)

	dst := t.TempDir()
	run(t, dst, "init", "-q", "-b", "other")
	run(t, dst, "fetch", "-q", snap.BundlePath, "refs/heads/feat:refs/heads/feat", snap.Ref+":"+snap.Ref)
	run(t, dst, "checkout", "-q", "feat")
	sh := func(script string) string {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dst
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	sh(snap.OverlayCommand())
	require.NoError(t, snap.CheckTree(sh(snap.VerifyCommand())))

	// Any drift is caught.
	require.NoError(t, os.WriteFile(filepath.Join(dst, "a.txt"), []byte("drift\n"), 0o644))
	require.ErrorContains(t, snap.CheckTree(sh(snap.VerifyCommand())), "not the snapshot's")
}

func TestRunRefusesToProceedWhenTheDestinationDiffers(t *testing.T) {
	dir := repoWithWork(t)
	s := Session{ID: "sess-1", Title: "t", Branch: "feat", Worktree: dir}
	f := &fakeSteps{destTree: "deadbeef"}
	err := Run(context.Background(), f, s, Host{Name: "h"}, Options{TempDir: t.TempDir()})
	require.ErrorContains(t, err, "not the snapshot's")
	assert.NotContains(t, f.calls, "start", "the agent is not started on a wrong tree")
	assert.NotContains(t, f.calls, "archive", "and the source is not released")
	assert.Equal(t, 1, f.reaped)
	assert.Empty(t, snapRefs(t, dir))
}

// #5977: a script that backgrounds work and then exits (any status) leaves nothing
// running in its group.
func TestScriptGroupIsReapedOnExitWhateverTheStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		script  string
		wantErr bool
	}{
		"nonzero": {`(sleep 0.5; touch late) >/dev/null 2>&1 & exit 1`, true},
		"zero":    {`(sleep 0.5; touch late) >/dev/null 2>&1 & exit 0`, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			err := RunScript(context.Background(), tc.script, ScriptSpec{SourceWorktree: dir}, Host{})
			assert.Equal(t, tc.wantErr, err != nil)
			time.Sleep(1200 * time.Millisecond)
			assert.NoFileExists(t, filepath.Join(dir, "late"), "a descendant outlived the script and could modify the destination")
		})
	}
}

// #5980: the example passes quoted ssh options through intact.
func TestExampleMoveScriptKeepsQuotedSSHOptions(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("the example parses its JSON with python3")
	}
	script, err := filepath.Abs("../examples/move-script/move.sh")
	require.NoError(t, err)

	src, bin, log := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "args.log")
	require.NoError(t, os.WriteFile(filepath.Join(src, ".env"), []byte("SECRET=1\n"), 0o600))
	// A fake ssh that records one argument per line, then drains stdin.
	fake := filepath.Join(bin, "fakessh")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nfor a in \"$@\"; do printf 'ARG:%s\\n' \"$a\" >> "+log+"; done\ncat >/dev/null\n"), 0o755))

	const proxy = `ProxyCommand=nc -X connect -x proxy.invalid:8080 %h %p`
	full := fake + ` -o "` + proxy + `" box`
	spec := ScriptSpec{SourceWorktree: src, DestWorkspace: "/ws"}
	script = "sh " + script
	require.NoError(t, RunScript(context.Background(), script, spec, Host{SSHFullCmd: full}))

	got, err := os.ReadFile(log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	assert.Contains(t, lines, "ARG:"+proxy, "the quoted option reaches ssh as ONE argument")
	assert.NotContains(t, lines, "ARG:ProxyCommand=nc", "and is not split on its spaces")
}
