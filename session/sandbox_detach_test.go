package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W1.4: the agent-server launch detaches into its own session where setsid
// exists, guarded so hosts without it (macOS) keep the nohup-only launch, and
// `$!` must still report the agent-server's own PID.
func TestStartAgentServerDetachesWithSetsidWhenAvailable(t *testing.T) {
	var launch string
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		launch = script
		return []byte("4242\n"), nil
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/d", spec: ProvisionSpec{Title: "s"}}
	require.NoError(t, w.startAgentServer(time.Second))
	assert.Contains(t, launch, "command -v setsid", "setsid must be probed, not assumed (macOS has none)")
	assert.Contains(t, launch, "nohup $d ", "the detach prefix rides in front of the agent-server")
	assert.True(t, strings.HasSuffix(launch, "& echo $!"), "the PID capture stays last: %s", launch)
	assert.Equal(t, "4242", w.RemotePID)
}

// The detach prefix, run through a real POSIX shell, must report the PID of the
// process that carries the agent-server's argv — the reap-side identity check
// depends on it.
func TestSetsidDetachPrefixKeepsTheReportedPID(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	out, err := exec.Command("sh", "-c",
		`d=; if command -v setsid >/dev/null 2>&1; then d=setsid; fi; nohup $d sleep 30 >/dev/null 2>&1 </dev/null & echo $!`).Output()
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		return err == nil && strings.HasPrefix(string(cmdline), "sleep\x00")
	}, 3*time.Second, 20*time.Millisecond, "the reported PID must be the exec'd process, not a wrapper")
}
