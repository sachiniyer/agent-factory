//go:build linux

package sessionenv

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(entries)
}

// A peer can attach SCM_RIGHTS descriptors to its request; the kernel installs
// them in the broker. They must be closed and the message refused.
func TestCapability_BrokerClosesDescriptorsAttachedToARequest(t *testing.T) {
	_, sock := startBroker(t, paneOf("s", os.Getpid()))
	extra, err := os.Open("/dev/null")
	require.NoError(t, err)
	defer extra.Close()

	before := openFDCount(t)
	for i := 0; i < 20; i++ {
		conn, err := net.Dial("unix", sock)
		require.NoError(t, err)
		uc := conn.(*net.UnixConn)
		_, _, err = uc.WriteMsgUnix([]byte("{\"Hello\":true}\n"), unix.UnixRights(int(extra.Fd()), int(extra.Fd()), int(extra.Fd())), nil)
		require.NoError(t, err)
		// The broker refuses and closes; wait for that.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		require.Zero(t, n, "a message carrying descriptors must be refused without an answer")
		conn.Close()
	}
	require.Eventually(t, func() bool { return openFDCount(t) <= before }, 5*time.Second, 20*time.Millisecond,
		"the broker leaked descriptors received over SCM_RIGHTS: before=%d now=%d", before, openFDCount(t))
}
