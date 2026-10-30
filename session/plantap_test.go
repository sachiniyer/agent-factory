package session

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

func TestClaudeLaunchRunsThePlanTapAsItsStatusLine(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pinTapBinary(t, "/opt/it's af/af")

	command := injectSystemPrompt("claude", skillTarget{})
	_, ok := sessionenv.GeneratedArgsBetween("claude", command)
	require.True(t, ok, "the account boundary must be able to declare the generated words")
	// The shell, not a Go parser, says what claude receives.
	out, err := exec.Command("/bin/sh", "-c", "set -- "+strings.TrimPrefix(command, "claude ")+`; printf '%s\n%s' "$3" "$4"`).Output()
	require.NoError(t, err)
	flag, value, _ := strings.Cut(string(out), "\n")
	require.Equal(t, "--settings", flag)
	var settings struct {
		StatusLine struct {
			Type            string `json:"type"`
			Command         string `json:"command"`
			RefreshInterval int    `json:"refreshInterval"`
		} `json:"statusLine"`
	}
	require.NoError(t, json.Unmarshal([]byte(value), &settings))
	require.Equal(t, "command", settings.StatusLine.Type)
	require.Equal(t, `'/opt/it'\''s af/af' internal plan-tap`, settings.StatusLine.Command)
	require.Equal(t, 60, settings.StatusLine.RefreshInterval)

	afExecutable = func() (string, error) { return "", errors.New("no binary") }
	command = injectSystemPrompt("claude", skillTarget{})
	require.Contains(t, command, "--plugin-dir ")
	require.False(t, strings.Contains(command, "--settings"), command)
	require.NotContains(t, injectSystemPrompt("codex", skillTarget{}), "--settings")
}

func pinTapBinary(t *testing.T, exe string) {
	t.Helper()
	prevExe, prevLook, prevManaged := afExecutable, afLookPath, managedSettingsFiles
	afExecutable = func() (string, error) { return exe, nil }
	afLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	managedSettingsFiles = nil
	t.Cleanup(func() { afExecutable, afLookPath, managedSettingsFiles = prevExe, prevLook, prevManaged })
}

func tapStatusLine(t *testing.T, settings string) map[string]any {
	t.Helper()
	var s struct {
		StatusLine map[string]any `json:"statusLine"`
	}
	require.NoError(t, json.Unmarshal([]byte(settings), &s))
	return s.StatusLine
}

func TestClaudeLaunchKeepsTheUsersOwnSettingsFlag(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	pinTapBinary(t, "/opt/af")
	for _, resolved := range []string{
		"claude --settings /home/me/s.json",
		"claude --settings=/home/me/s.json",
		`claude --settings '{"model":"x"}'`,
		"claude --model x --settings",
	} {
		command := injectSystemPrompt(resolved, skillTarget{})
		require.Equal(t, 1, strings.Count(command, "--settings"), command)
		require.Contains(t, command, "--plugin-dir ")
	}
	require.Contains(t, injectSystemPrompt("claude --setting-sources user", skillTarget{}), " --settings ")
}

func TestPlanTapSettingsKeepsTheUsersRefreshAndPadding(t *testing.T) {
	pinTapBinary(t, "/opt/af")
	root := t.TempDir()
	require.Equal(t, float64(60), tapStatusLine(t, planTapSettings(root))["refreshInterval"])

	require.NoError(t, os.WriteFile(filepath.Join(root, "settings.json"),
		[]byte(`{"statusLine":{"type":"command","command":"x","refreshInterval":5,"padding":2}}`), 0o644))
	sl := tapStatusLine(t, planTapSettings(root))
	require.Equal(t, float64(5), sl["refreshInterval"])
	require.Equal(t, float64(2), sl["padding"])
	require.Equal(t, "'/opt/af' internal plan-tap", sl["command"])

	t.Setenv("CLAUDE_CONFIG_DIR", root)
	require.Equal(t, float64(2), tapStatusLine(t, planTapSettings(""))["padding"], "the ambient config dir is read when unscoped")
}

func TestTapBinaryPrefersTheAfOnPathWhenItIsTheSameFile(t *testing.T) {
	dir := t.TempDir()
	versioned := filepath.Join(dir, "Cellar", "af", "1.2", "af")
	require.NoError(t, os.MkdirAll(filepath.Dir(versioned), 0o755))
	require.NoError(t, os.WriteFile(versioned, []byte("#!/bin/sh\n"), 0o755))
	stable := filepath.Join(dir, "bin", "af")
	require.NoError(t, os.MkdirAll(filepath.Dir(stable), 0o755))
	require.NoError(t, os.Symlink(versioned, stable))
	other := filepath.Join(dir, "other-af")
	require.NoError(t, os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755))

	pinTapBinary(t, versioned)
	afLookPath = func(string) (string, error) { return stable, nil }
	got, err := tapBinary()
	require.NoError(t, err)
	require.Equal(t, stable, got)

	afLookPath = func(string) (string, error) { return other, nil }
	got, _ = tapBinary()
	require.Equal(t, versioned, got, "a different af on PATH is not used")

	afLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	got, _ = tapBinary()
	require.Equal(t, versioned, got)
}
