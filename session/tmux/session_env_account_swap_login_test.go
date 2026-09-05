package tmux

import (
	"slices"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// accountLoginBrowserNames are the variables af's ACCOUNT LOGIN flow sets to
// route a sign-in through its device-code shape instead of a browser callback
// (#3854): gemini reads NO_BROWSER, and claude reads BROWSER and spawns it with
// the URL, so BROWSER=true is the no-op that suppresses the launch.
//
// They belong to a login pane and nowhere else. The login pane runs on the
// daemon's host, which is headless — but a SESSION pane is the thing a human is
// attached to, and one that inherited these would silently change how its agent
// behaves: an agent that would have opened a browser stops doing so, or one that
// tries to exec a program literally named "true" as the operator's browser.
var accountLoginBrowserNames = []string{"NO_BROWSER", "BROWSER"}

// An account swap re-launches a session's panes under a NEW identity, on the
// daemon poll loop — the same process that runs account logins. If the daemon's
// own environment ever carried the login suppressors (an operator's shell, a
// systemd unit, or a future login implementation that exported them rather than
// scoping them to its pane), the replacement panes would inherit them and the
// swapped session would quietly behave differently from the one it replaced.
//
// What actually prevents that is the allowlist being SUBTRACTIVE and
// default-deny, not any check written at the swap. That makes this a test worth
// pinning: the property is a consequence of a list neither #3127 nor #3854 owns,
// so nothing else fails when someone adds these names to it.
//
// Both pane shapes are covered because a swap restarts both: the agent pane
// through the account launch boundary, and every credential-bearing sibling
// through the environment-only one.
func TestAccountLaunchEnvironmentExcludesLoginBrowserSuppressors(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if _, err := agentaccount.Register(home, ProgramCodex, "work"); err != nil {
		t.Fatal(err)
	}
	for _, name := range accountLoginBrowserNames {
		t.Setenv(name, "true")
	}

	agent := NewTmuxSession("swap-replacement", "codex")
	agent.SetAccountForAgent("codex", "work")
	sibling := agent.NewSiblingSession("swap-replacement-worker", "npm run dev")

	for _, tc := range []struct {
		name    string
		session *TmuxSession
	}{
		{"agent pane", agent},
		{"account-scoped sibling", sibling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, environ, imports, err := tc.session.launchEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range accountLoginBrowserNames {
				if launchEnvironmentHasName(environ, name) {
					t.Errorf("a swapped %s inherited the login-only browser suppressor %s", tc.name, name)
				}
				if slices.Contains(imports, name) {
					t.Errorf("a swapped %s imported the login-only browser suppressor %s from the tmux server", tc.name, name)
				}
			}
		})
	}
}

// The refusal above must not be a blanket one: an operator who names these in
// session_env_passthrough has asked for them in every session, and af does not
// second-guess an explicit choice. Without this half, deleting the names from
// commonNames would satisfy the test above for the wrong reason — and so would a
// pass-through path that silently dropped every name it was given.
func TestAccountLaunchEnvironmentStillHonorsExplicitBrowserPassthrough(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if _, err := agentaccount.Register(home, ProgramCodex, "work"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", "/usr/bin/firefox")

	agent := NewTmuxSession("swap-replacement-optin", "codex")
	agent.SetAccountForAgent("codex", "work")
	if err := agent.SetEnvPassthrough([]string{"BROWSER"}); err != nil {
		t.Fatal(err)
	}

	_, environ, _, err := agent.launchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := launchEnvironmentValue(environ, "BROWSER"); !ok || got != "/usr/bin/firefox" {
		t.Fatalf("BROWSER = %q, %v; want the operator's explicit pass-through value", got, ok)
	}
}
