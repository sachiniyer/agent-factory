//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func startSystemSleep(t *testing.T) int {
	t.Helper()
	c := exec.Command("/bin/sleep", "60")
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	pid := c.Process.Pid
	// Wait for the exec, so the flags read are /bin/sleep's and not the
	// forked copy of this test binary.
	require.Eventually(t, func() bool {
		argv := Argv(pid)
		return len(argv) > 0 && argv[0] == "/bin/sleep"
	}, 5*time.Second, 10*time.Millisecond)
	return pid
}

// TestCodeSigningFlagsReadsCSRestrict pins the csops plumbing #3584 rests on,
// against the measurement in the issue: Apple's /bin/sleep carries
// CS_RESTRICT and a linker-signed Go test binary does not.
func TestCodeSigningFlagsReadsCSRestrict(t *testing.T) {
	pid := startSystemSleep(t)
	flags, err := codeSigningFlags(pid)
	require.NoError(t, err)
	require.NotZero(t, flags&csRestrict, "/bin/sleep flags %#x lack CS_RESTRICT", flags)

	self, err := codeSigningFlags(os.Getpid())
	require.NoError(t, err)
	require.Zero(t, self&csRestrict, "the test binary's flags %#x carry CS_RESTRICT", self)
}

// TestSIPRestrictsDtraceAgreesWithCsrutil checks the csrctl read against the
// operator-facing tool. It asserts only on a plain "enabled." or "disabled."
// status: a custom SIP configuration can go either way on the dtrace bit.
func TestSIPRestrictsDtraceAgreesWithCsrutil(t *testing.T) {
	restricted, err := sipRestrictsDtrace()
	require.NoError(t, err)
	out, err := exec.Command("csrutil", "status").CombinedOutput()
	if err != nil {
		t.Skipf("csrutil status unavailable: %v: %s", err, out)
	}
	status := strings.TrimSpace(string(out))
	switch {
	case strings.HasSuffix(status, "status: disabled."):
		require.False(t, restricted, "csrutil says %q", status)
	case strings.HasSuffix(status, "status: enabled."):
		require.True(t, restricted, "csrutil says %q", status)
	default:
		t.Skipf("custom SIP configuration: %q", status)
	}
}

// TestWithheldEnvCauseMatchesTheRead holds on either SIP state: with the
// dtrace restriction in force, /bin/sleep's environment is withheld and
// attributed. Without it (GitHub's runners), the environment is served and
// nothing is attributed.
func TestWithheldEnvCauseMatchesTheRead(t *testing.T) {
	pid := startSystemSleep(t)
	restricted, err := sipRestrictsDtrace()
	require.NoError(t, err)

	_, envErr := Environ(pid)
	cause, ok := WithheldEnvCause(pid)
	if restricted {
		require.Error(t, envErr, "SIP is on, so /bin/sleep's environment should be withheld")
		require.True(t, ok)
		require.NotEmpty(t, cause)
	} else {
		require.NoError(t, envErr, "SIP is off, so /bin/sleep's environment should be served")
		require.False(t, ok)
	}

	_, ok = WithheldEnvCause(os.Getpid())
	require.False(t, ok, "our own environment is always served")
}
