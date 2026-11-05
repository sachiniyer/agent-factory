//go:build windows

package probe

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// probeExecSupported is false on Windows: there is no process-group primitive
// af can use here to guarantee a timeout ends the launcher's whole tree, and
// the credential boundary refuses what it cannot prove rather than running a
// probe whose descendant could keep spending quota after the verdict. Every
// probe Request on this platform resolves to a refusal.
const probeExecSupported = false

func setupProcGroup(c *exec.Cmd) {} // unreachable: probes refuse first

func cancelViaGroup(c *exec.Cmd) func() error { return nil }

func sweepProcGroup(c *exec.Cmd) {}

// accountHomeKey on Windows falls back to the canonical path spelling — an
// unreachable path today because probes refuse before keying matters, kept
// honest so a future Windows implementation starts from a real identity.
func accountHomeKey(dir string, info os.FileInfo) string {
	return "path:" + strings.ToLower(filepath.Clean(dir))
}

var errNoAuthLock = errors.New("auth.lock is not supported on this platform")

func holdAuthLock(path string) (func(), error) { return nil, errNoAuthLock }
