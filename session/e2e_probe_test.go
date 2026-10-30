package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2EProductionResolveProgramOnHost exercises the PRODUCTION entry point
// ResolveProgramOnHost (which calls loginShellName(os.Getenv) and execProbeRunner)
// with a real non-POSIX login shell set via SHELL= and a startup profile that puts
// a fake agent on PATH. This is the path af agent-server takes on an off-box host.
// Uses exec.LookPath (which does NOT resolve symlinks, matching loginShellName's
// raw $SHELL read) so /usr/bin/csh stays "csh" for the hostProbeSpec switch.
func TestE2EProductionResolveProgramOnHost(t *testing.T) {
	for _, shellName := range []string{"fish", "csh", "tcsh"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s not installed: %v", shellName, err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", shell)
			t.Setenv("USER", "")
			if shellName == "fish" {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			}
			agentBin := filepath.Join(home, "agent-bin")
			require.NoError(t, os.MkdirAll(agentBin, 0o755))
			agentExe := filepath.Join(agentBin, "myagent")
			require.NoError(t, os.WriteFile(agentExe, []byte("#!/bin/sh\necho ok\n"), 0o755))
			var profile string
			var profilePath string
			switch shellName {
			case "fish":
				profile = fmt.Sprintf("set -gx PATH %s $PATH\n", agentBin)
				profilePath = filepath.Join(home, ".config/fish/conf.d/test.fish")
			case "csh":
				profile = fmt.Sprintf("setenv PATH %s:$PATH\n", agentBin)
				profilePath = filepath.Join(home, ".login")
			case "tcsh":
				profile = fmt.Sprintf("setenv PATH %s:$PATH\n", agentBin)
				profilePath = filepath.Join(home, ".login")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(profilePath), 0o755))
			require.NoError(t, os.WriteFile(profilePath, []byte(profile), 0o644))
			t.Setenv("PATH", "/usr/local/go/bin:/usr/bin:/bin")

			var warn logtest.Buffer
			prev := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warn)
			t.Cleanup(func() { log.WarningLog.SetOutput(prev) })

			command, loginPATH, err := ResolveProgramOnHost(context.Background(), "myagent")
			require.NoError(t, err, "warnings: %s", warn.String())
			assert.Equal(t, agentExe, command, "resolved from login PATH (warnings: %s)", warn.String())
			assert.Contains(t, strings.Split(loginPATH, ":"), agentBin,
				"pane adopts login PATH (warnings: %s)", warn.String())
			assert.Empty(t, warn.String(), "successful resolution must not warn")
		})
	}
}

// TestE2EProductionResolveProgramOnHostBash verifies the POSIX (bash) case is
// unchanged end-to-end through the production entry point: the login profile's
// PATH is adopted and the agent resolves from it.
func TestE2EProductionResolveProgramOnHostBash(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not installed: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", shell)
	t.Setenv("USER", "")
	agentBin := filepath.Join(home, "agent-bin")
	require.NoError(t, os.MkdirAll(agentBin, 0o755))
	agentExe := filepath.Join(agentBin, "myagent")
	require.NoError(t, os.WriteFile(agentExe, []byte("#!/bin/sh\necho ok\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"),
		[]byte(fmt.Sprintf("export PATH=%s:$PATH\n", agentBin)), 0o644))
	t.Setenv("PATH", "/usr/bin:/bin")

	command, loginPATH, err := ResolveProgramOnHost(context.Background(), "myagent")
	require.NoError(t, err)
	assert.Equal(t, agentExe, command)
	assert.Contains(t, strings.Split(loginPATH, ":"), agentBin)
}

// TestE2EProductionResolveProgramOnHostBrokenShell verifies the warning +
// accurate diagnostic when the login shell cannot be probed (section G).
func TestE2EProductionResolveProgramOnHostBrokenShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/nonexistent/shell")
	t.Setenv("USER", "")
	t.Setenv("PATH", "/usr/bin:/bin")

	var warn logtest.Buffer
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warn)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })

	_, _, err := ResolveProgramOnHost(context.Background(), "claude")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spawning environment PATH")
	assert.Contains(t, err.Error(), "/nonexistent/shell")
	assert.Contains(t, warn.String(), "could not probe the login-shell PATH")
	assert.Contains(t, warn.String(), "/nonexistent/shell")
}
