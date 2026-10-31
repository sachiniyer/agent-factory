//go:build windows

package tmux

import "fmt"

func (t *TmuxSession) mintLaunchCapability(launchSpec) (string, string, error) {
	return "", "", fmt.Errorf("account-scoped launch capabilities are unsupported on windows")
}

func revokeLaunchCapabilities(string) {}

func revokeSingleUseLaunchCapabilities(string) {}

func launchCapabilities(string) (int, int) { return 0, 0 }
