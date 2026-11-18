//go:build darwin

package proctree

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
