//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// The poison the consensus now refuses, staged on a real darwin x86_64
// kernel: a FOREIGN carrier execs at path Q under a daemon-claiming argv,
// a real af build is moved onto Q, and the carrier mmaps Q — which now
// resolves to the af build — at a fixed address BELOW its own PIE text
// (0x100000000). The unfiltered PROC_PIDREGIONPATHINFO2 walk used to win
// the cursor order on that low mapping and return the AF inode, so
// exeOpen(Q) opened the af build and buildinfo attested AF for a process
// that never ran an af byte (#7514). The fold in collectTextVnode now
// sees TWO distinct inodes naming the same exe path — the foreign text
// vnode and the af inode mapped under the name — and fails closed.
//
// arm64 is excluded by the kernel, not the test: load_machfile enforces a
// 4 GiB hard pagezero, so MAP_FIXED below the text region returns
// KERN_INVALID_ADDRESS and the poison cannot be staged. The test skips on
// arm64 — the attack is x86_64-only, and so is the kernel-level witness.
func TestExeAttestsAFRefusesAMmapSpoofOfTheTextVnode(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the mmap-below-text poison requires x86_64 darwin; arm64's hard 4 GiB pagezero blocks it")
	}
	dir := t.TempDir()
	// The carrier is FOREIGN: the sleeper helper's build info names no af
	// module. Exec'd at Q, its text vnode is the sleeper's inode and the
	// kernel name-caches the path Q.
	carrier := filepath.Join(dir, "af")
	sleeper.Named(t, carrier)

	// The af build to mmap — never execute. A copy of this test binary,
	// whose build info names the af module. mv'd onto Q so the af inode's
	// name cache picks up the path the kernel reports as the exe.
	afCopy := filepath.Join(dir, "af-mmap")
	copyFile(t, os.Args[0], afCopy)

	// The carrier mmaps its own exe path at a low fixed address (2 MiB,
	// page-aligned, well under the PIE text at 0x100000000) — but ONLY
	// after the test stages the name swap, so the mmap reads the af build,
	// not the sleeper. The gate file is the deterministic handshake: the
	// sleeper waits for it before the mmap, the test creates it after the
	// mv has landed.
	gate := filepath.Join(dir, "gate")
	const mmapAddr = "0x200000"
	proc := exec.Command(carrier, "300")
	proc.Env = append(os.Environ(),
		sleeper.MmapPathEnv+"="+carrier,
		sleeper.MmapAddrEnv+"="+mmapAddr,
		sleeper.MmapGateEnv+"="+gate)
	require.NoError(t, proc.Start())
	t.Cleanup(func() {
		_ = os.Remove(gate)
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	})
	p, err := Lookup(proc.Process.Pid)
	require.NoError(t, err)

	// Before the swap the carrier attests as foreign: its text vnode is
	// the sleeper's inode, and the sleeper's build info names no af
	// module. This is the baseline the poison would.override.
	require.False(t, ExeAttestsAF(p),
		"a foreign binary under an af name attests as foreign before any poison is staged")

	// Stage the name swap: Q now opens the af build. stat(Q) answers the
	// af inode — exactly the verdict the path-based read the original
	// walk authorized.
	require.NoError(t, os.Rename(afCopy, carrier))
	afID := func() exeID {
		var st syscall.Stat_t
		require.NoError(t, syscall.Stat(carrier, &st))
		return exeID{dev: uint64(uint32(st.Dev)), ino: st.Ino}
	}()

	// Release the gate: the carrier mmaps Q (= the af build now) at the
	// low fixed address. From here the region walk sees TWO distinct
	// inodes naming the exe path — the foreign text vnode (high, PROT_EXEC)
	// and the af inode the carrier mapped under the name (low, PROT_READ).
	require.NoError(t, os.WriteFile(gate, []byte("go"), 0o644))

	// Wait for the mmap to land so the walk sees both. The signature of
	// the poison is that exeIdentity stops answering: the consensus
	// fails closed on the two distinct inodes, never returning the af
	// inode the buggy first-match walk returned.
	require.Eventually(t, func() bool {
		_, ok := exeIdentity(p.PID)
		return !ok
	}, 5*time.Second, 20*time.Millisecond,
		"the mmap'd af region must land — the consensus must fail closed on the two distinct inodes naming Q")

	// The verdict: the running image is foreign, so attestation must
	// refuse even though the path now opens an af build and one region
	// naming the exe path carries the af inode. The buggy first-match
	// walk returned true here; the consensus returns false.
	require.False(t, ExeAttestsAF(p),
		"a foreign process that maps — but never executes — an af build under its exe path must not attest")

	// And through the memo: a positive cached against the af inode must
	// not survive — the running image never was that file. The
	// revalidation read is the SAME walk, so the consensus fails closed
	// here too, and the cached positive is dropped.
	att := NewExeAttestations()
	att.seen[attestKey{pid: p.PID, startID: p.StartID}] = attestEntry{verdict: true, exe: afID}
	require.False(t, att.AttestsAF(p),
		"the cached positive on the af inode must not stand — the revalidation walk fails closed on the same poison")
}
