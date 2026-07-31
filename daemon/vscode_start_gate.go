package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const vscodeStartGateExt = ".start"

// vscodeStartGateScript keeps the harmless shell launcher behind a bounded gate
// until the daemon has durably recorded its identity. A daemon crash before the
// record is written therefore leaves, at worst, a shell that exits on its own;
// it never execs an editor that no replacement daemon can identify and reap.
const vscodeStartGateScript = `
gate=$1
shift
i=0
while [ "$i" -lt 200 ]; do
	if [ -f "$gate" ]; then
		rm -f -- "$gate"
		exec "$@"
		exit 127
	fi
	sleep 0.05
	i=$((i + 1))
done
exit 124
`

func vscodeStartGatePath(socketPath string) string {
	return socketPath + vscodeStartGateExt
}

func newGatedVSCodeCommand(binary string, args []string, gatePath string) *exec.Cmd {
	gateArgs := []string{"-c", vscodeStartGateScript, "af-vscode-start", gatePath, binary}
	gateArgs = append(gateArgs, args...)
	return newDaemonChildCommand("/bin/sh", gateArgs...)
}

func releaseVSCodeStartGate(path string) error {
	if err := os.WriteFile(path, []byte("start\n"), 0o600); err != nil {
		return fmt.Errorf("releasing editor startup gate: %w", err)
	}
	return nil
}

func (s *vscodeServer) processGroupAlive(pgid int) (bool, error) {
	if s.groupAlive != nil {
		return s.groupAlive(pgid)
	}
	return processGroupAlive(pgid)
}

func (s *vscodeServer) waitForProcessGroupExit(pgid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := s.processGroupAlive(pgid)
		if err != nil || !alive {
			return !alive, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
