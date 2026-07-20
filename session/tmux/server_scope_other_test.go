//go:build !linux

package tmux

import (
	"strings"
	"testing"
)

// This runs on the macOS CI leg added by #1931. It pins the launchd path
// explicitly so a Linux-only systemd wrapper cannot go green by accident
// (#2039): Darwin must keep invoking tmux directly.
func TestNewTmuxServerCommandNonLinuxStaysDirect(t *testing.T) {
	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
	if scoped {
		t.Fatal("non-Linux tmux command was marked systemd-scoped")
	}
	want := "tmux new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("non-Linux tmux command = %q, want %q", got, want)
	}
}
