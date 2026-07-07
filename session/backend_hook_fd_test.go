package session

import (
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestHookBackendEnsurePTYClosesStalePreviewStdout(t *testing.T) {
	dir := t.TempDir()
	attachCmd := writeScript(t, dir, "attach.sh", `echo "preview for $1"; exit 0`)
	b := &HookBackend{Hooks: config.RemoteHooks{AttachCmd: attachCmd}}
	i := &Instance{Title: "stale-fd-test", Path: t.TempDir(), backend: b}

	require.NoError(t, b.ensurePTY(i))

	var first *hookPTY
	require.Eventually(t, func() bool {
		first = b.getPTY(i.Title)
		if first == nil {
			return false
		}
		first.mu.Lock()
		closed := first.closed
		first.mu.Unlock()
		return closed
	}, 2*time.Second, 20*time.Millisecond, "preview process did not exit")
	firstStdout := first.stdout

	require.NoError(t, b.ensurePTY(i))
	require.ErrorIs(t, firstStdout.Close(), os.ErrClosed,
		"stale preview stdout fd must be closed before ensurePTY deletes the old entry")

	b.closePTY(i.Title)
}
