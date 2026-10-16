//go:build windows

package sessionenv

import (
	"fmt"
	"os"
)

func WrapCommand(string, string, []string, string) (string, error) {
	return "", fmt.Errorf("tmux session environments are unsupported on windows")
}

func WrapAccountCommand(string, string, string, AccountLaunchProof, []string, string) (string, error) {
	return "", fmt.Errorf("tmux session environments are unsupported on windows")
}

func WrapAccountEnvironmentCommand(string, string, string, []string, string) (string, error) {
	return "", fmt.Errorf("tmux session environments are unsupported on windows")
}

func WrapAgentServerCommand(string, []string, []string) (string, error) {
	return "", fmt.Errorf("agent-server session environments are unsupported on windows")
}

func HandleInternalExec() {}

// spawnDirInodeID has no meaning off unix: Windows tmux panes do not exist,
// so no inode identity travels and the shim-side verification never runs.
func spawnDirInodeID(os.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
