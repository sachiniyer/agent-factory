//go:build darwin

package proctree

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// #7492: PROC_PIDPATHINFO answers through the BUFFER, not the syscall return
// — proc_pidpathinfo_internal never writes retval, so a successful read
// returns 0 and the NUL-terminated path is the only answer. Treating that 0
// as "no answer" made every real af build unverifiable on darwin. Assert the
// contract end to end for the one process the kernel must always answer for:
// the caller itself.
func TestExePathAnswersForSelfThroughProcInfo(t *testing.T) {
	path, ok := exePath(os.Getpid())
	require.True(t, ok,
		"proc_info(PROC_PIDPATHINFO) must answer for our own pid — its retval is unused, the buffer is the answer")
	bi, err := buildinfo.ReadFile(path)
	require.NoError(t, err, "the kernel-attested path is a readable binary")
	require.Equal(t, afModulePath, bi.Main.Path,
		"the kernel-attested binary carries this module's build info")
}

// The forged-daemon verdict relies on `exec -a` landing in kern.procargs2.
// It does — for a binary the kernel does not restrict. A host's own binary
// (/usr/bin/yes, a cs_restricted platform binary) can have its procargs2
// withheld even from a same-uid reader, so it can never serve as the forged
// carrier; the unsigned test sleeper's argv reads everywhere. Pin the
// channel this test file's forgery depends on.
func TestForgedArgvReachesProcArgs2ForAnUnsignedBinary(t *testing.T) {
	carrier := sleeper.Named(t, filepath.Join(t.TempDir(), "af"))
	forged := exec.Command(carrier, "--daemon")
	forged.Args[0] = "af"
	require.NoError(t, forged.Start())
	t.Cleanup(func() { _ = forged.Process.Kill(); _, _ = forged.Process.Wait() })
	require.Eventually(t, func() bool {
		return ArgvIsAFDaemon(Argv(forged.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond,
		"exec -a af … --daemon on an unsigned binary must land in kern.procargs2")
}
