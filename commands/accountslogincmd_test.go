package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// The `af accounts login` CLI (#3384). What is worth pinning here is not that
// the verb reaches the daemon — that is one call — but the branches AROUND it,
// each of which is a decision the operator experiences: does af take the
// terminal, does it report the login as done, and does it say so when the flow
// left the account empty. The daemon call and the tmux attach are both seams, so
// every branch is reachable without a daemon or a tmux server.

// stubAccountLogin replaces the daemon call for one test, and records what the
// command asked for.
func stubAccountLogin(t *testing.T, resp daemon.AccountLoginResponse, err error) *daemon.AccountLoginRequest {
	t.Helper()
	var seen daemon.AccountLoginRequest
	prev := accountLoginViaDaemon
	accountLoginViaDaemon = func(req daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
		seen = req
		return resp, err
	}
	t.Cleanup(func() { accountLoginViaDaemon = prev })
	return &seen
}

// stubAccountLoginAttach replaces the terminal handover, and reports whether it
// happened. A test must never actually run `tmux attach-session` — it would take
// the test runner's terminal, or fail for reasons that have nothing to do with
// the branch under test.
func stubAccountLoginAttach(t *testing.T, err error) *bool {
	t.Helper()
	attached := false
	prevAttach := attachAccountLogin
	attachAccountLogin = func(string, string) error {
		attached = true
		return err
	}
	prevTTY := accountsLoginStdinIsTTY
	accountsLoginStdinIsTTY = func() bool { return true }
	// Default the pane to GONE, which is what "the flow ended" looks like. The
	// detach case sets it back to alive for itself.
	prevAlive := accountLoginPaneAlive
	accountLoginPaneAlive = func(string, string) bool { return false }
	t.Cleanup(func() {
		attachAccountLogin = prevAttach
		accountsLoginStdinIsTTY = prevTTY
		accountLoginPaneAlive = prevAlive
	})
	return &attached
}

// registerLoggedInAccount makes an account that already holds the agent's own
// credential artifact, which is what af reads to report a login succeeded.
func registerLoggedInAccount(t *testing.T, home, agent, name, artifact string) {
	t.Helper()
	dir, err := agentaccount.Register(home, agent, name)
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	path := filepath.Join(dir, artifact)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("prepare artifact directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

// A malformed invocation is the failure an automation caller is most likely to
// hit, and under --json it must be the envelope rather than cobra's human
// `Error:` plus usage — the contract `add` and `list` already keep (#3057).
func TestAccountsLoginArgumentCountIsEnveloped(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--json"},
		{"login", "--json", "codex"},
		{"login", "--json", "codex", "work", "extra"},
	} {
		stdout, stderr, err := runAccounts(t, args...)
		if err == nil {
			t.Fatalf("%v succeeded", args)
		}
		env := requireEnvelope(t, stderr, "stderr")
		if env.Error == nil {
			t.Fatalf("%v produced no envelope error: %q", args, stderr)
		}
		if stdout != "" {
			t.Fatalf("%v wrote to stdout: %q", args, stdout)
		}
	}
}

// The account NAME is validated before the daemon is asked to do anything, so a
// traversal or an unusable spelling reads as what it is instead of arriving
// wrapped in an RPC failure.
func TestAccountsLoginRefusesABadNameWithoutCallingTheDaemon(t *testing.T) {
	called := false
	prev := accountLoginViaDaemon
	accountLoginViaDaemon = func(daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
		called = true
		return daemon.AccountLoginResponse{}, nil
	}
	t.Cleanup(func() { accountLoginViaDaemon = prev })

	if _, _, err := runAccounts(t, "login", "codex", "../../etc"); err == nil {
		t.Fatal("a traversal account name was accepted")
	}
	if called {
		t.Fatal("the daemon was asked to log in to an invalid account name")
	}
}

// --daemon-url promises a remote daemon. The credential directory and the login
// pane are both on THAT host while the terminal is here, so af refuses rather
// than running a login somewhere the operator is not sitting — the same seam
// `add` and `list` refuse at.
func TestAccountsLoginRefusesARemoteDaemon(t *testing.T) {
	stubAccountLogin(t, daemon.AccountLoginResponse{}, nil)
	withAccountsRemoteDaemon(t, "http://example.invalid:8080")
	_, stderr, err := runAccounts(t, "login", "codex", "work")
	if err == nil {
		t.Fatal("login against a remote daemon was accepted")
	}
	if !strings.Contains(err.Error(), "remote daemon") {
		t.Fatalf("refusal does not name the remote target: %v (stderr %q)", err, stderr)
	}
}

// Without a terminal to hand over — a script, a hook, a CI step — af prints the
// exact attach command instead of running a full-screen tmux client into a pipe.
// The socket is pinned, because the daemon and this process can resolve
// different TMUX_TMPDIRs (#2019).
func TestAccountsLoginWithoutATerminalPrintsHowToAttach(t *testing.T) {
	seen := stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Dir: "/home/u/.agent-factory/accounts/codex/work",
		Program: "codex login", SessionName: "af_af-login-codex-work", SocketPath: "/tmp/tmux-1000/default",
	}, nil)
	attached := stubAccountLoginAttach(t, nil)
	// A terminal IS present; --no-attach is what declines it here, so this also
	// covers the flag rather than only the headless case.
	stdout, stderr, err := runAccounts(t, "login", "--no-attach", "codex", "work")
	if err != nil {
		t.Fatalf("login --no-attach failed: %v (stderr %q)", err, stderr)
	}
	if *attached {
		t.Fatal("--no-attach took the terminal anyway")
	}
	if seen.Agent != "codex" || seen.Name != "work" {
		t.Fatalf("daemon asked for %+v", *seen)
	}
	// Shell-quoted, because the line is meant to be pasted: an agent-factory home
	// or socket dir with a space in it must still produce a command that runs.
	want := "tmux -S '/tmp/tmux-1000/default' attach-session -t 'af_af-login-codex-work'"
	if strings.TrimSpace(stdout) != want {
		t.Fatalf("stdout = %q, want the bare attach command %q", stdout, want)
	}
	if !strings.Contains(stderr, "codex login") {
		t.Fatalf("stderr does not say what af is running:\n%s", stderr)
	}
}

// With no socket resolved, the attach falls back to the default socket rather
// than emitting `-S ` with nothing after it.
func TestAccountsLoginAttachCommandFallsBackToTheDefaultSocket(t *testing.T) {
	if got := accountLoginAttachCommand("af_af-login-codex-work", ""); got != "tmux attach-session -t 'af_af-login-codex-work'" {
		t.Fatalf("attach command with no socket = %q", got)
	}
}

// The whole point of the verb: with a terminal, af hands it to the pane, and
// afterwards reports the outcome from the agent's own artifact.
func TestAccountsLoginAttachesAndThenReportsTheAccountLoggedIn(t *testing.T) {
	home := t.TempDir()
	registerLoggedInAccount(t, home, "codex", "work", "auth.json")
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work",
	}, nil)
	attached := stubAccountLoginAttach(t, nil)

	_, stderr, err := runAccountsInHome(t, home, "login", "codex", "work")
	if err != nil {
		t.Fatalf("login failed: %v (stderr %q)", err, stderr)
	}
	if !*attached {
		t.Fatal("af did not hand the terminal to the login pane")
	}
	if !strings.Contains(stderr, "is logged in") {
		t.Fatalf("af did not report the account logged in:\n%s", stderr)
	}
}

// #3384's verification requirement, at the surface an operator sees: a flow that
// ends leaving the account empty is a FAILURE. The alternative is a registered
// account that looks fine and fails much later, at session start, naming none of
// this.
func TestAccountsLoginReportsAnEmptyAccountAsFailure(t *testing.T) {
	home := t.TempDir()
	if _, err := agentaccount.Register(home, "codex", "work"); err != nil {
		t.Fatalf("register account: %v", err)
	}
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work",
	}, nil)
	stubAccountLoginAttach(t, nil)

	_, stderr, err := runAccountsInHome(t, home, "login", "codex", "work")
	if err == nil {
		t.Fatal("a login that left the account empty was reported as success")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("failure does not say the account is not logged in: %v (stderr %q)", err, stderr)
	}
}

// A flow that ended before af could hand over the terminal has no pane, so af
// must report the outcome rather than attach to a session name that is empty.
func TestAccountsLoginReportsAFinishedFlowWithoutAttaching(t *testing.T) {
	home := t.TempDir()
	registerLoggedInAccount(t, home, "codex", "work", "auth.json")
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		Finished: true, LoggedIn: true,
		Notices: []string{"codex's login command ended before af could hand over the terminal"},
	}, nil)
	attached := stubAccountLoginAttach(t, nil)

	_, stderr, err := runAccountsInHome(t, home, "login", "codex", "work")
	if err != nil {
		t.Fatalf("login failed: %v (stderr %q)", err, stderr)
	}
	if *attached {
		t.Fatal("af attached to a flow that had already finished")
	}
	if !strings.Contains(stderr, "ended before af could hand over the terminal") {
		t.Fatalf("the daemon's notice did not reach the operator:\n%s", stderr)
	}
	if !strings.Contains(stderr, "is logged in") {
		t.Fatalf("af did not report the outcome:\n%s", stderr)
	}
}

// A failed attach is not the end of the login — the pane is still running — so
// the error has to carry the way back to it.
func TestAccountsLoginAttachFailureNamesTheWayBack(t *testing.T) {
	home := t.TempDir()
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)
	stubAccountLoginAttach(t, errors.New("exit status 1"))

	_, _, err := runAccountsInHome(t, home, "login", "codex", "work")
	if err == nil {
		t.Fatal("a failed attach was reported as success")
	}
	if !strings.Contains(err.Error(), "tmux -S '/tmp/s/default' attach-session -t 'af_af-login-codex-work'") {
		t.Fatalf("attach failure does not say how to reach the pane: %v", err)
	}
}

// --json is for a caller that is not sitting at a terminal, so it reports the
// pane rather than taking the terminal to it.
func TestAccountsLoginJSONReportsThePaneAndNeverAttaches(t *testing.T) {
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Dir: "/h/accounts/codex/work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)
	attached := stubAccountLoginAttach(t, nil)

	stdout, _, err := runAccounts(t, "login", "--json", "codex", "work")
	if err != nil {
		t.Fatalf("login --json failed: %v", err)
	}
	if *attached {
		t.Fatal("--json took the terminal")
	}
	env := requireEnvelope(t, stdout, "stdout")
	if env.Error != nil {
		t.Fatalf("--json reported an error: %+v", env.Error)
	}
	for _, want := range []string{`"session_name"`, `"socket_path"`, `"logged_in"`, `"program"`} {
		if !strings.Contains(string(env.Data), want) {
			t.Fatalf("envelope omits %s: %s", want, env.Data)
		}
	}
}

// A daemon-side refusal — an agent with no verified login command, the codex
// keyring collapse, a missing binary — reaches the operator as itself, not as a
// generic failure.
func TestAccountsLoginSurfacesTheDaemonsRefusal(t *testing.T) {
	stubAccountLogin(t, daemon.AccountLoginResponse{},
		errors.New("agent does not support multiple accounts: af cannot log in to \"amp\""))
	_, _, err := runAccounts(t, "login", "amp", "work")
	if err == nil {
		t.Fatal("a refused login was reported as success")
	}
	if !strings.Contains(err.Error(), "amp") {
		t.Fatalf("refusal lost the agent it was about: %v", err)
	}
}

// Detaching from a login to go finish the browser step is an ordinary mid-flow
// pause, not a failed login. Asking only the account cannot tell it from a flow
// that ended, so af asks the pane whether it is still running — and says how to
// get back to it rather than reporting an error.
func TestAccountsLoginDetachIsNotAFailure(t *testing.T) {
	home := t.TempDir()
	if _, err := agentaccount.Register(home, "codex", "work"); err != nil {
		t.Fatalf("register account: %v", err)
	}
	stubAccountLogin(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)
	stubAccountLoginAttach(t, nil)
	prevAlive := accountLoginPaneAlive
	accountLoginPaneAlive = func(string, string) bool { return true }
	t.Cleanup(func() { accountLoginPaneAlive = prevAlive })

	_, stderr, err := runAccountsInHome(t, home, "login", "codex", "work")
	if err != nil {
		t.Fatalf("detaching from a running login was reported as a failure: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "still running") {
		t.Fatalf("af did not say the login is still open:\n%s", stderr)
	}
	if !strings.Contains(stderr, "af accounts login codex work") {
		t.Fatalf("af did not say how to rejoin the flow:\n%s", stderr)
	}
}
