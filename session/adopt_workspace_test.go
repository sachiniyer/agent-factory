package session

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preservedHostDir builds, on this machine, what an archive without a push leaves
// on a host: <dir>/workspace is a clone and <dir>/workspace-<seg> is its linked
// worktree on branch. The worktree carries committed, staged, unstaged,
// untracked and ignored state, which the proof must not disturb.
func preservedHostDir(t *testing.T, branch string) (dir, worktree string) {
	t.Helper()
	dir = t.TempDir()
	run := func(where string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", where, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	clone := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(clone, 0o755))
	run(clone, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(clone, "base.txt"), []byte("base"), 0o644))
	run(clone, "add", "base.txt")
	run(clone, "commit", "-q", "-m", "base")
	worktree = filepath.Join(dir, "workspace-wt")
	run(clone, "worktree", "add", "-q", "-b", branch, worktree)
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(worktree, name), []byte(body), 0o644))
	}
	write("committed.txt", "c")
	run(worktree, "add", "committed.txt")
	run(worktree, "commit", "-q", "-m", "work")
	write("staged.txt", "s")
	run(worktree, "add", "staged.txt")
	write("committed.txt", "c-edited")
	write("untracked.txt", "u")
	exclude := strings.TrimSpace(adoptGitOut(t, worktree, "rev-parse", "--git-path", "info/exclude"))
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(worktree, exclude)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(exclude), 0o755))
	require.NoError(t, os.WriteFile(exclude, []byte("*.ign\n"), 0o644))
	write("keep.ign", "i")
	return dir, worktree
}

func adoptGitOut(t *testing.T, where string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", where}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func treeFingerprint(t *testing.T, worktree string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(adoptGitOut(t, worktree, "status", "--porcelain", "--ignored"))
	b.WriteString(adoptGitOut(t, worktree, "rev-parse", "HEAD"))
	for _, f := range []string{"committed.txt", "staged.txt", "untracked.txt", "keep.ign"} {
		body, err := os.ReadFile(filepath.Join(worktree, f))
		require.NoError(t, err)
		b.WriteString(f + "=" + string(body) + "\n")
	}
	return b.String()
}

func runAdoptCheck(t *testing.T, dir, branch, pid string) (string, error) {
	t.Helper()
	w := &sandboxWorkspace{
		shell: &sandboxProvisioner{runCommandFn: func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
			return localShell{}.Run(d, script, stdin, combined)
		}},
		spec: ProvisionSpec{Adopt: &AdoptSpec{SessionDir: dir, Branch: branch, OldPID: pid}},
	}
	return w.adoptWorkspace(time.Minute)
}

func TestAdoptProofFindsThePreservedWorktreeAndWritesNothing(t *testing.T) {
	dir, worktree := preservedHostDir(t, "af/feature")
	before := treeFingerprint(t, worktree)

	got, err := runAdoptCheck(t, dir, "af/feature", "")
	require.NoError(t, err)
	assert.Equal(t, worktree, got)
	assert.Equal(t, before, treeFingerprint(t, worktree),
		"the tree, index and ignored files are exactly as archived after the proof")
}

func TestAdoptProofGoneOnlyWhenTheHostSaysSo(t *testing.T) {
	t.Run("session dir absent", func(t *testing.T) {
		_, err := runAdoptCheck(t, filepath.Join(t.TempDir(), "nope"), "b", "")
		assert.ErrorIs(t, err, ErrPreservedWorkspaceGone)
	})
	t.Run("clone absent", func(t *testing.T) {
		_, err := runAdoptCheck(t, t.TempDir(), "b", "")
		assert.ErrorIs(t, err, ErrPreservedWorkspaceGone)
	})
	t.Run("a transport failure is not gone", func(t *testing.T) {
		w := &sandboxWorkspace{
			shell: &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
				return nil, errors.New("ssh: connection refused")
			}},
			spec: ProvisionSpec{Adopt: &AdoptSpec{SessionDir: "/d", Branch: "b"}},
		}
		_, err := w.adoptWorkspace(time.Minute)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPreservedWorkspaceGone, "an unreachable host proves nothing about the workspace")
	})
}

func TestAdoptProofRefusesWhatItCannotProve(t *testing.T) {
	dir, worktree := preservedHostDir(t, "af/feature")
	before := treeFingerprint(t, worktree)

	t.Run("a different branch", func(t *testing.T) {
		_, err := runAdoptCheck(t, dir, "af/other", "")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPreservedWorkspaceGone)
		assert.Contains(t, err.Error(), "af/other")
	})
	t.Run("the worktree directory is missing from a present clone", func(t *testing.T) {
		d2, wt2 := preservedHostDir(t, "af/feature")
		require.NoError(t, os.RemoveAll(wt2))
		_, err := runAdoptCheck(t, d2, "af/feature", "")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPreservedWorkspaceGone,
			"a present clone may still hold the work; only a vanished clone is gone")
		assert.Contains(t, err.Error(), "missing")
	})
	t.Run("the previous agent-server still runs", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "sleep 30", "x", dir)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		_, err := runAdoptCheck(t, dir, "af/feature", itoa(cmd.Process.Pid))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still running")
	})
	t.Run("an unrelated process with that pid is not a survivor", func(t *testing.T) {
		cmd := exec.Command("sleep", "30")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		got, err := runAdoptCheck(t, dir, "af/feature", itoa(cmd.Process.Pid))
		require.NoError(t, err)
		assert.Equal(t, worktree, got)
	})
	assert.Equal(t, before, treeFingerprint(t, worktree), "no refusal wrote to the workspace")
}

func TestParseAdoptVerdictNeverReadsGarbageAsGone(t *testing.T) {
	for _, out := range []string{"", "\n", "Permission denied", "GONE-ish", "PRESENT", "PRESENT   ", "OTHER"} {
		_, gone, err := parseAdoptVerdict(out)
		assert.False(t, gone, "%q", out)
		assert.Error(t, err, "%q", out)
	}
	_, gone, err := parseAdoptVerdict("motd noise\nGONE\n")
	assert.True(t, gone)
	assert.NoError(t, err)
}

func TestAgentServerCommandCarriesTheAdoptedWorktree(t *testing.T) {
	w := &sandboxWorkspace{SessionDir: "/d", spec: ProvisionSpec{Title: "t", Branch: "b"}, adoptedWorktree: "/d/workspace-wt"}
	cmd, err := w.agentServerCommand()
	require.NoError(t, err)
	assert.Contains(t, cmd, "--adopt-worktree")
	assert.Contains(t, cmd, "/d/workspace-wt")

	w.adoptedWorktree = ""
	cmd, err = w.agentServerCommand()
	require.NoError(t, err)
	assert.NotContains(t, cmd, "--adopt-worktree")
}

// The decision, with the provisioning injected: adopt when the workspace is there,
// replace only when the host proved it gone, and refuse in every other case.
func TestRestorePreservedWorkspaceDecision(t *testing.T) {
	newRow := func(branch string) (*Instance, *ArchivedWorkspaceData) {
		w := &ArchivedWorkspaceData{Host: "box", Dir: "/d"}
		backend, teardown, err := restoreRuntimeCleanup("t", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
			SSHCommand: "ssh recorded-host", SessionDir: "/d",
		}})
		require.NoError(t, err)
		return &Instance{Title: "t", Branch: branch, backend: backend, runtimeTeardown: teardown,
			archivedWorkspace: cloneArchivedWorkspace(w)}, w
	}
	t.Run("present: adopted, nothing replaced", func(t *testing.T) {
		inst, w := newRow("b")
		var calls []*AdoptSpec
		got, err := inst.restorePreservedWorkspaceWith(w, func(a *AdoptSpec) error { calls = append(calls, a); return nil })
		require.NoError(t, err)
		assert.Equal(t, w, got)
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0])
		assert.Equal(t, "/d", calls[0].SessionDir)
		assert.Equal(t, "b", calls[0].Branch)
		assert.NotNil(t, inst.ArchivedWorkspace(), "the record stays until the launch succeeds")
	})
	t.Run("proven gone: replaced onto the recorded branch", func(t *testing.T) {
		inst, w := newRow("b")
		var calls []*AdoptSpec
		got, err := inst.restorePreservedWorkspaceWith(w, func(a *AdoptSpec) error {
			calls = append(calls, a)
			if a != nil {
				return ErrPreservedWorkspaceGone
			}
			return nil
		})
		require.NoError(t, err)
		assert.Nil(t, got, "a replacement is a first-time launch")
		require.Len(t, calls, 2)
		assert.Nil(t, calls[1])
		assert.Nil(t, inst.ArchivedWorkspace())
	})
	t.Run("proven gone with no recorded branch: not at all", func(t *testing.T) {
		inst, w := newRow("")
		n := 0
		_, err := inst.restorePreservedWorkspaceWith(w, func(*AdoptSpec) error { n++; return ErrPreservedWorkspaceGone })
		require.Error(t, err)
		assert.Equal(t, 1, n, "no replacement may be attempted without a branch to land on")
		assert.NotNil(t, inst.ArchivedWorkspace())
	})
	t.Run("anything else refuses and replaces nothing", func(t *testing.T) {
		inst, w := newRow("b")
		n := 0
		_, err := inst.restorePreservedWorkspaceWith(w, func(*AdoptSpec) error { n++; return errors.New("host unreachable") })
		require.Error(t, err)
		assert.Equal(t, 1, n)
		assert.NotNil(t, inst.ArchivedWorkspace())
	})
}

// A failed adopting provision stops what it started and removes nothing.
func TestAbandonAdoptionKeepsThePreservedWorkspace(t *testing.T) {
	dir, worktree := preservedHostDir(t, "af/feature")
	before := treeFingerprint(t, worktree)
	var scripts []string
	p := &sandboxProvisioner{
		sessionDir: dir,
		spec:       ProvisionSpec{Adopt: &AdoptSpec{SessionDir: dir, Branch: "af/feature"}},
		runCommandFn: func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
			scripts = append(scripts, script)
			return localShell{}.Run(d, script, stdin, combined)
		},
	}
	err := p.provisionFailure(errors.New("banner never appeared"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was not touched")
	for _, s := range scripts {
		assert.NotContains(t, s, "rm -rf", "an adopting provision never removes the workspace")
	}
	assert.DirExists(t, dir)
	assert.Equal(t, before, treeFingerprint(t, worktree))
}

// A launch that fails after the reattach puts the row back as the archive left it.
func TestRollbackAdoptionRestoresThePreservedRecordAndKeepsTheWorkspace(t *testing.T) {
	inst, dir, _ := preservedSessionWithAccount(t, nil)
	stops := attachStoppableAgent(t, inst, false)
	w := &ArchivedWorkspaceData{Host: "box1", Dir: dir}

	inst.rollbackAdoption(w)

	assert.Equal(t, int32(1), stops.Load(), "the agent-server started for the attempt is stopped")
	assert.Nil(t, inst.remoteClient)
	assert.Equal(t, w, inst.ArchivedWorkspace(), "restored as a preserved archive, so a retry adopts it again")
	assert.FileExists(t, filepath.Join(dir, "workspace", "work.txt"), "the work stays")
	assert.NoDirExists(t, filepath.Join(dir, "account"), "credentials synced for the attempt do not stay")
}

// fakeSSH writes an ssh stand-in that logs its argv and answers with out (or exits
// with code), so a test can see which transport a command rode.
func fakeSSH(t *testing.T, log, out string, code int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	body := "#!/bin/sh\necho \"$@\" >> " + shellQuote(log) + "\n"
	if out != "" {
		body += "echo " + shellQuote(out) + "\n"
	}
	body += "exit " + itoa(code) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// #5947 review (C): the proof reaches the host through the transport the row
// recorded. A host list or repo config that changed since the archive is never
// consulted, so a different machine cannot answer GONE for this workspace.
func TestAdoptProofRidesTheRecordedTransport(t *testing.T) {
	recordedLog := filepath.Join(t.TempDir(), "recorded.log")
	currentLog := filepath.Join(t.TempDir(), "current.log")
	recorded := fakeSSH(t, recordedLog, "GONE", 0)
	current := fakeSSH(t, currentLog, "PRESENT /elsewhere/wt", 0)

	backend, teardown, err := restoreRuntimeCleanup("t", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: recorded + " recorded-host", SessionDir: "/remote/af-sessions/x",
	}})
	require.NoError(t, err)
	inst := &Instance{Title: "t", backend: backend, runtimeTeardown: teardown,
		archivedWorkspace: &ArchivedWorkspaceData{Dir: "/remote/af-sessions/x"}}

	tr, err := inst.recordedTransport()
	require.NoError(t, err)
	assert.Equal(t, recorded+" recorded-host", tr.SSHCmd)

	spec := ProvisionSpec{Adopt: &AdoptSpec{SessionDir: "/remote/af-sessions/x", Branch: "b", Transport: tr}}
	// What a provision composes for its shell, with the current configuration
	// offering a different machine.
	p := &sandboxProvisioner{spec: spec, sshCmd: adoptedTransportOr(spec, current+" current-host")}
	w := &sandboxWorkspace{shell: p, spec: spec}
	_, err = w.adoptWorkspace(time.Minute)
	assert.ErrorIs(t, err, ErrPreservedWorkspaceGone)

	got, rerr := os.ReadFile(recordedLog)
	require.NoError(t, rerr)
	assert.Contains(t, string(got), "recorded-host")
	_, serr := os.Stat(currentLog)
	assert.True(t, os.IsNotExist(serr), "the current configuration's host must never be asked")
}

func TestAdoptProofOnAnUnreachableRecordedHostIsAnErrorNeverGone(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ssh.log")
	unreachable := fakeSSH(t, log, "", 255)
	p := &sandboxProvisioner{sshCmd: unreachable + " recorded-host"}
	w := &sandboxWorkspace{shell: p, spec: ProvisionSpec{Adopt: &AdoptSpec{SessionDir: "/d", Branch: "b"}}}
	_, err := w.adoptWorkspace(time.Minute)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPreservedWorkspaceGone)
}

func TestRecordedTransportCarriesTheSSHIdentityAndNeedsOne(t *testing.T) {
	t.Run("a row with no recorded transport cannot be probed", func(t *testing.T) {
		inst := &Instance{Title: "t", backend: &LocalBackend{}}
		_, err := inst.recordedTransport()
		require.Error(t, err)
	})
	t.Run("an ssh row keeps its recorded config, posture and pin", func(t *testing.T) {
		backend, teardown, err := restoreRuntimeCleanup("t", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
			Config:     config.SSHConfig{Host: "recorded.example", User: "u"},
			SessionDir: "/remote/af-sessions/x", HostName: "buildbox",
			HostKeyVerification: config.SSHHostKeyStrict,
		}})
		require.NoError(t, err)
		inst := &Instance{Title: "t", backend: backend, runtimeTeardown: teardown}
		tr, err := inst.recordedTransport()
		require.NoError(t, err)
		require.NotNil(t, tr.SSH)
		assert.Equal(t, "recorded.example", tr.SSH.Config.Host)
		assert.Equal(t, "buildbox", tr.SSH.HostName)
		assert.Contains(t, tr.SSHCmd, "recorded.example")
		assert.NotNil(t, tr.Prepare)
	})
}

func TestAdoptedTransportOrPrefersTheRecordedOne(t *testing.T) {
	assert.Equal(t, "current", adoptedTransportOr(ProvisionSpec{}, "current"))
	assert.Equal(t, "current", adoptedTransportOr(ProvisionSpec{Adopt: &AdoptSpec{}}, "current"))
	assert.Equal(t, "recorded -p 22", adoptedTransportOr(
		ProvisionSpec{Adopt: &AdoptSpec{Transport: &RecordedTransport{SSHCmd: " recorded -p 22 "}}}, "current"))
}

// #5947 review (A): the gate between "the tunnel listens" and the first agent RPC.
func TestWaitAdoptedServerReady(t *testing.T) {
	prev := adoptReadyTimeout
	t.Cleanup(func() { adoptReadyTimeout = prev })

	t.Run("waits through a not-yet-ready server", func(t *testing.T) {
		adoptReadyTimeout = 10 * time.Second
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "ok"}})
		}))
		defer srv.Close()
		require.NoError(t, waitAdoptedServerReady(strings.TrimPrefix(srv.URL, "http://"), "tok", "t"))
		assert.GreaterOrEqual(t, calls.Load(), int32(3))
	})
	t.Run("a tunnel that never answers is an error", func(t *testing.T) {
		adoptReadyTimeout = 600 * time.Millisecond
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close()) // the port now refuses, like a tunnel whose ssh exited
		err = waitAdoptedServerReady(addr, "tok", "t")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not answer")
	})
}

// The restore-checkpoint rules of #5992 and the proven-gone rule of W8.6 hold
// together: an adoption never consults origin, a verified checkpoint is fetched as
// is, an unverified one is checked against origin before it is discarded, and a
// workspace the host proved gone is replaced onto its recorded branch or not at all.
func TestSettleRestoreCheckpoint(t *testing.T) {
	prev := branchOnOrigin
	t.Cleanup(func() { branchOnOrigin = prev })
	var asked int
	stub := func(onOrigin bool, err error) {
		asked = 0
		branchOnOrigin = func(string, string) (bool, error) { asked++; return onOrigin, err }
	}
	newRow := func() *Instance { return &Instance{Title: "t", Branch: "af/b", branchUnpushed: true} }
	spec := func() *ProvisionSpec {
		return &ProvisionSpec{CloneURL: "git@example.com:o/r.git", RestoreBranch: "af/b"}
	}

	t.Run("an adoption fetches nothing and never consults origin", func(t *testing.T) {
		stub(false, errors.New("origin unreachable"))
		s := spec()
		require.NoError(t, newRow().settleRestoreCheckpoint(s, "af/b", true, true, false))
		assert.Zero(t, asked)
	})
	t.Run("a verified checkpoint (a legacy row) is fetched without asking", func(t *testing.T) {
		stub(false, nil)
		s := spec()
		require.NoError(t, newRow().settleRestoreCheckpoint(s, "af/b", false, false, false))
		assert.Zero(t, asked)
		assert.Equal(t, "af/b", s.RestoreBranch)
	})
	t.Run("an unobserved push that origin holds is fetched and marked pushed", func(t *testing.T) {
		stub(true, nil)
		row, s := newRow(), spec()
		require.NoError(t, row.settleRestoreCheckpoint(s, "af/b", true, false, false))
		assert.Equal(t, "af/b", s.RestoreBranch)
		assert.False(t, row.branchUnpushed)
	})
	t.Run("an ordinary replacement discards a branch origin lacks", func(t *testing.T) {
		stub(false, nil)
		s := spec()
		require.NoError(t, newRow().settleRestoreCheckpoint(s, "af/b", true, false, false))
		assert.Equal(t, "", s.RestoreBranch)
	})
	t.Run("a proven-gone workspace is replaced onto its branch or not at all", func(t *testing.T) {
		stub(false, nil)
		s := spec()
		err := newRow().settleRestoreCheckpoint(s, "af/b", true, false, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nothing to restore it from")
		assert.Equal(t, "af/b", s.RestoreBranch, "the spec is untouched on a refusal")
	})
	t.Run("an origin that cannot be asked refuses in every mode", func(t *testing.T) {
		for _, replacing := range []bool{false, true} {
			stub(false, errors.New("auth"))
			require.Error(t, newRow().settleRestoreCheckpoint(spec(), "af/b", true, false, replacing))
		}
	})
}

// The record of a preserved workspace survives a refused replacement, so a retry
// reaches the same proven-gone verdict rather than the ordinary path.
func TestRefusedReplacementKeepsThePreservedRecord(t *testing.T) {
	inst := &Instance{Title: "t", Branch: "b", archivedWorkspace: &ArchivedWorkspaceData{Dir: "/d"}}
	backend, teardown, err := restoreRuntimeCleanup("t", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh recorded-host", SessionDir: "/d",
	}})
	require.NoError(t, err)
	inst.backend, inst.runtimeTeardown = backend, teardown
	_, err = inst.restorePreservedWorkspaceWith(inst.ArchivedWorkspace(), func(a *AdoptSpec) error {
		if a != nil {
			return ErrPreservedWorkspaceGone
		}
		return errors.New("branch not on origin")
	})
	require.Error(t, err)
	assert.NotNil(t, inst.ArchivedWorkspace())
}
