//go:build windows

package tmux

import "fmt"

func (t *TmuxSession) mintLaunchCapability(launchSpec) (string, string, error) {
	return "", "", fmt.Errorf("account-scoped launch capabilities are unsupported on windows")
}

func revokeLaunchCapabilities(string) {}

func (t *TmuxSession) beginCapabilityPrepare()    {}
func (t *TmuxSession) revokePendingCapabilities() {}
func (t *TmuxSession) commitCapabilities(bool)    {}

func launchCapabilities(string) (int, int) { return 0, 0 }
