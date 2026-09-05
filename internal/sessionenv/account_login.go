package sessionenv

import "strings"

// The LOGIN PANE's environment (#3854), which is a different thing from an
// account-scoped session's and must stay one.
//
// af runs an agent's own login flow in a tmux pane on the DAEMON's host. That
// host is headless and usually remote — the operator is on a laptop over
// Tailscale — so the two browser-based shapes both fail there: opening a browser
// means opening it on a screen nobody is looking at, and waiting for an OAuth
// redirect means waiting on the daemon's own localhost, which the operator's
// machine cannot reach. The device-code shape is the one that works: the CLI
// prints a URL and a code, the human signs in from whatever device they are
// actually holding, and the CLI polls.
//
// Each agent selects that shape differently, and only the ones that select it
// through the ENVIRONMENT are here. codex takes a flag, so its lever lives in
// agentaccount's loginCommands beside the rest of its argv.
//
// A SESSION IS NOT A LOGIN. Nothing here is applied by ApplyAccount or
// ApplyAccountEnvironment, and that omission is deliberate rather than pending:
// NO_BROWSER changes how the gemini CLI behaves for an entire working session,
// and BROWSER redirects every URL the agent would open for its user. Both are
// correct for a pane whose only job is a sign-in and wrong for a pane doing
// work, so the login pane names them explicitly and nothing else can inherit
// them.
var accountLoginEnvironment = map[string][]string{
	// claude 2.1.261 has NO browser-free flag — `claude auth login --help` lists
	// --claudeai, --console, --email and --sso and nothing else — so the lever is
	// its URL opener, which reads BROWSER and spawns it with the URL as its only
	// argument. `true` is the POSIX no-op, and the CLI recognizes that exact
	// spelling as "not a real browser" in its own should-open predicate.
	//
	// Measured on this box against 2.1.261, with a recording stand-in on PATH as
	// xdg-open. With neither BROWSER nor DISPLAY set — which is af's pane today,
	// since the environment boundary allowlists neither — the opener short-circuits
	// on Linux and spawns NOTHING; with DISPLAY=:0 it spawned the stand-in, and on
	// the LOCALHOST callback URL, which is precisely the redirect a remote operator
	// can never reach. With BROWSER pointing at the stand-in and no DISPLAY it
	// spawned the stand-in too — so a BROWSER arriving through
	// session_env_passthrough defeats the no-display short-circuit. BROWSER=true
	// with DISPLAY=:0 spawned neither.
	//
	// In every case the CLI printed "If the browser didn't open, visit: <the
	// MANUAL URL>" and then "Paste code here if prompted > ", with a reader on
	// stdin for the pasted `code#state`. So the manual path is the one that runs,
	// and this pins it there rather than leaving it to what the host happens to
	// have set.
	"claude": {"BROWSER=true"},
	// gemini 0.51.0 reads NO_BROWSER into Config.noBrowser, and
	// isBrowserLaunchSuppressed() — `getNoBrowser() || !shouldAttemptBrowserLaunch()`
	// — routes the sign-in through authWithUserCode, which writes "Please visit
	// the following URL to authorize the application:" followed by the URL and
	// then prompts "Enter the authorization code: ". Read out of the installed
	// bundle rather than assumed; it is not in `gemini --help`.
	//
	// shouldAttemptBrowserLaunch() already answers false on a Linux host with no
	// DISPLAY/WAYLAND_DISPLAY/MIR_SOCKET, so today's pane usually takes this path
	// anyway. "Usually" is the problem: that half of the predicate does not exist
	// on a macOS daemon host, and a DISPLAY passed through session_env_passthrough
	// turns it off on Linux. NO_BROWSER is the half that does not depend on the
	// host.
	"gemini": {"NO_BROWSER=true"},
}

// AccountLoginEnvironment returns the NAME=VALUE entries af adds to one agent's
// login pane, in order. The result is a copy, and nil for an agent whose
// browser-free flow needs nothing from the environment.
func AccountLoginEnvironment(agent string) []string {
	entries := accountLoginEnvironment[agent]
	if len(entries) == 0 {
		return nil
	}
	return append([]string(nil), entries...)
}

// AccountLoginEnvironmentNames returns just the names, in the same order.
//
// The names travel separately from the values because the launcher needs them in
// three places and the value in only one: the pane's exec shim re-filters
// os.Environ() against a default-deny allowlist immediately before exec, so the
// name has to be admitted there or the value never reaches the agent; the
// daemon's own copy has to be stripped from the client environment af hands
// tmux; and tmux's update-environment list has to name it so an existing server
// unsets a stale copy instead of reviving it in the new session.
func AccountLoginEnvironmentNames(agent string) []string {
	entries := accountLoginEnvironment[agent]
	if len(entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			names = append(names, name)
		}
	}
	return names
}
