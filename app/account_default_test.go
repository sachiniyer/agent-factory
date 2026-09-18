package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// #3386: a project can default to an account, and the naming form must SHOW that
// default rather than send nothing and let the daemon fill it in.
//
// The distinction is the whole issue. The session is identical either way — the
// daemon applies the same default — so what these pin is the difference between a
// form that says "Ambient identity" while the create prefers `work`, and one that
// says `work` before the user presses Enter. Since the pool router (#4404) the
// preselection is display-only, not a wire value: an untouched field submits ""
// and stays routable, which is also why the skew check does not compare it —
// routing may legitimately land on another account when the preferred one is
// walled, and an "asked for X, got Y" alarm on a correct reroute is exactly the
// noise a skew check must never raise (#4404 review).

// requireNamingFormOpened asserts that pressing `n`/`N` OPENED the naming form
// rather than refusing the keypress.
//
// It replaces a bare `require.Nil(t, cmd)` at every startNewInstance call site.
// That was the right proxy until opening the form began asking the daemon which
// account this project defaults to (#3386): the keypress now legitimately produces
// a command, so "produced no command" fails on a form that opened perfectly.
//
// The replacement is STRICTER than the assertion it removes, not looser. Every
// refusal in startNewInstance returns through handleNotice/handleError before the
// form is set up, and both land in the error box — so an empty box is direct
// evidence that nothing was reported, where a nil command was only evidence that
// nothing was scheduled.
func requireNamingFormOpened(t *testing.T, h *home, msgAndArgs ...any) {
	t.Helper()
	require.Empty(t, h.errBox.FullError(), msgAndArgs...)
}

// withDefaults is the registry answer from a host whose config scopes accounts
// per agent.
func withDefaults(defaults map[string]string) daemon.ListAccountsResponse {
	resp := twoAgentsWithAccounts()
	resp.Defaults = defaults
	return resp
}

// deliverAccountDefault runs the preselection fetch the way the event loop would:
// build the command, drain it, and hand the message back to the model.
func deliverAccountDefault(t *testing.T, h *home, naming *session.Instance, agent string) {
	t.Helper()
	cmd := h.fetchAccountDefault(naming, agent)
	require.NotNil(t, cmd, "the form must ask the daemon which account this project would apply")
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if def, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(def)
		}
	}
}

// TestNamingFormPreselectsTheProjectDefaultAccount is the issue's headline: the
// default is visible on the form — and stays ROUTABLE, because a preselection
// is a preview, not a pick (#4404 review).
func TestNamingFormPreselectsTheProjectDefaultAccount(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	var got sessionStartRequest
	t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, req sessionStartRequest) (*session.Instance, error) {
		got = req
		// A daemon that knows the field echoes it back on the created session; the
		// skew check compares the two only when an account was actually sent.
		return startedWithAccount(t, inst.Title, req.Account), nil
	}))
	calls, asked := stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)
	inst := startNaming(t, h, "defaulted")

	deliverAccountDefault(t, h, inst, "claude")
	assert.Equal(t, 1, *calls, "one round trip, the same ListAccounts the picker uses")
	assert.Equal(t, []string{""}, *asked, "the whole registry is fetched and narrowed here, as the picker does")
	require.Equal(t, "work", h.pendingAccount,
		"the project's configured account must be preselected, not applied invisibly by the daemon")
	assert.False(t, h.pendingAccountChosen, "a preselection is a preview, not a decision the user made")

	require.True(t, submitNaming(t, h))
	assert.Empty(t, got.Account,
		"an unchosen preselection must send NO account — serializing it would pin what was only displayed")
	assert.False(t, got.AccountAmbient, "and it is not an ambient pick either — it is routable")
}

// TestProjectDefaultNeverOverridesADeliberatePick is the race this feature is one
// keystroke away from losing. The fetch is asynchronous, so a default landing
// after the user has chosen would put the session on an identity they just chose
// against — the exact outcome the account field exists to prevent.
func TestProjectDefaultNeverOverridesADeliberatePick(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)
	inst := startNaming(t, h, "picked-first")

	openAccountField(t, h)
	pickAccount(t, h, "personal")
	require.Equal(t, "personal", h.pendingAccount)

	deliverAccountDefault(t, h, inst, "claude")
	assert.Equal(t, "personal", h.pendingAccount, "a late default must not replace the account the user chose")
}

// The ambient identity is the sharp case of the same rule: it IS the empty
// string, so "the user chose ambient" and "no answer yet" cannot be told apart by
// value — only by whether a choice was made.
func TestProjectDefaultNeverOverridesADeliberateAmbientPick(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)
	inst := startNaming(t, h, "ambient-on-purpose")

	openAccountField(t, h)
	pickAccount(t, h, "Use the ambient identity (no account)")
	require.Equal(t, ambientAccount, h.pendingAccount)
	require.True(t, h.pendingAccountAmbient, "the ambient row must record the ambient pick")

	deliverAccountDefault(t, h, inst, "claude")
	assert.Equal(t, ambientAccount, h.pendingAccount,
		"choosing the ambient identity is a decision, and a default must not quietly undo it")
}

// An account belongs to ONE agent. A default that landed after the program moved
// would attach another registry's identity to this session.
func TestProjectDefaultForAnotherAgentIsIgnored(t *testing.T) {
	h := newTestHome(t)
	stubAccounts(t, withDefaults(map[string]string{"codex": "work"}), nil)
	inst := startNaming(t, h, "wrong-agent")

	deliverAccountDefault(t, h, inst, "codex")
	assert.Equal(t, ambientAccount, h.pendingAccount,
		"the form runs claude; a codex default must never reach it, even though the answer names one")
}

// A default for a different naming flow is the same class of staleness.
func TestProjectDefaultForAnotherFormIsIgnored(t *testing.T) {
	h := newTestHome(t)
	stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)
	startNaming(t, h, "current")

	stale, err := session.NewInstance(session.InstanceOptions{Title: "stale", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	deliverAccountDefault(t, h, stale, "claude")
	assert.Equal(t, ambientAccount, h.pendingAccount, "an answer for a form that is gone must not fill in this one")
}

// A registry af could not read costs the preview, never the create: the daemon
// applies the same default regardless, so there is nothing to report.
func TestAProjectDefaultFetchFailureIsSilent(t *testing.T) {
	h := newTestHome(t)
	stubAccounts(t, daemon.ListAccountsResponse{}, assert.AnError)
	inst := startNaming(t, h, "no-registry")

	deliverAccountDefault(t, h, inst, "claude")
	assert.Equal(t, ambientAccount, h.pendingAccount)
	assert.Equal(t, stateNew, h.state, "the form stays open and usable")
}

// TestAccountPickerMarksTheProjectDefault is the visibility half: the row that
// the project chose says so, and only that agent's row does.
func TestAccountPickerMarksTheProjectDefault(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubAccounts(t, withDefaults(map[string]string{"claude": "work", "codex": "work"}), nil)
	startNaming(t, h, "labelled")

	openAccountField(t, h)
	items := accountItems(h)
	require.Len(t, items, 4, "the routable and ambient rows plus claude's two accounts: %v", items)
	assert.Equal(t, "personal", items[2], "an account this project did not choose is unmarked")
	assert.Equal(t, "work — project default", items[3],
		"the configured account must SAY it is the project default; a silent preselection is the complaint")
}

// A default naming an account the registry does not list is a real state — a
// project configured before the account existed, or one deleted since — and it is
// the state the user most needs to see, because the create refuses it.
func TestAccountPickerOffersAnUnregisteredProjectDefault(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubAccounts(t, withDefaults(map[string]string{"claude": "retired"}), nil)
	startNaming(t, h, "retired-default")

	openAccountField(t, h)
	items := accountItems(h)
	require.Len(t, items, 5, "the routable and ambient rows, claude's two accounts, and the configured-but-absent one: %v", items)
	assert.Equal(t, "retired — project default · not registered", items[4],
		"appended last, and labelled — hiding it would show the ambient identity while the config says otherwise")

	pickAccount(t, h, "retired")
	assert.Equal(t, "retired", h.pendingAccount, "it is selectable: the daemon is the authority on what it accepts")
}

// An agent with no credential-root variable can never be scoped to an account, so
// `default_accounts` cannot hold an entry for it and asking would be a round trip
// per `n` whose answer is known to be empty.
func TestNoDefaultIsFetchedForAnAgentThatCannotTakeAccounts(t *testing.T) {
	h := newTestHome(t)
	calls, _ := stubAccounts(t, withDefaults(map[string]string{"claude": "work"}), nil)
	inst := startNaming(t, h, "aider-session")

	assert.Nil(t, h.fetchAccountDefault(inst, "aider"), "there is nothing to preselect for aider")
	assert.Equal(t, 0, *calls, "and nothing to ask the daemon")
}
