package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
)

// These tests run the REAL sweep scripts against a temp tree on this machine
// (no ssh): a local shell stands in for the host, and real processes stand in
// for agent-servers, so "a foreign same-name process survives" and "unpushed work
// is reported, not removed" are proved against actual /proc and git behavior.

type sweepFixture struct {
	t     *testing.T
	root  string
	key   []byte
	host  *sweepHostState
	owned map[string]bool
}

func localSweepShell(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
	cmd := exec.Command("/bin/sh", "-c", script)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if combined {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the sweep's process proof reads /proc")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	key, err := runtimetoken.EnsureKey()
	require.NoError(t, err)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	f := &sweepFixture{t: t, root: root, key: key, owned: map[string]bool{}}
	prov := &sandboxProvisioner{runCommandFn: localSweepShell}
	f.host = &sweepHostState{
		label: "test-host", shell: prov, roots: map[string]bool{root: true}, dirs: map[string]bool{}, refs: map[string]bool{},
		reaper: func(dir, pid, ref string) *sandboxProvisioner {
			return &sandboxProvisioner{runCommandFn: localSweepShell, sessionDir: dir, remotePID: pid, tokenRef: ref}
		},
	}
	return f
}

// session makes one session dir. validToken false writes a token this AF home did
// not derive (a foreign af home or a lookalike).
func (f *sweepFixture) session(name, ref string, validToken bool, age time.Duration) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	require.NoError(f.t, os.MkdirAll(dir, 0o700))
	token := "not-derived-by-this-af-home"
	if validToken {
		var err error
		token, err = runtimetoken.DeriveAgentServerToken(f.key, ref)
		require.NoError(f.t, err)
	}
	env := filepath.Join(dir, sshIdentityEnvName)
	require.NoError(f.t, os.WriteFile(env, []byte(runtimetoken.RefEnvName+"="+shellQuote(ref)+"\n"), 0o600))
	require.NoError(f.t, os.WriteFile(filepath.Join(dir, runtimetoken.TokenFileName), []byte(token+"\n"), 0o600))
	old := time.Now().Add(-age)
	require.NoError(f.t, os.Chtimes(env, old, old))
	return dir
}

func (f *sweepFixture) git(dir string, args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(f.t, err, "git %v: %s", args, out)
}

// workspace creates dir/workspace as a clone whose only commit is pushed (clean),
// or — unpushed — carries one more local commit.
func (f *sweepFixture) workspace(dir string, unpushed bool) {
	f.t.Helper()
	remote := filepath.Join(dir, "remote.git")
	f.git(f.root, "init", "-q", "--bare", remote)
	w := filepath.Join(dir, sshWorkspaceSubdir)
	require.NoError(f.t, os.MkdirAll(w, 0o755))
	f.git(w, "init", "-q")
	f.git(w, "commit", "-q", "--allow-empty", "-m", "base")
	f.git(w, "remote", "add", "origin", remote)
	f.git(w, "push", "-q", "origin", "HEAD")
	if unpushed {
		f.git(w, "commit", "-q", "--allow-empty", "-m", "work that exists nowhere else")
	}
}

// agentServer starts a real process whose argv[0] is bin and whose environment
// carries ref, standing in for an agent-server.
func (f *sweepFixture) agentServer(bin, ref string) *exec.Cmd {
	f.t.Helper()
	sleeper.Named(f.t, bin)
	cmd := exec.Command(bin, "300")
	cmd.Env = append(os.Environ(), runtimetoken.RefEnvName+"="+ref)
	require.NoError(f.t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	f.t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return cmd
}

func procAlive(cmd *exec.Cmd) bool { return cmd.Process.Signal(syscall.Signal(0)) == nil }

func (f *sweepFixture) sweep() []SweepFinding {
	f.t.Helper()
	return sweepHost(f.host, f.key, 15*time.Minute, func(dir, ref string) bool { return f.owned[dir] })
}

func findingFor(findings []SweepFinding, dir string) (SweepFinding, bool) {
	for _, fd := range findings {
		if fd.Dir == dir {
			return fd, true
		}
	}
	return SweepFinding{}, false
}

// A clean, provably-ours orphan is reaped: its agent-server is killed by identity
// and its dir removed — while a same-named process elsewhere, and a dir whose
// token this AF home did not derive, are left exactly as they were.
func TestSweepReapsAProvenOrphanAndSparesForeignSameNameProcesses(t *testing.T) {
	f := newSweepFixture(t)
	const ref = "v1.orphanref0001"
	orphan := f.session("orphan.AAA111", ref, true, time.Hour)
	f.workspace(orphan, false)
	orphanProc := f.agentServer(filepath.Join(orphan, sshAfBinaryName), ref)

	// A process with the SAME name and even the same ref, but not at this dir's
	// binary path: it is not this session's agent-server and must survive.
	bystander := f.agentServer(filepath.Join(f.root, "elsewhere", sshAfBinaryName), ref)

	// A dir this AF home did not make (token not derived by our key), with its own
	// same-named process: never a candidate.
	foreign := f.session("foreign.BBB222", "v1.foreignref0002", false, time.Hour)
	foreignProc := f.agentServer(filepath.Join(foreign, sshAfBinaryName), "v1.foreignref0002")

	require.True(t, procAlive(orphanProc) && procAlive(bystander) && procAlive(foreignProc), "anti-vacuous: every process starts alive")

	findings := f.sweep()

	fd, ok := findingFor(findings, orphan)
	require.True(t, ok, "the orphan must be a finding: %+v", findings)
	assert.Equal(t, SweepReaped, fd.Action, fd.Reason)
	_, err := os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "the orphan's dir must be removed")
	waitDead(t, orphanProc)

	assert.True(t, procAlive(bystander), "a same-named process outside the orphan's dir must survive the sweep")
	assert.True(t, procAlive(foreignProc), "a foreign dir's process must survive")
	_, err = os.Stat(foreign)
	assert.NoError(t, err, "a dir this AF home cannot prove it made must be left alone")
	_, found := findingFor(findings, foreign)
	assert.False(t, found, "a foreign dir is not even reported: it was never af's")
}

func waitDead(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !procAlive(cmd) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", cmd.Process.Pid)
}

// A workspace that may hold work existing nowhere else is REPORTED: the dir, its
// workspace and its running process are all left in place.
func TestSweepReportsUnpushedWorkAndRemovesNothing(t *testing.T) {
	cases := map[string]func(f *sweepFixture, dir string){
		"unpushed commit": func(f *sweepFixture, dir string) { f.workspace(dir, true) },
		"uncommitted file": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			require.NoError(f.t, os.WriteFile(filepath.Join(dir, sshWorkspaceSubdir, "notes.txt"), []byte("wip"), 0o644))
		},
		"stash": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			require.NoError(f.t, os.WriteFile(filepath.Join(w, "tracked.txt"), []byte("a"), 0o644))
			f.git(w, "add", "tracked.txt")
			f.git(w, "commit", "-q", "-m", "add")
			f.git(w, "push", "-q", "origin", "HEAD")
			require.NoError(f.t, os.WriteFile(filepath.Join(w, "tracked.txt"), []byte("b"), 0o644))
			f.git(w, "stash", "-q")
		},
		// The agent's commits can live in a LINKED worktree: the main workspace
		// is clean and fully pushed, yet the work exists nowhere else (Codex P1).
		"unpushed commit only in a linked worktree": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			f.git(w, "worktree", "add", "-q", "-b", "agent-work", filepath.Join(dir, "linked"))
			f.git(filepath.Join(dir, "linked"), "commit", "-q", "--allow-empty", "-m", "linked-only work")
		},
		// A detached HEAD is on no branch, so only a per-worktree HEAD check sees it.
		"unpushed commit on a detached HEAD in a linked worktree": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			f.git(w, "worktree", "add", "-q", "--detach", filepath.Join(dir, "linked"))
			f.git(filepath.Join(dir, "linked"), "commit", "-q", "--allow-empty", "-m", "detached work")
		},
		"uncommitted file only in a linked worktree": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			f.git(w, "worktree", "add", "-q", "-b", "agent-work", filepath.Join(dir, "linked"))
			require.NoError(f.t, os.WriteFile(filepath.Join(dir, "linked", "notes.txt"), []byte("wip"), 0o644))
		},
		"a branch ahead of its upstream": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			f.git(w, "branch", "--set-upstream-to=origin/main", "HEAD")
			f.git(w, "checkout", "-q", "-b", "side")
			f.git(w, "commit", "-q", "--allow-empty", "-m", "side work")
			f.git(w, "push", "-q", "origin", "side")
			f.git(w, "branch", "--set-upstream-to=origin/main", "side")
		},
		"a linked worktree whose directory is gone": func(f *sweepFixture, dir string) {
			f.workspace(dir, false)
			w := filepath.Join(dir, sshWorkspaceSubdir)
			f.git(w, "worktree", "add", "-q", "-b", "agent-work", filepath.Join(dir, "linked"))
			require.NoError(f.t, os.RemoveAll(filepath.Join(dir, "linked")))
		},
		"not a git workspace": func(f *sweepFixture, dir string) {
			require.NoError(f.t, os.MkdirAll(filepath.Join(dir, sshWorkspaceSubdir), 0o755))
			require.NoError(f.t, os.WriteFile(filepath.Join(dir, sshWorkspaceSubdir, "x"), []byte("x"), 0o644))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSweepFixture(t)
			const ref = "v1.unpushedref001"
			dir := f.session("work.CCC333", ref, true, time.Hour)
			setup(f, dir)
			proc := f.agentServer(filepath.Join(dir, sshAfBinaryName), ref)

			findings := f.sweep()

			fd, ok := findingFor(findings, dir)
			require.True(t, ok, "must be reported: %+v", findings)
			assert.Equal(t, SweepReported, fd.Action)
			assert.Contains(t, fd.Reason, "left in place")
			_, err := os.Stat(dir)
			assert.NoError(t, err, "the dir must not be removed")
			_, err = os.Stat(filepath.Join(dir, sshWorkspaceSubdir))
			assert.NoError(t, err, "the workspace must not be removed")
			assert.True(t, procAlive(proc), "the process must not be killed")
		})
	}
}

// Owned rows, young dirs and an ownership claimed while the sweep ran are never
// touched, and nothing is reported for them.
func TestSweepSkipsOwnedYoungAndLateOwnedDirs(t *testing.T) {
	f := newSweepFixture(t)

	owned := f.session("owned.DDD444", "v1.ownedref00001", true, time.Hour)
	f.workspace(owned, false)
	f.host.dirs[owned] = true

	ownedByRef := f.session("byref.EEE555", "v1.byrefref000001", true, time.Hour)
	f.workspace(ownedByRef, false)
	f.host.refs["v1.byrefref000001"] = true

	young := f.session("young.FFF666", "v1.youngref000001", true, time.Minute)
	f.workspace(young, false)

	late := f.session("late.GGG777", "v1.lateref0000001", true, time.Hour)
	f.workspace(late, false)
	f.owned[late] = true // a row persisted while the sweep was running

	findings := f.sweep()
	assert.Empty(t, findings, "nothing here was af's to reap or flag")
	for _, d := range []string{owned, ownedByRef, young, late} {
		_, err := os.Stat(d)
		assert.NoErrorf(t, err, "%s must survive", filepath.Base(d))
	}
}

// A process claiming a dir's binary whose identity cannot be proven blocks the
// reap: the sweep reports rather than deleting under it.
func TestSweepReportsAnUnprovenProcessInsideTheDir(t *testing.T) {
	f := newSweepFixture(t)
	const ref = "v1.unprovenref001"
	dir := f.session("unproven.HHH888", ref, true, time.Hour)
	f.workspace(dir, false)
	proc := f.agentServer(filepath.Join(dir, sshAfBinaryName), "v1.some-other-ref")

	findings := f.sweep()
	fd, ok := findingFor(findings, dir)
	require.True(t, ok)
	assert.Equal(t, SweepReported, fd.Action)
	assert.True(t, procAlive(proc))
	_, err := os.Stat(dir)
	assert.NoError(t, err)
}

// Candidates must carry both identity files, and only direct children count: the
// host's shared binary cache and stray directories are never candidates.
func TestSweepListIgnoresDirsWithoutIdentity(t *testing.T) {
	f := newSweepFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "bin", "af"), []byte("shared"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(f.root, "stray", "nested"), 0o755))
	real := f.session("real.III999", "v1.listref00000001", true, time.Hour)
	// A symlinked "session dir" is never followed.
	require.NoError(t, os.Symlink(real, filepath.Join(f.root, "link")))

	cands, err := sweepList(f.host.shell, f.root, 15*time.Minute)
	require.NoError(t, err)
	var dirs []string
	for _, c := range cands {
		dirs = append(dirs, filepath.Base(c.dir))
	}
	assert.Equal(t, []string{"real.III999"}, dirs)
	assert.True(t, cands[0].old)
	assert.False(t, strings.Contains(cands[0].token, "\n"))
}

func TestSweepSandboxOrphansNeedsTheKey(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir()) // no runtime-identity key minted
	assert.Empty(t, SweepSandboxOrphans([]SweepRow{{Title: "x", BackendType: "sandbox"}}, time.Minute, nil),
		"with no key there is no proof to make, so nothing is touched")
}

// Anti-vacuous control for the linked-worktree cases: a clean, fully pushed
// workspace WITH a clean linked worktree is still reaped, so the refusals above
// come from the unpushed work and not from the mere presence of a worktree.
func TestSweepReapsACleanWorkspaceWithACleanLinkedWorktree(t *testing.T) {
	f := newSweepFixture(t)
	const ref = "v1.cleanlinked001"
	dir := f.session("work.DDD444", ref, true, time.Hour)
	f.workspace(dir, false)
	w := filepath.Join(dir, sshWorkspaceSubdir)
	f.git(w, "worktree", "add", "-q", "-b", "agent-work", filepath.Join(dir, "linked"))
	f.git(w, "push", "-q", "origin", "agent-work")
	proc := f.agentServer(filepath.Join(dir, sshAfBinaryName), ref)

	findings := f.sweep()

	fd, ok := findingFor(findings, dir)
	require.True(t, ok, "%+v", findings)
	assert.Equal(t, SweepReaped, fd.Action, fd.Reason)
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
	assert.False(t, procAlive(proc))
}

// SweepRowsOf must see what a LIVE instance owns: ToInstanceData stages the
// cleanup identity privately, so an instance-backed projection (not a hand-built
// SweepRow) is what proves the ownership checks can see a running session's dir
// (Codex P2 on #5850).
func TestSweepRowsOfSeesALiveSandboxInstancesIdentity(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "live", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.backend = &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/live.AAA111",
		TokenRef:   "v1.liveref000001",
	}}

	rows := SweepRowsOf([]*Instance{inst, nil})
	require.Len(t, rows, 1, "a live sandbox instance must be projected")
	dir, ref := rows[0].Identity()
	assert.Equal(t, "/remote/af-sessions/live.AAA111", dir)
	assert.Equal(t, "v1.liveref000001", ref)
}
