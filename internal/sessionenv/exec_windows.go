//go:build windows

package sessionenv

import "fmt"

func WrapCommand(string, string, []string, string) (string, error) {
	return "", fmt.Errorf("tmux session environments are unsupported on windows")
}

func HandleInternalExec() {}
