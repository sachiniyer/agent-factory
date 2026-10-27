// Package linger decides whether a systemd user manager outlives its owner's
// last logout. Without lingering, a user unit stops at logout and is not started
// at boot, so `af daemon install` on a headless box would claim reboot survival
// it does not have (W9.8, topo:T4).
package linger

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// lingerDir is where logind records lingering users, one empty file per login
// name. Reading it needs no privilege and no running manager.
const lingerDir = "/var/lib/systemd/linger"

// Seams, replaced by tests.
var (
	goos        = func() string { return runtimeGOOS }
	currentUser = user.Current
	statFile    = os.Stat
	runCommand  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
	dir = lingerDir
)

// Remedy is the exact command that grants the current user lingering.
func Remedy(username string) string {
	return fmt.Sprintf("sudo loginctl enable-linger %s", username)
}

// Ensure makes the current user's systemd manager linger, or returns an error
// naming the exact remedy. It is a no-op off Linux, where no such precondition
// exists. It runs BEFORE the unit is written so a refusal changes nothing.
func Ensure() error {
	if !Applies() {
		return nil
	}
	u, err := currentUser()
	if err != nil {
		return fmt.Errorf("cannot determine the current user to check systemd lingering: %w", err)
	}
	if Enabled(u.Username) {
		return nil
	}
	out, err := runCommand("loginctl", "enable-linger", u.Username)
	if err != nil {
		return fmt.Errorf("the daemon would stop at logout and not start at boot: systemd lingering is off for %q and could not be enabled (%v%s). "+
			"Run `%s`, then re-run `af daemon install`",
			u.Username, err, trimmedSuffix(out), Remedy(u.Username))
	}
	if !Enabled(u.Username) {
		return fmt.Errorf("`loginctl enable-linger %s` returned success but %s/%s is still absent, so the daemon would not survive logout. "+
			"Run `%s` and check that logind is the session manager, then re-run `af daemon install`",
			u.Username, dir, u.Username, Remedy(u.Username))
	}
	return nil
}

// Applies reports whether lingering is a precondition on this platform.
func Applies() bool { return goos() == "linux" }

// Enabled reports whether logind records lingering for username.
func Enabled(username string) bool {
	if username == "" || strings.ContainsAny(username, "/\x00") {
		return false
	}
	_, err := statFile(filepath.Join(dir, username))
	return err == nil
}

func trimmedSuffix(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return ": " + s
}
