//go:build !linux

package session

import (
	"strings"
	"testing"
)

// The non-linux half of the relabel decision. This file is built only off linux
// — the tests in bind_mount_relabel_test.go skip there, and that skip is exactly
// what hid the macOS regression this pins (#3589 review): with no /proc to read,
// the "cannot prove" branch relabelled, and a colon-bearing AGENT_FACTORY_HOME
// was refused on a platform that cannot enforce SELinux at all.
//
// NOTE the filename: a `_darwin_test.go` suffix would be an implicit GOOS
// constraint for darwin ONLY, which would silently exclude windows.

func TestSelinuxRelabelForHost_NonLinuxNeverRelabels(t *testing.T) {
	if selinuxRelabelForHost() {
		t.Error("SELinux is a linux LSM; a non-linux host has nothing to relabel")
	}
}

// TestDockerAccountMount_NonLinuxKeepsTheColonPath is the regression proper: the
// colon path must still reach Docker's --mount form rather than a refusal.
func TestDockerAccountMount_NonLinuxKeepsTheColonPath(t *testing.T) {
	mount, err := dockerAccountMount("work", "/Users/op/af:home/accounts/codex/work", selinuxRelabelForHost())
	if err != nil {
		t.Fatalf("a colon-bearing account path must still mount off linux: %v", err)
	}
	if len(mount) != 2 || mount[0] != "--mount" || !strings.Contains(mount[1], "src=/Users/op/af:home/accounts/codex/work") {
		t.Errorf("expected Docker's --mount form for a colon path; got %v", mount)
	}
}
