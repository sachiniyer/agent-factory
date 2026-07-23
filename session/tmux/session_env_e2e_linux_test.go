//go:build linux

package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRealPaneEnvironmentIsFiltered reads variable names from the pane
// process's own /proc environment on the package's private tmux server. It
// covers the full production chain: Start -> tmux -> internal exec shim ->
// agent-named program, including a pre-existing tmux server.
func TestRealPaneEnvironmentIsFiltered(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	const (
		customName = "CUSTOM_PROVIDER_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv("ANTHROPIC_API_KEY", "test-value")
	t.Setenv(deniedName, "test-value")
	forceNewSessionEnvMarkers(t, true)

	// Create the private package tmux server before the Codex authentication and
	// explicit pass-through variables enter the client environment. This makes
	// the test exercise the existing-server import path instead of accidentally
	// passing because a fresh server snapshotted the values at startup.
	seedName := "af_session_env_seed"
	seedServer := exec.Command("tmux", "new-session", "-d", "-s", seedName, "sleep", "30")
	if err := seedServer.Run(); err != nil {
		t.Fatal("could not prepare the isolated pre-existing tmux server")
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+seedName).Run() })
	originalUpdateEnvironment, err := exec.Command("tmux", "show-options", "-gv", "update-environment").Output()
	if err != nil {
		t.Fatal("could not read the isolated tmux server environment policy")
	}
	t.Setenv("OPENAI_API_KEY", "test-value")
	t.Setenv(customName, "test-value")

	dir := t.TempDir()
	namesPath := filepath.Join(dir, "environment-names")
	pushMarkerPath := filepath.Join(dir, "push-complete")
	workspacePath := filepath.Join(dir, "workspace")
	remotePath := filepath.Join(dir, "remote.git")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--bare", remotePath},
		{"-C", workspacePath, "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal("could not prepare the isolated Git push fixture")
		}
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "tracked.txt"), []byte("session environment push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", workspacePath, "add", "tracked.txt"},
		{"-C", workspacePath, "-c", "user.name=Agent Factory Test", "-c", "user.email=test@example.invalid", "commit", "-m", "test session push"},
		{"-C", workspacePath, "remote", "add", "origin", remotePath},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal("could not prepare the isolated Git push fixture")
		}
	}
	agentPath := filepath.Join(dir, ProgramCodex)
	program := "#!/bin/sh\n" +
		"test -n \"$OPENAI_API_KEY\" && test -n \"$CUSTOM_PROVIDER_TOKEN\" || exit 9\n" +
		"tr '\\000' '\\n' < /proc/$$/environ | sed 's/=.*//' | sort > \"$1\"\n" +
		"if git -C \"$2\" push origin HEAD:refs/heads/session-env-e2e >/dev/null 2>&1; then : > \"$3\"; fi\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(agentPath, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}

	session := NewTmuxSession("real-env-boundary", strings.Join([]string{
		shellQuoteArg(agentPath), shellQuoteArg(namesPath), shellQuoteArg(workspacePath), shellQuoteArg(pushMarkerPath),
	}, " "))
	if err := session.SetEnvPassthrough([]string{customName}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = session.CloseAndWaitForPaneExit() })
	restoredUpdateEnvironment, err := exec.Command("tmux", "show-options", "-gv", "update-environment").Output()
	if err != nil {
		t.Fatal("could not read the restored tmux server environment policy")
	}
	if string(restoredUpdateEnvironment) != string(originalUpdateEnvironment) {
		t.Fatal("session launch did not restore the existing tmux server environment policy")
	}

	var names []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(namesPath)
		if err == nil && len(data) > 0 {
			names = strings.Fields(string(data))
			if _, pushErr := os.Stat(pushMarkerPath); pushErr == nil {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(names) == 0 {
		t.Fatal("pane did not report its environment names")
	}
	if _, err := os.Stat(pushMarkerPath); err != nil {
		t.Fatal("pane process could not push with its filtered environment")
	}
	if err := exec.Command("git", "--git-dir", remotePath, "rev-parse", "--verify", "refs/heads/session-env-e2e").Run(); err != nil {
		t.Fatal("pane push did not create the expected remote branch")
	}
	t.Logf("pane environment: count=%d names=%s", len(names), strings.Join(names, ","))
	for _, want := range []string{"PATH", "HOME", "AF_SESSION", "GH_TOKEN", "OPENAI_API_KEY", customName} {
		if want == "GH_TOKEN" && os.Getenv(want) == "" {
			continue
		}
		if !slices.Contains(names, want) {
			t.Fatalf("pane environment omitted allowed variable %s", want)
		}
	}
	for _, denied := range []string{deniedName, "ANTHROPIC_API_KEY"} {
		if slices.Contains(names, denied) {
			t.Fatalf("pane environment retained disallowed variable %s", denied)
		}
	}
}
