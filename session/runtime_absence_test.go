package session

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingShell struct{}

func (failingShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return nil, errors.New("ssh: connect to host: Connection timed out")
}

// A runtime counts as gone only on POSITIVE evidence from its host: the host
// answered, the dir is absent or someone else's, and no process carries the ref.
// A failed or unanswered probe, a dir that is still this session's, or a process
// that still carries the identity is never absence (#5571 W1.6).
func TestProveRuntimeGoneNeedsPositiveEvidence(t *testing.T) {
	const ref = "v1.absenceref0001"
	local := &sandboxProvisioner{runCommandFn: localSweepShell}

	t.Run("the host answered and the session dir is gone", func(t *testing.T) {
		f := newSweepFixture(t)
		ok, why := proveRuntimeGone(local, filepath.Join(f.root, "never-existed"), ref, time.Second)
		assert.True(t, ok, why)
	})
	t.Run("the dir was reused by another identity", func(t *testing.T) {
		f := newSweepFixture(t)
		dir := f.session("reused.AAA111", "v1.someoneelse0002", true, time.Hour)
		ok, why := proveRuntimeGone(local, dir, ref, time.Second)
		assert.True(t, ok, why)
	})
	t.Run("the dir is still this session's", func(t *testing.T) {
		f := newSweepFixture(t)
		dir := f.session("ours.BBB222", ref, true, time.Hour)
		f.workspace(dir, true)
		ok, why := proveRuntimeGone(local, dir, ref, time.Second)
		assert.False(t, ok, "anti-vacuous: the same probe says not-gone for a present dir")
		assert.Contains(t, why, "still on the host")
	})
	t.Run("the dir is gone but a process still carries the identity", func(t *testing.T) {
		f := newSweepFixture(t)
		dir := filepath.Join(f.root, "gone.CCC333")
		proc := f.agentServer(filepath.Join(f.root, "elsewhere", sshAfBinaryName), ref)
		require.True(t, procAlive(proc))
		ok, why := proveRuntimeGone(local, dir, ref, time.Second)
		assert.False(t, ok, "a live process with this ref is not a gone runtime")
		assert.Contains(t, why, "process")
	})
	t.Run("the probe got no answer", func(t *testing.T) {
		ok, why := proveRuntimeGone(failingShell{}, "/remote/af-sessions/x", ref, time.Second)
		assert.False(t, ok, "an unreachable host proves nothing")
		assert.Contains(t, why, "did not answer")
	})
	t.Run("the probe output is cut short", func(t *testing.T) {
		cut := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			return []byte("HOST-ANSWERED\nDIR-GONE\n"), nil
		}}
		ok, _ := proveRuntimeGone(cut, "/remote/x", ref, time.Second)
		assert.False(t, ok, "a probe that did not finish proves nothing")
	})
	t.Run("an answering shell that never reports the dir", func(t *testing.T) {
		empty := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			return []byte("HOST-ANSWERED\nPROBE-DONE\n"), nil
		}}
		ok, _ := proveRuntimeGone(empty, "/remote/x", ref, time.Second)
		assert.False(t, ok)
	})
	t.Run("no /proc to enumerate", func(t *testing.T) {
		noproc := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			return []byte("HOST-ANSWERED\nDIR-GONE\nNOPROC\nPROBE-DONE\n"), nil
		}}
		ok, why := proveRuntimeGone(noproc, "/remote/x", ref, time.Second)
		assert.False(t, ok)
		assert.Contains(t, why, "/proc")
	})
}

// A row with no persisted identity (written before W1.1, or a kind af cannot
// probe) can never be proven gone: a missing handle is not evidence.
func TestProveRuntimeGoneRefusesARowWithNoPersistedIdentity(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "noident", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	ok, why := inst.ProveRuntimeGone(time.Second)
	assert.False(t, ok)
	assert.Contains(t, why, "no runtime identity")

	inst.backend = &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SSHCommand: "ssh sandbox.invalid", SessionDir: "/remote/af-sessions/x"}}
	ok, why = inst.ProveRuntimeGone(time.Second)
	assert.False(t, ok, "a pre-W1.1 row with no token ref cannot be identity-checked")
	assert.Contains(t, why, "incomplete")
}
