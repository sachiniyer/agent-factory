package app

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #3844: an account could be registered and logged into from the TUI config tab
// (#3385) and used from the CLI (`af sessions create --account`, #3051), and from
// nowhere else. The flow a user was walked through ended in a dead end.
//
// These tests drive the REAL key path (handleKeyPress plus the async registry
// message), because every interesting failure in this flow lives in the hops: a
// key swallowed on the way to handleStateNew, a picker opened over a form whose
// PROGRAM changed while the fetch was in flight, an account the daemon said
// cannot scope a session, and a daemon too old to have applied the field at all.

// stubAccounts points the registry seam at a fixture and returns both the number
// of calls and the agent each call asked for, so a test can prove the fetch
// happens on demand and prove what it asked the daemon.
func stubAccounts(t *testing.T, resp daemon.ListAccountsResponse, err error) (*int, *[]string) {
	t.Helper()
	calls := 0
	var asked []string
	t.Cleanup(SetAccountListerForTest(func(agent string) (daemon.ListAccountsResponse, error) {
		calls++
		asked = append(asked, agent)
		return resp, err
	}))
	return &calls, &asked
}

// twoAgentsWithAccounts is the ordinary answer from a host that has been used:
// two claude accounts (one of them never logged into) and one codex account,
// which is the shape constraint 1 is about — claude's "work" and codex's "work"
// are different identities in different registries.
func twoAgentsWithAccounts() daemon.ListAccountsResponse {
	return daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "personal", Dir: "/h/accounts/claude/personal", LoggedIn: true},
			{Agent: "claude", Name: "work", Dir: "/h/accounts/claude/work", LoggedIn: true},
			{Agent: "codex", Name: "work", Dir: "/h/accounts/codex/work", LoggedIn: true},
		},
		Agents: []string{"claude", "codex", "gemini"},
	}
}

// openAccountField presses ctrl+o and delivers the registry message the press
// produced, the way the event loop would. It returns the messages the press
// produced so a test can assert on a fetch that never happened.
func openAccountField(t *testing.T, h *home) []tea.Msg {
	t.Helper()
	produced := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlO})
	for _, msg := range produced {
		if registry, ok := msg.(accountRegistryMsg); ok {
			h.handleAccountRegistry(registry)
		}
	}
	return produced
}

// accountItems is what the open picker is showing, as the overlay renders it.
func accountItems(h *home) []string {
	items := make([]string, 0, len(h.accountPickerChoices))
	for _, choice := range h.accountPickerChoices {
		items = append(items, choice.item())
	}
	return items
}

// pickAccount moves the picker's cursor to the row whose label matches and
// submits it. Like pickBackend, the submit's cmd is discarded rather than
// drained: a refused choice answers with a transient notice whose cmd is the
// clear TIMER, and draining that would just wait out a deadline.
func pickAccount(t *testing.T, h *home, label string) {
	t.Helper()
	require.Equal(t, stateSelectAccount, h.state, "the account field must be open")
	idx := -1
	for i, choice := range h.accountPickerChoices {
		if choice.label == label {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "picker has no row labelled %q", label)
	h.selectionOverlay.SetSelectedIndex(idx)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
}

// selectProgram drives the naming form's program field (tab) to a program by
// name, which is the only way to change the agent a create runs as mid-form.
func selectProgram(t *testing.T, h *home, program string) {
	t.Helper()
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateSelectProgram, h.state, "tab during naming must open the program field")
	idx := -1
	for i, p := range tmux.SupportedPrograms {
		if p == program {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "%q is not a supported program", program)
	h.selectionOverlay.SetSelectedIndex(idx)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "submitting the program field returns to naming")
}

// submitNaming presses Enter and delivers the instanceStartedMsg the create
// produced back into the event loop, the way the runtime would. Returns false
// when the press produced no such message.
func submitNaming(t *testing.T, h *home) bool {
	t.Helper()
	for _, msg := range pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter}) {
		if started, ok := msg.(instanceStartedMsg); ok {
			_, _ = h.Update(started)
			return true
		}
	}
	return false
}

// TestAccountPickerListsOnlyTheProgramsAgent is constraint 1, and it is the one
// property that makes this a create-time picker rather than a second copy of the
// config tab's registry view. An account belongs to ONE agent: offering codex's
// "work" to a claude session offers a guaranteed failure, and the failure it
// invites is the identity kind — a create that lands somewhere the user did not
// choose.
//
// The fixture answers with every agent's accounts on purpose. The daemon is ASKED
// to narrow (the request carries no agent here, deliberately — see
// openAccountPicker), so the narrowing has to be a property of the picker rather
// than a promise about the response.
func TestAccountPickerListsOnlyTheProgramsAgent(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	calls, asked := stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "scoped-create")

	openAccountField(t, h)
	require.Equal(t, 1, *calls, "opening the field must ask the daemon for the registry")
	require.Equal(t, []string{""}, *asked,
		"the client asks for the WHOLE registry and narrows it itself, which is what keeps the "+
			"full agent roster available for the agents that have no accounts at all")
	require.Equal(t, stateSelectAccount, h.state, "ctrl+o during naming must open the account field")

	labels := make([]string, 0, len(h.accountPickerChoices))
	for _, choice := range h.accountPickerChoices {
		labels = append(labels, choice.label)
		assert.Equal(t, "claude", choice.agent,
			"every offered row must belong to the agent the form's program runs as")
	}
	require.Len(t, labels, 3, "the ambient row plus claude's two accounts: %v", labels)
	assert.Equal(t, []string{"personal", "work"}, labels[1:],
		"the picker must offer claude's accounts, in the daemon's order")

	// The codex account is the control: it is in the response, and it must not be
	// in the list. Same NAME as one of claude's, which is the collision the whole
	// constraint is about.
	for _, choice := range h.accountPickerChoices {
		assert.NotEqual(t, "codex", choice.agent, "a codex account must not be offered to a claude session")
	}
}

// TestAccountPickerFollowsAProgramChange is the other half of constraint 1: the
// list has to follow the program the form CURRENTLY has, not the one it had when
// the field was first opened — and a choice made under the old program cannot
// survive the change, because the same name means a different identity (or no
// identity at all) in the new agent's registry.
func TestAccountPickerFollowsAProgramChange(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "switch-agents")

	openAccountField(t, h)
	pickAccount(t, h, "work")
	require.Equal(t, "work", h.pendingAccount, "the claude account must attach")

	selectProgram(t, h, "codex")
	assert.Empty(t, h.pendingAccount,
		"changing the program must drop the account: the same name is a different identity in another registry")

	openAccountField(t, h)
	labels := make([]string, 0, len(h.accountPickerChoices))
	for _, choice := range h.accountPickerChoices {
		labels = append(labels, choice.label)
		assert.Equal(t, "codex", choice.agent, "the reopened list must follow the NEW program")
	}
	require.Len(t, labels, 2, "the ambient row plus codex's one account: %v", labels)

	// Leave the reopened field without choosing: the create must then go out on the
	// ambient identity, not on the claude account the program change dropped.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateNew, h.state, "esc closes the field back to the form")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Account,
		"an account dropped by a program change must not reach the daemon")
	assert.Equal(t, "codex", got.Program)
}

// TestNamingFormAccountReachesSessionStartRequest is the plumbing guard: an
// account picked in the naming form must arrive on the request the TUI hands the
// daemon, on the field `af sessions create --account` fills. Before this field
// existed the request had no Account member at all.
func TestNamingFormAccountReachesSessionStartRequest(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "on-my-other-login")

	openAccountField(t, h)
	pickAccount(t, h, "work")
	require.Equal(t, stateNew, h.state, "submitting the field returns to naming, not out of the create")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "work", got.Account, "the picked account must ride the sessionStartRequest to the daemon")
	// Guard the siblings: a change that populated Account by rebuilding the struct
	// must not drop another field on the way.
	assert.Equal(t, "on-my-other-login", got.Title)
	assert.Equal(t, "claude", got.Program)
}

// TestNamingFormWithoutAccountSendsNothing pins the unchanged default: a user who
// never opens the field submits exactly what they submitted before it existed, and
// the session runs on the ambient identity. This is the half that keeps "populate
// Account" from becoming "always send an account".
func TestNamingFormWithoutAccountSendsNothing(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "ambient-create")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, got.Account, "an untouched account field must send no account")
	assert.Equal(t, "ambient-create", got.Title)
}

// TestAccountPickerAmbientRowSendsNoAccount covers the sentinel: choosing the
// ambient row explicitly must be identical to never opening the field. A
// non-empty sentinel would eventually be transmitted as a literal account name —
// and an account name that does not exist is a refused create at best.
func TestAccountPickerAmbientRowSendsNoAccount(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "explicit-ambient")

	openAccountField(t, h)
	pickAccount(t, h, "work")
	require.Equal(t, "work", h.pendingAccount)

	openAccountField(t, h)
	pickAccount(t, h, h.accountPickerChoices[0].label)
	assert.Empty(t, h.pendingAccount, "the ambient row must clear a previously picked account")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Account, "the ambient row must send NO account")
}

// TestAccountPickerRefusesARegistrationOnlyAccount is constraint 2. An agent can
// be on the account roster — registrable, loggable-into — while af has not yet
// verified that the account boundary can prove how it launches. Such an account
// is LISTED with the reason, the way the backend picker lists an unavailable
// backend, and REFUSED on pick rather than sent to a create that would refuse it.
//
// The daemon is the authority on the state: AccountEntry.RegistrationOnly is
// computed by the build that owns the registry, which may be newer than this one.
// So the fixture sets the flag on claude — an agent THIS build considers launch
// proven — which is exactly the skew the flag exists to transmit, and it proves
// the picker trusts the wire value rather than re-deriving it locally.
func TestAccountPickerRefusesARegistrationOnlyAccount(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(300, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	got := recordStartRequest(t)
	stubAccounts(t, daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "unproven", Dir: "/h/accounts/claude/unproven",
				RegistrationOnly: true, LoggedIn: true},
		},
		Agents: []string{"claude"},
	}, nil)
	startNaming(t, h, "cannot-scope-this")

	openAccountField(t, h)
	require.Contains(t, accountItems(h)[1], "registration only",
		"a registration-only row must be marked in the list, before any keypress")
	pickAccount(t, h, "unproven")

	assert.Equal(t, stateNew, h.state, "a refused choice returns to the form, still retryable")
	require.NotNil(t, h.namingInstance, "a refused choice must not cancel the create")
	assert.Empty(t, h.pendingAccount, "a refused account must not be attached")
	assert.Contains(t, h.errBox.FullError(), "cannot scope a session",
		"the refusal must say what it refuses, and why")
	// A precondition that was CHECKED and failed is user feedback, not an
	// operation failure — the same severity split the backend picker keeps.
	assert.Contains(t, info.String(), "cannot scope a session",
		"a checked-and-failed precondition belongs at INFO")
	assert.Empty(t, errorLogs.String(),
		"a designed precondition refusal must not read as an operation failure")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Account, "a refused account must not reach the daemon")
}

// TestAccountPickerKeepsANotLoggedInAccountSelectable is constraint 3. `logged_in`
// is a stat of the agent's OWN credential file, so "not logged in" means "this
// directory has no credential in it yet" — not "this account is broken". Hiding
// such a row would hide the account the user registered thirty seconds ago and is
// on their way to logging into, which is the single most likely account to be
// picked right after the config tab created it.
func TestAccountPickerKeepsANotLoggedInAccountSelectable(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(300, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	got := recordStartRequest(t)
	stubAccounts(t, daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "just-registered", Dir: "/h/accounts/claude/just-registered", LoggedIn: false},
		},
		Agents: []string{"claude"},
	}, nil)
	startNaming(t, h, "about-to-log-in")

	openAccountField(t, h)
	require.Contains(t, accountItems(h)[1], "not logged in",
		"a not-logged-in row must say so in the list, before any keypress")
	pickAccount(t, h, "just-registered")

	assert.Equal(t, "just-registered", h.pendingAccount,
		"a not-logged-in account is a legitimate choice and must attach")
	assert.Contains(t, h.errBox.FullError(), "no claude credential yet",
		"picking it must say what is missing rather than let the state pass silently (#2020)")
	assert.NotEmpty(t, info.String(), "nothing failed, so the message belongs at INFO")
	assert.Empty(t, errorLogs.String(), "a registered account with no credential is not an operation failure")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "just-registered", got.Account, "the choice must still reach the daemon")
}

// TestAccountPickerOffersWhateverTheDaemonListed is the anti-drift property, and
// the reason this picker reads the daemon's registry rather than anything local:
// an account registered on the daemon host a second ago is offered here with no
// change to app/. The fixture names accounts no list in this process has ever
// heard of, which a local enum — or a name→label map — would drop or blank.
func TestAccountPickerOffersWhateverTheDaemonListed(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubAccounts(t, daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "moonbase-oncall", Dir: "/h/accounts/claude/moonbase-oncall", LoggedIn: true},
		},
		Agents: []string{"claude"},
	}, nil)
	startNaming(t, h, "future-account")

	openAccountField(t, h)
	pickAccount(t, h, "moonbase-oncall")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "moonbase-oncall", got.Account,
		"an account name this build has never seen must be offered and sent verbatim")
}

// TestAccountFieldSaysWhenTheAgentHasNoRegistry covers the ordinary case that is
// not a failure: aider supports no accounts, so there is nothing to pick. The
// roster naming the agents that DO comes from the daemon's own answer
// (ListAccountsResponse.Agents is always the full one), so the sentence stays
// true the day a fourth agent is verified — and the form stays open.
func TestAccountFieldSaysWhenTheAgentHasNoRegistry(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(300, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	startNaming(t, h, "no-registry-here")
	selectProgram(t, h, "aider")

	openAccountField(t, h)

	assert.Equal(t, stateNew, h.state, "there is nothing to open, and the create stays open")
	assert.Contains(t, h.errBox.FullError(), "aider")
	assert.Contains(t, h.errBox.FullError(), "claude, codex, gemini",
		"the agents that DO support accounts come from the daemon's roster")
	assert.NotEmpty(t, info.String(), "an agent without accounts is a fact, not a failure")
	assert.Empty(t, errorLogs.String())
}

// TestAccountFieldSaysWhenNothingIsRegistered is the empty-registry case: the
// agent supports accounts and none exists yet. A modal holding one row answers
// nothing, so the field points at where accounts are made instead.
func TestAccountFieldSaysWhenNothingIsRegistered(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(300, 1)
	stubAccounts(t, daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{},
		Agents:  []string{"claude", "codex"},
	}, nil)
	startNaming(t, h, "nothing-registered")

	openAccountField(t, h)

	assert.Equal(t, stateNew, h.state)
	assert.Contains(t, h.errBox.FullError(), "af accounts add claude",
		"the notice must name how to create the first account")
}

// startedWithAccount is the session a fake daemon reports back after a create.
func startedWithAccount(t *testing.T, title, account string) *session.Instance {
	t.Helper()
	inst := newLoadingInstance(t, title)
	inst.Account = account
	inst.SetStartedForTest(true)
	return inst
}

// TestNamingFormReportsAnAccountTheDaemonDidNotApply is constraint 5, the version
// skew this feature has to guard: a daemon predating account support DROPS the
// field silently, so the session runs on the ambient identity while the UI goes on
// reporting the account the user picked. That is the silent-wrong-identity outcome
// the whole feature exists to prevent, arriving through skew instead of through
// the environment — and a UI is where it would go unnoticed longest.
//
// The check reads what came BACK, not what was sent, because the daemon is the
// authority on what it stored. It is the same check `af sessions create` runs
// (api/sessions.go), and it names both accounts and the session, because one now
// exists that has to be removed rather than used.
func TestNamingFormReportsAnAccountTheDaemonDidNotApply(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(400, 1)
	_, errorLogs := captureHomeMessageLogs(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	// A daemon too old to know the field: it creates the session and reports it
	// back with no account on it.
	t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, _ sessionStartRequest) (*session.Instance, error) {
		return startedWithAccount(t, inst.Title, ""), nil
	}))
	startNaming(t, h, "skewed-daemon")

	openAccountField(t, h)
	pickAccount(t, h, "work")
	require.True(t, submitNaming(t, h), "the create must produce a completion message")

	full := h.errBox.FullError()
	assert.Contains(t, full, `"work"`, "the message must name the account that was asked for")
	assert.Contains(t, full, "ambient identity", "and the identity the session is actually running on")
	assert.Contains(t, full, "skewed-daemon", "and the session, which now exists and must be removed")
	assert.Contains(t, errorLogs.String(), "did not apply account",
		"a session running as the wrong identity is an operation failure, not user feedback")
}

// TestNamingFormAcceptsAnAppliedAccount is the control for the test above: a
// daemon that DID apply the account must produce no message at all. Without it the
// skew check could be a constant that fires on every account-scoped create, which
// would be a worse outcome than not having it — a warning nobody reads.
func TestNamingFormAcceptsAnAppliedAccount(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(400, 1)
	_, errorLogs := captureHomeMessageLogs(t)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, req sessionStartRequest) (*session.Instance, error) {
		return startedWithAccount(t, inst.Title, req.Account), nil
	}))
	startNaming(t, h, "modern-daemon")

	openAccountField(t, h)
	pickAccount(t, h, "work")
	require.True(t, submitNaming(t, h), "the create must produce a completion message")

	assert.Empty(t, h.errBox.FullError(), "an applied account must raise nothing")
	assert.NotContains(t, errorLogs.String(), "did not apply account")
}

// TestAccountFieldIgnoresARegistryForAnOtherProgram is the staleness guard, and it
// is the clause the backend field does not need. The fetch is async: changing the
// program while it is in flight changes which agent's accounts are the right ones,
// and opening the delivered list anyway would offer identities from a registry the
// create will not use.
func TestAccountFieldIgnoresARegistryForAnOtherProgram(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	naming := startNaming(t, h, "raced-program-change")

	// The answer to a fetch started while the form said "claude"…
	stale := accountRegistryMsg{naming: naming, agent: "claude", resp: twoAgentsWithAccounts()}
	// …landing after the user moved the form to codex.
	selectProgram(t, h, "codex")
	h.handleAccountRegistry(stale)

	assert.Equal(t, stateNew, h.state,
		"a registry for a program the form no longer has must not open a picker")
	assert.Nil(t, h.accountPickerChoices)
}

// TestAccountFieldSurvivesARegistryFailure keeps the field optional: a registry we
// could not read must not cost the user their half-typed create.
func TestAccountFieldSurvivesARegistryFailure(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(300, 1)
	_, errorLogs := captureHomeMessageLogs(t)
	got := recordStartRequest(t)
	stubAccounts(t, daemon.ListAccountsResponse{}, errors.New("daemon-http.sock: connection refused"))
	startNaming(t, h, "daemon-is-out")

	openAccountField(t, h)

	assert.Equal(t, stateNew, h.state, "the naming form stays open")
	require.NotNil(t, h.namingInstance)
	assert.Contains(t, h.errBox.FullError(), "cannot list accounts for claude",
		"the failure must lead with what it blocks, then the daemon's own words")
	assert.Contains(t, errorLogs.String(), "connection refused")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Account, "the create still goes through, on the ambient identity")
	assert.Equal(t, "daemon-is-out", got.Title)
}

// TestAccountSkewRefusalReadsWhatCameBack pins the comparison itself, arm by arm.
// It is the check's whole content: a false negative here is a session silently
// running as someone else, and a false positive is a warning on every
// account-scoped create, which trains the user to ignore the real one.
func TestAccountSkewRefusalReadsWhatCameBack(t *testing.T) {
	applied := &session.Instance{Title: "s", Account: "work"}

	assert.NoError(t, accountSkewRefusal(ambientAccount, &session.Instance{Title: "s"}),
		"a create that asked for no account has nothing to compare")
	assert.NoError(t, accountSkewRefusal(ambientAccount, applied),
		"a daemon that volunteered an account nobody asked for is not this check's business")
	assert.NoError(t, accountSkewRefusal("work", applied), "an applied account is the silent case")

	dropped := accountSkewRefusal("work", &session.Instance{Title: "s"})
	require.Error(t, dropped, "a daemon that dropped the field must be reported")
	assert.Contains(t, dropped.Error(), "ambient identity")

	other := accountSkewRefusal("work", &session.Instance{Title: "s", Account: "personal"})
	require.Error(t, other, "a daemon that applied a DIFFERENT account must be reported too")
	assert.Contains(t, other.Error(), `account "personal"`,
		"the message names what the session is actually running as, not just what was asked for")

	missing := accountSkewRefusal("work", nil)
	require.Error(t, missing, "no session to check against is not a pass")
	assert.Contains(t, missing.Error(), "af sessions list --json")
}

// TestAccountChoiceItemMarksBothStates pins the row text itself, which is the
// surface the two "listed, not hidden" constraints actually live on. Both marks
// can be true at once, and the join is the repo's ` · ` separator with the clause
// set off by an em dash.
func TestAccountChoiceItemMarksBothStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice accountChoice
		want   string
	}{
		{"plain", accountChoice{value: "work", label: "work", loggedIn: true}, "work"},
		{"not logged in", accountChoice{value: "work", label: "work"}, "work — not logged in"},
		{"registration only", accountChoice{value: "work", label: "work", loggedIn: true, registrationOnly: true},
			"work — registration only"},
		{"both", accountChoice{value: "work", label: "work", registrationOnly: true},
			"work — registration only · not logged in"},
		{"ambient", accountChoice{value: ambientAccount, label: "Ambient identity (the agent's own login)"},
			"Ambient identity (the agent's own login)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.choice.item())
			assert.False(t, strings.Contains(tc.choice.item(), "..."),
				"copy uses … rather than three dots")
		})
	}
}
