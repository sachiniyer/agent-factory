package tmux

import (
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// registerLoginAccount makes a throwaway AF home holding one account, and
// returns its directory. Nothing here starts tmux: every assertion below reads
// prepareLaunchEnvironment, which only computes what Start would pass.
func registerLoginAccount(t *testing.T, agent, name string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	dir, err := agentaccount.Register(home, agent, name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The login pane's environment is what makes the flow browser-free (#3854). The
// daemon host is headless and remote, so a login that opens a browser there — or
// waits on an OAuth redirect to the daemon's own localhost — is one nobody can
// finish. Each agent gets exactly the lever that selects its device-code path,
// and codex gets none because its lever is a flag on the command.
func TestLoginPaneEnvironmentSelectsTheBrowserFreeFlow(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)

	for _, tc := range []struct {
		agent   string
		program string
		want    map[string]string
		absent  []string
	}{
		{
			agent:   ProgramGemini,
			program: "gemini",
			want:    map[string]string{"NO_BROWSER": "true"},
			absent:  []string{"BROWSER"},
		},
		{
			agent:   ProgramClaude,
			program: "claude auth login",
			want:    map[string]string{"BROWSER": "true"},
			absent:  []string{"NO_BROWSER"},
		},
		{
			agent:   ProgramCodex,
			program: "codex login --device-auth",
			want:    nil,
			absent:  []string{"NO_BROWSER", "BROWSER"},
		},
	} {
		dir := registerLoginAccount(t, tc.agent, "work")
		pane := NewTmuxSession("af-login-"+tc.agent+"-work", tc.program)
		// TWICE, because a setter that wrote its own additions back onto the
		// pass-through list would read them as the operator's on the second call
		// and drop the lever — silently, and only on a retry.
		pane.SetAccountLoginForAgent(tc.agent, "work")
		pane.SetAccountLoginForAgent(tc.agent, "work")

		wrapped, launchEnv, imports, sessionEnv, _, err := pane.prepareLaunchEnvironment()
		if err != nil {
			t.Fatalf("%s login pane: %v", tc.agent, err)
		}
		configVar, _ := sessionenv.SupportsAccounts(tc.agent)
		if got, ok := launchEnvironmentValue(sessionEnv, configVar); !ok || got != dir {
			t.Fatalf("%s login pane %s = %q, %v; want the account directory %q", tc.agent, configVar, got, ok, dir)
		}
		for name, want := range tc.want {
			got, ok := launchEnvironmentValue(sessionEnv, name)
			if !ok || got != want {
				t.Fatalf("%s login pane %s = %q, %v; want %q", tc.agent, name, got, ok, want)
			}
			// The pane's exec shim re-filters os.Environ() against the default-deny
			// allowlist immediately before exec, so a value tmux holds but the shim
			// does not admit never reaches the agent.
			if !slices.Contains(strings.Fields(wrapped), name) {
				t.Fatalf("%s login pane sets %s but does not admit it through the exec shim's allowlist: %s",
					tc.agent, name, wrapped)
			}
			// Named in update-environment so an EXISTING tmux server unsets its stale
			// copy rather than reviving the daemon's own value in the new session.
			if !slices.Contains(imports, name) {
				t.Fatalf("%s login pane did not name %s in the tmux import list", tc.agent, name)
			}
			// And stripped from the client environment af hands tmux, so the value
			// the session gets is af's and not the daemon's.
			if launchEnvironmentHasName(launchEnv, name) {
				t.Fatalf("%s login pane leaked the daemon's own %s into the tmux client environment", tc.agent, name)
			}
		}
		for _, name := range tc.absent {
			if launchEnvironmentHasName(sessionEnv, name) {
				t.Fatalf("%s login pane carries %s, which is not its browser-free lever", tc.agent, name)
			}
			if slices.Contains(strings.Fields(wrapped), name) {
				t.Fatalf("%s login pane allowlisted %s, which is not its browser-free lever: %s",
					tc.agent, name, wrapped)
			}
		}
	}
}

// A SESSION IS NOT A LOGIN. NO_BROWSER changes how the gemini CLI behaves for a
// whole run and BROWSER redirects every URL the agent opens, so neither may
// reach a working account-scoped session — not the agent pane, not a process
// tab, not a shell tab, which are the three shapes an account can be applied to.
func TestAccountScopedSessionsNeverGetTheLoginBrowserLevers(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)

	for _, agent := range []string{ProgramGemini, ProgramClaude, ProgramCodex} {
		registerLoginAccount(t, agent, "work")

		session := NewTmuxSession("account-"+agent, agent)
		session.SetAccountForAgent(agent, "work")
		process := session.NewSiblingSession("account-"+agent+"-process", "make -j4")
		shell, err := session.NewShellSiblingSession("account-"+agent+"-shell", "/bin/sh")
		if err != nil {
			t.Fatal(err)
		}

		for label, pane := range map[string]*TmuxSession{
			"agent": session, "process": process, "shell": shell,
		} {
			wrapped, launchEnv, _, sessionEnv, defaultCommand, err := pane.prepareLaunchEnvironment()
			if err != nil {
				t.Fatalf("%s %s session: %v", agent, label, err)
			}
			for _, name := range []string{"NO_BROWSER", "BROWSER"} {
				if launchEnvironmentHasName(sessionEnv, name) {
					t.Fatalf("account-scoped %s %s session carries the login pane's %s", agent, label, name)
				}
				if launchEnvironmentHasName(launchEnv, name) {
					t.Fatalf("account-scoped %s %s session admitted %s into the tmux client environment", agent, label, name)
				}
				if slices.Contains(strings.Fields(wrapped), name) {
					t.Fatalf("account-scoped %s %s session allowlisted %s for the exec shim: %s",
						agent, label, name, wrapped)
				}
			}
			if defaultCommand == "" {
				t.Fatalf("account-scoped %s %s session lost its scoped default command", agent, label)
			}
		}
	}
}

// The operator's own pass-through WINS. session_env_passthrough is the escape
// hatch for a daemon host that genuinely has a browser to open, and af's
// browser-free default must be a default rather than a wall — the same
// safe-default-plus-escape-hatch rule the rest of the account boundary follows.
func TestLoginPaneLeavesAnExplicitBrowserPassthroughAlone(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	registerLoginAccount(t, ProgramClaude, "work")
	t.Setenv("BROWSER", "/usr/bin/firefox")

	pane := NewTmuxSession("af-login-claude-work", "claude auth login")
	if err := pane.SetEnvPassthrough([]string{"BROWSER"}); err != nil {
		t.Fatal(err)
	}
	pane.SetAccountLoginForAgent(ProgramClaude, "work")

	wrapped, launchEnv, _, sessionEnv, _, err := pane.prepareLaunchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Fields(wrapped), "BROWSER") {
		t.Fatalf("the operator's BROWSER pass-through lost its exec-shim allowlist entry: %s", wrapped)
	}
	if launchEnvironmentHasName(sessionEnv, "BROWSER") {
		t.Fatal("af overrode an operator's explicit BROWSER pass-through on the login pane")
	}
	if got, ok := launchEnvironmentValue(launchEnv, "BROWSER"); !ok || got != "/usr/bin/firefox" {
		t.Fatalf("login pane BROWSER = %q, %v; want the operator's pass-through value", got, ok)
	}
}
