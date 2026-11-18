//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// #7494 item 2 — a NON-REGULAR file renamed over the exe path must refuse
// without ever blocking: a FIFO's plain open waits on a writer that never
// comes, which is exactly the stalled-read wedge the budget only bounds
// after it has already spent a worker. The gated open must return —
// O_NONBLOCK plus the regular-file check — so the refusal is immediate and
// no worker is ever parked at all.
func TestOpenRegularExeRefusesANonRegularPathWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "af")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan bool, 1)
	go func() {
		f, ok := openRegularExe(fifo, 0, 0)
		if f != nil {
			f.Close()
		}
		done <- ok
	}()
	select {
	case ok := <-done:
		require.False(t, ok, "a non-regular file at the exe path cannot attest")
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO at the exe path parked the open — the non-regular gate never ran")
	}

	// A regular file still passes the gate on its own (dev, ino): the check
	// binds the text vnode's identity, nothing else.
	regular := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o755))
	st, err := os.Stat(regular)
	require.NoError(t, err)
	sys, ok := st.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	f, ok := openRegularExe(regular, uint64(uint32(sys.Dev)), sys.Ino)
	require.True(t, ok, "a regular file carrying the asked identity opens")
	f.Close()
}

// #7509 item 2 — the name-swap that launders a cached positive on darwin:
// a process execs a FOREIGN binary at path Q under a daemon-claiming argv,
// then an attested af image is moved onto Q. A stat of the path answers
// the attested (dev,ino) — the name now belongs to the af build — while
// the text vnode the kernel is still executing belongs to the foreign
// image. Revalidation must compare the region walk's (dev,ino), never the
// path's: the cached positive must not stand.
func TestExeIdentityReadsTheTextVnodeNotTheMovedName(t *testing.T) {
	dir := t.TempDir()
	// The carrier is a FOREIGN binary under an af name: the sleeper
	// helper's build info names no af module.
	carrier := sleeper.Named(t, filepath.Join(dir, "af"))

	proc := exec.Command(carrier, "300")
	require.NoError(t, proc.Start())
	t.Cleanup(func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() })
	p, err := Lookup(proc.Process.Pid)
	require.NoError(t, err)

	// The text vnode's identity is the foreign image's — the inode the
	// kernel is executing.
	id, ok := exeIdentity(p.PID)
	require.True(t, ok, "the running image's text vnode must answer")

	// Now move a real af build onto the name. stat(Q) will answer the af
	// inode — exactly the verdict the path-based read laundered — while
	// the region walk keeps reporting the foreign text vnode.
	afCopy := filepath.Join(dir, "af-copy")
	copyFile(t, os.Args[0], afCopy)
	require.NoError(t, os.Rename(afCopy, carrier))

	pathID, ok := func() (exeID, bool) {
		var st syscall.Stat_t
		if err := syscall.Stat(carrier, &st); err != nil {
			return exeID{}, false
		}
		return exeID{dev: uint64(uint32(st.Dev)), ino: st.Ino}, true
	}()
	require.True(t, ok)

	current, ok := exeIdentity(p.PID)
	if ok {
		require.Equal(t, id, current,
			"the text vnode does not move when the name does — the region walk keeps reporting the running image")
		require.NotEqual(t, pathID, current,
			"the path now names the af build; the running image is still foreign")
	}
	// !ok is the other honest answer: an unlinked name may drop out of
	// the vnode's path cache, and no answer is unverified — fail-closed
	// either way. What the read may NEVER do is answer pathID.

	// And through the memo: a positive cached against the af image's
	// identity must NOT survive the name swap — the running image never
	// was that file.
	att := NewExeAttestations()
	att.seen[attestKey{pid: p.PID, startID: p.StartID}] = attestEntry{verdict: true, exe: pathID}
	require.False(t, att.AttestsAF(p),
		"stat(Q) would answer the attested inode — the cached positive must not stand on a name")
}
