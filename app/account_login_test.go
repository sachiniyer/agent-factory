package app

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
)

// The app-side wiring for the config overlay's Accounts section (#3385).
//
// These drive the real handlers with the daemon calls and the terminal handover
// stubbed, the same shape as the config-agent tests beside them. What is worth
// pinning is the division of labour: the pane asks, the app performs, and the two
// verbs behave DIFFERENTLY on purpose — a register keeps the overlay open, a
// login gives the terminal away.

// stubAccountSeams installs the three daemon calls and returns what was asked
// for, so a test can assert the app called the daemon rather than that it merely
// did not crash.
type accountSeamCalls struct {
	list     int
	register []daemon.RegisterAccountRequest
	login    []daemon.AccountLoginRequest
}

func stubAccountSeams(t *testing.T, resp daemon.AccountLoginResponse, loginErr error) *accountSeamCalls {
	t.Helper()
	calls := &accountSeamCalls{}
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			calls.list++
			return daemon.ListAccountsResponse{
				Entries: []daemon.AccountEntry{
					{Agent: "codex", Name: "work", Dir: "/d/codex/work", LoggedIn: true},
				},
				Agents: []string{"claude", "codex", "gemini"},
			}, nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			calls.register = append(calls.register, req)
			return daemon.RegisterAccountResponse{
				Entry:   daemon.AccountEntry{Agent: req.Agent, Name: req.Name, Dir: "/d/" + req.Agent + "/" + req.Name},
				Notices: []string{"CODEX_HOME relocates codex's whole home."},
			}, nil
		},
		func(req daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
			calls.login = append(calls.login, req)
			return resp, loginErr
		},
	))
	return calls
}

// sizeConfigPane gives the pane the geometry the app always gives it before
// anything is rendered — layoutPaneOverlays() runs inside showConfigEditor, so a
// zero-sized pane is a state production cannot reach.
//
// It is load-bearing for every assertion below that reads String(), and not
// incidental setup: at width 0 the pane clamps its wrap to 20 columns, so
// "Accounts could not be read: …" renders as "Accounts could not / be read: …"
// and a Contains on the phrase fails against a section that reported it
// perfectly well. Measured, not guessed — the same text at 100x40 comes back
// verbatim, which is what ui/config_pane_accounts_test.go asserts at the layer
// that owns the rendering.
func sizeConfigPane(h *home) {
	h.configPane.SetSize(100, 40)
}

// stubAccountLoginAttach replaces the terminal handover and reports whether it
// happened. A test must never run a real `tmux attach-session`.
func stubAccountLoginAttach(t *testing.T) *int {
	t.Helper()
	attached := 0
	prev := execAccountLoginAttach
	execAccountLoginAttach = func(string, string) *exec.Cmd {
		attached++
		// `true` exits 0 immediately, so tea.ExecProcess has something real to run
		// that cannot take a terminal or outlive the test.
		return exec.Command("true")
	}
	t.Cleanup(func() { execAccountLoginAttach = prev })
	return &attached
}

// A login request from the pane reaches the daemon — off the event loop, like
// every other spawn, because it registers a directory and starts a process.
func TestAccountLoginRequestReachesTheDaemonAsynchronously(t *testing.T) {
	h := newTestHome(t)
	calls := stubAccountSeams(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)

	cmd := h.handleAccountRequest(ui.AccountRequest{
		Kind: ui.AccountRequestLogin, Agent: "codex", Name: "work",
	})
	require.NotNil(t, cmd, "a login request must produce a command")
	assert.Empty(t, calls.login, "the daemon call must not run inline on the UI thread")

	msg := cmd()
	require.Len(t, calls.login, 1)
	assert.Equal(t, daemon.AccountLoginRequest{Agent: "codex", Name: "work"}, calls.login[0])

	started, ok := msg.(accountLoginStartedMsg)
	require.True(t, ok, "the command must report back as accountLoginStartedMsg, got %T", msg)
	assert.Equal(t, "af_af-login-codex-work", started.sessionName)
	assert.Equal(t, "/tmp/s/default", started.socketPath)
}

// A started login hands the terminal to its pane.
func TestAccountLoginStartedHandsOverTheTerminal(t *testing.T) {
	h := newTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	attached := stubAccountLoginAttach(t)

	_, cmd := h.handleAccountLoginStarted(accountLoginStartedMsg{
		agent: "codex", name: "work",
		sessionName: "af_af-login-codex-work", socketPath: "/tmp/s/default",
	})
	require.NotNil(t, cmd, "a started login must return the terminal handover")
	// The child is built by enterAccountLogin and handed to tea.ExecProcess, which
	// is what runs it — so the builder has already been called by the time the
	// command exists, and running the command here only produces bubbletea's exec
	// message. Asserting on the builder is asserting on the handover.
	assert.Equal(t, 1, *attached, "the login pane was not attached to")
}

// A flow that ended before af could hand over the terminal has no pane. Attaching
// to an empty session name would hand the terminal to `tmux attach-session -t ""`.
func TestAccountLoginFinishedNeverAttaches(t *testing.T) {
	h := newTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	attached := stubAccountLoginAttach(t)

	model, _ := h.handleAccountLoginStarted(accountLoginStartedMsg{
		agent: "codex", name: "work", finished: true, loggedIn: true,
	})
	require.NotNil(t, model)
	assert.Equal(t, 0, *attached, "af attached to a flow that had already finished")
}

// The copy for a finished flow follows the ACCOUNT, not the launch: a flow that
// left a credential is a success, one that did not is a failure that names the
// state it left behind.
func TestAccountLoginFinishedCopyFollowsTheAccount(t *testing.T) {
	ok := accountLoginFinishedStatus(accountLoginStartedMsg{
		agent: "codex", name: "work", loggedIn: true,
		notices: []string{"CODEX_HOME relocates codex's whole home."},
	})
	assert.Contains(t, ok, "is logged in")
	assert.Contains(t, ok, "relocates codex's whole home")

	bad := accountLoginFinishedStatus(accountLoginStartedMsg{agent: "codex", name: "work"})
	assert.Contains(t, bad, "registered but not logged in")
	assert.Contains(t, bad, "af accounts login codex work",
		"a failed login must name the way to retry it on the daemon host")
}

// A register is performed inline and keeps the overlay open: it is a step on the
// way to logging in, not a reason to close the surface the user is working in.
func TestAccountRegisterRefreshesTheSectionInPlace(t *testing.T) {
	h := newTestHome(t)
	calls := stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	cmd := h.handleAccountRequest(ui.AccountRequest{
		Kind: ui.AccountRequestRegister, Agent: "codex", Name: "fresh",
	})
	assert.Nil(t, cmd, "a register needs no command: it is one mkdir and a stat, not a process")
	require.Len(t, calls.register, 1)
	assert.Equal(t, daemon.RegisterAccountRequest{Agent: "codex", Name: "fresh"}, calls.register[0])
	assert.GreaterOrEqual(t, calls.list, 1, "the section must be re-read after a register")
}

// The daemon's refusal is what the operator sees. It holds the name rule, the
// case-collision rule and the roster, and each of those messages already says
// what to do about it.
func TestAccountRegisterSurfacesTheDaemonsRefusal(t *testing.T) {
	h := newTestHome(t)
	refusal := errors.New("account name \"Work\" collides with existing account \"work\" for codex")
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return daemon.ListAccountsResponse{}, nil
		},
		func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return daemon.RegisterAccountResponse{}, refusal
		},
		func(daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
			return daemon.AccountLoginResponse{}, nil
		},
	))

	sizeConfigPane(h)
	cmd := h.handleAccountRequest(ui.AccountRequest{
		Kind: ui.AccountRequestRegister, Agent: "codex", Name: "Work",
	})
	assert.Nil(t, cmd)
	assert.Contains(t, h.configPane.String(), "collides with existing account",
		"the daemon's refusal did not reach the pane the user is looking at")
}

// Nothing asked for means nothing done — the request is read on every keypress
// routed to the overlay, so the empty case is the common one.
func TestNoAccountRequestDoesNothing(t *testing.T) {
	h := newTestHome(t)
	calls := stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	assert.Nil(t, h.handleAccountRequest(ui.AccountRequest{}))
	assert.Empty(t, calls.login)
	assert.Empty(t, calls.register)
}

// A failed accounts read becomes the SECTION's message rather than blocking the
// overlay: the config editor is still useful, and an operator who came to change
// a key should not be turned away because one daemon call failed.
func TestAccountsReadFailureIsReportedInTheSection(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return daemon.ListAccountsResponse{}, errors.New("the daemon did not answer")
		},
		func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return daemon.RegisterAccountResponse{}, nil
		},
		func(daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
			return daemon.AccountLoginResponse{}, nil
		},
	))
	sizeConfigPane(h)
	h.loadAccountsIntoPane()
	view := h.configPane.String()
	assert.True(t, strings.Contains(view, "could not be read"),
		"a failed accounts read is not reported in the section:\n%s", view)
}

// The daemon's rows reach the pane, including the logged-in state and the roster
// an account can be registered for.
func TestAccountsReadPopulatesTheSection(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	h.loadAccountsIntoPane()
	require.True(t, h.configPane.AccountsLoaded())
	view := h.configPane.String()
	for _, want := range []string{"codex · work", "logged in", "register a gemini account"} {
		assert.Contains(t, view, want)
	}
}

// A login the daemon refused — an agent with no verified flow, the codex keyring
// collapse, a missing binary — is surfaced rather than swallowed, and nothing is
// attached to.
func TestAccountLoginRefusalIsSurfaced(t *testing.T) {
	h := newTestHome(t)
	attached := stubAccountLoginAttach(t)
	model, cmd := h.handleAccountLoginStarted(accountLoginStartedMsg{
		agent: "amp", name: "work",
		err: errors.New("agent does not support multiple accounts: af cannot log in to \"amp\""),
	})
	require.NotNil(t, model)
	assert.NotNil(t, cmd, "a refused login must raise the error rather than pass silently")
	assert.Equal(t, 0, *attached)
}

// The attach command is the shared bare-session builder: $TMUX scrubbed so it can
// nest, and the socket pinned so it resolves independently of either side's
// TMUX_TMPDIR (#2019).
func TestAccountLoginAttachPinsTheSocketAndScrubsTmux(t *testing.T) {
	cmd := execAccountLoginAttach("af_af-login-codex-work", "/tmp/tmux-1000/default")
	joined := strings.Join(cmd.Args, " ")
	assert.Contains(t, joined, "-S /tmp/tmux-1000/default")
	assert.Contains(t, joined, "attach-session -t af_af-login-codex-work")
	for _, kv := range cmd.Env {
		assert.False(t, strings.HasPrefix(kv, "TMUX="),
			"$TMUX survived into the attach; tmux refuses to nest and the takeover exits 1")
	}

	// With no socket resolved it falls back to the default socket rather than
	// emitting a bare `-S`.
	fallback := execAccountLoginAttach("af_af-login-codex-work", "")
	assert.NotContains(t, strings.Join(fallback.Args, " "), "-S")
}

// The done handler reports back and never reaps the pane: af cannot tell a detach
// from a completed flow at that seam, and killing it would throw away a
// half-finished OAuth step.
func TestAccountLoginDoneReportsWithoutReapingThePane(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	model, _ := h.handleAccountLoginDone(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.NotNil(t, model)
	assert.Contains(t, h.configPane.String(), "Back from the codex login",
		"the user is not told the takeover ended")
}

// A takeover that could not take the terminal is an error the user sees, not a
// silent return to the list.
func TestAccountLoginDoneSurfacesAFailedTakeover(t *testing.T) {
	h := newTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	_, cmd := h.handleAccountLoginDone(accountLoginDoneMsg{
		agent: "codex", name: "work", err: errors.New("exit status 1"),
	})
	assert.NotNil(t, cmd, "a failed takeover must raise an error")
}

// The overlay reads the accounts on EVERY open, so one registered from the CLI or
// logged in from the web since the TUI started shows as it is now.
func TestOpeningTheConfigOverlayReadsTheAccounts(t *testing.T) {
	h := newTestHome(t)
	calls := stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	before := calls.list
	model, _ := h.showConfigEditor()
	require.NotNil(t, model)
	require.True(t, h.configPane.HasFocus(),
		"precondition: the config overlay opened — a failed config read would make the assertion below vacuous")
	assert.Greater(t, calls.list, before, "opening the config overlay did not read the accounts")
}
