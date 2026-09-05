package sessionenv

import (
	"slices"
	"strings"
	"testing"
)

// The login pane runs on the DAEMON host, which is headless and usually remote
// (#3854). A browser-callback login there either opens a browser nobody is
// sitting in front of or listens for an OAuth redirect on the daemon's own
// localhost, which the operator's machine cannot reach. So each agent's login
// pane gets the environment that selects its browser-free flow, and the values
// are pinned here because they are the whole feature.
func TestAccountLoginEnvironment_SelectsTheBrowserFreeFlowPerAgent(t *testing.T) {
	for agent, want := range map[string][]string{
		// gemini 0.51.0: NO_BROWSER makes Config.isBrowserLaunchSuppressed() true,
		// which routes the sign-in through authWithUserCode — "Please visit the
		// following URL to authorize the application:" then "Enter the
		// authorization code: ".
		"gemini": {"NO_BROWSER=true"},
		// claude 2.1.261 has no such flag, and its opener reads BROWSER and spawns
		// it with the URL. `true` is the no-op the CLI itself recognizes.
		"claude": {"BROWSER=true"},
		// codex 0.153.2 takes a FLAG (`codex login --device-auth`), so there is
		// nothing for the environment to say.
		"codex": nil,
	} {
		got := AccountLoginEnvironment(agent)
		if !slices.Equal(got, want) {
			t.Fatalf("AccountLoginEnvironment(%q) = %v, want %v", agent, got, want)
		}
	}
	if got := AccountLoginEnvironment("amp"); got != nil {
		t.Fatalf("AccountLoginEnvironment(%q) = %v, want nil for an agent off the login roster", "amp", got)
	}
}

// The names travel separately from the values because the tmux launcher has to
// name them in three places — the pass-through allowlist the pane's exec shim
// re-applies, the client environment they are stripped from, and tmux's
// update-environment list — and only set the value once.
func TestAccountLoginEnvironmentNames_MatchTheEntries(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "gemini"} {
		entries := AccountLoginEnvironment(agent)
		names := AccountLoginEnvironmentNames(agent)
		if len(names) != len(entries) {
			t.Fatalf("AccountLoginEnvironmentNames(%q) = %v, want one name per entry %v", agent, names, entries)
		}
		for idx, entry := range entries {
			name, _, ok := strings.Cut(entry, "=")
			if !ok || names[idx] != name {
				t.Fatalf("AccountLoginEnvironmentNames(%q)[%d] = %q, want the name of %q", agent, idx, names[idx], entry)
			}
		}
	}
}

// A SESSION IS NOT A LOGIN, and this is the half of #3854 that is easy to get
// wrong. NO_BROWSER changes how the gemini CLI behaves for the whole run, and
// BROWSER redirects every URL the agent opens; handing either to an
// account-scoped WORKING session would change the agent's behaviour for reasons
// that have nothing to do with the session. The login-pane environment must
// therefore be invisible to both account boundaries.
func TestAccountLoginEnvironment_NeverReachesAnAccountScopedSession(t *testing.T) {
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: "/home/op/.agent-factory/accounts/" + agent + "/" + name}, nil
	}
	t.Cleanup(func() { AccountLookup = nil })

	source := []string{"PATH=/usr/bin", "HOME=/home/op"}
	for _, agent := range []string{"claude", "codex", "gemini"} {
		account := Account{Agent: agent, Name: "work", Dir: "/home/op/.agent-factory/accounts/" + agent + "/work"}
		scoped, err := ApplyAccount(source, agent, account)
		if err != nil {
			t.Fatalf("ApplyAccount(%q): %v", agent, err)
		}
		environmentOnly, err := ApplyAccountEnvironment(source, "make -j4", account)
		if err != nil {
			t.Fatalf("ApplyAccountEnvironment(%q): %v", agent, err)
		}
		resolved, err := ResolveAccountEnvironment(agent, "work")
		if err != nil {
			t.Fatalf("ResolveAccountEnvironment(%q): %v", agent, err)
		}
		for _, environ := range [][]string{scoped, environmentOnly, resolved} {
			for _, name := range []string{"NO_BROWSER", "BROWSER"} {
				if environmentHasName(environ, name) {
					t.Fatalf("an account-scoped %s session carries the login pane's %s: %v", agent, name, environ)
				}
			}
		}
	}
}

func environmentHasName(environ []string, name string) bool {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
