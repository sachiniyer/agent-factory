//go:build linux

package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestFirstDaemonSessionImportsEnvironmentAfterServerBootstrap covers the
// no-server edge where the daemon creates the dedicated server between its
// initial existence probe and new-session. The first client must still extend
// update-environment so agent credentials reach that pane without becoming
// persistent server-global environment.
func TestFirstDaemonSessionImportsEnvironmentAfterServerBootstrap(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	systemdRun := filepath.Join(dir, "systemd-run")
	script := `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--unit=*|--property=*) shift ;;
        --) shift; break ;;
        *) exit 64 ;;
    esac
done
exec "$@"
`
	if err := os.WriteFile(systemdRun, []byte(script), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("OPENAI_API_KEY", "bootstrap-secret")
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	markerPath := filepath.Join(dir, "credential-observed")
	agentPath := filepath.Join(dir, ProgramCodex)
	agent := `#!/bin/sh
test "${OPENAI_API_KEY:-}" = bootstrap-secret || exit 9
: >"$1"
while :; do sleep 1; done
`
	if err := os.WriteFile(agentPath, []byte(agent), 0o700); err != nil {
		t.Fatalf("write agent shim: %v", err)
	}
	session := NewTmuxSession("first-daemon-environment", strings.Join([]string{
		shellquote.Quote(agentPath), shellquote.Quote(markerPath),
	}, " "))
	if err := session.Start(dir); err != nil {
		t.Fatalf("start first session after dedicated server bootstrap: %v", err)
	}
	t.Cleanup(func() { _, _ = session.CloseAndWaitForPaneExit() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("first pane did not receive its approved OPENAI_API_KEY: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
		shellquote.Quote(agentPath), shellquote.Quote(namesPath), shellquote.Quote(workspacePath), shellquote.Quote(pushMarkerPath),
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
	for _, want := range []string{"PATH", "HOME", "AF_SESSION", "AF_SESSION_GEN", "GH_TOKEN", "OPENAI_API_KEY", customName} {
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

// TestAccountScopedShellTabInheritsSelectedCredentials is the #3340 regression:
// a shell tab is a sibling tmux session, but its credential identity belongs to
// the parent agent session. The shell must receive the selected account root and
// must not receive the daemon's ambient API key.
func TestAccountScopedShellTabInheritsSelectedCredentials(t *testing.T) {
	testguard.IsolateTmux(t)
	forceNewSessionEnvMarkers(t, true)

	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	accountDir, err := agentaccount.Register(home, ProgramCodex, "work")
	if err != nil {
		t.Fatalf("register fixture account: %v", err)
	}
	ambientHome := filepath.Join(home, "ambient-codex")
	t.Setenv("CODEX_HOME", ambientHome)
	t.Setenv("OPENAI_API_KEY", "ambient-must-not-reach-tab")

	dir := t.TempDir()
	agentPath := filepath.Join(dir, ProgramCodex)
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatalf("write agent fixture: %v", err)
	}
	reportPath := filepath.Join(dir, "shell-environment")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	agent := NewTmuxSession("account-tab-parent", ProgramCodex)
	agent.SetAccountForAgent(ProgramCodex, "work")
	if err := agent.Start(dir); err != nil {
		t.Fatalf("start account-scoped parent session: %v", err)
	}
	t.Cleanup(func() { _, _ = agent.CloseAndWaitForPaneExit() })

	shell, err := agent.NewShellSiblingSession("af_account-tab-shell", "/bin/sh")
	if err != nil {
		t.Fatalf("prepare shell tab: %v", err)
	}
	if err := shell.Start(dir); err != nil {
		t.Fatalf("open shell tab: %v", err)
	}
	t.Cleanup(func() { _, _ = shell.CloseAndWaitForPaneExit() })
	reportCommand := "printf 'CODEX_HOME=%s\\n' \"${CODEX_HOME-<unset>}\" > " + shellquote.Quote(reportPath) + "; " +
		"if [ \"${OPENAI_API_KEY+x}\" = x ]; then printf 'OPENAI_API_KEY=present\\n' >> " + shellquote.Quote(reportPath) +
		"; else printf 'OPENAI_API_KEY=absent\\n' >> " + shellquote.Quote(reportPath) + "; fi\n"
	if err := shell.SendRawKeys([]byte(reportCommand)); err != nil {
		t.Fatalf("inspect account-scoped shell environment: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var report []byte
	for time.Now().Before(deadline) {
		report, err = os.ReadFile(reportPath)
		if err == nil && len(report) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("shell tab did not report its environment: %v", err)
	}
	got := string(report)
	if !strings.Contains(got, "CODEX_HOME="+accountDir+"\n") {
		t.Fatalf("shell tab did not inherit selected account root; report:\n%s", got)
	}
	if strings.Contains(got, "CODEX_HOME="+ambientHome+"\n") {
		t.Fatalf("shell tab inherited ambient credential root; report:\n%s", got)
	}
	if !strings.Contains(got, "OPENAI_API_KEY=absent\n") {
		t.Fatalf("shell tab inherited ambient API credentials; report:\n%s", got)
	}
}
