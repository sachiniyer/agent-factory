package app

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The implicit-submit honesty gates (#4893 review): a wire "" lets the daemon
// resolve default_program and default_accounts LIVE at create time, so every
// value the form displays must either be verified at submit or hold the press
// until a current answer lands. These tests pin each hold-and-review path.

// TestFailedLocalRevalidateRefusesImplicitSubmit pins the #4893 review finding
// on the re-read failure path: an implicit submit that cannot re-read
// default_program has no way to know what the wire's "" resolves to, so it
// must refuse retryably — the earlier draft fell through and sent a blind ""
// anyway.
func TestFailedLocalRevalidateRefusesImplicitSubmit(t *testing.T) {
	h := configFollowingHome(t)
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return "", errors.New("config read wedged")
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "an unverifiable display must not send a blind implicit program")
	require.Equal(t, stateNew, h.state, "the refusal leaves the form open")

	// Once the read recovers, the same Enter submits — the refusal is a hold,
	// not a dead end.
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return tmux.ProgramClaude, nil
	}
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
}

// TestMovedDefaultUpdatesTheFieldBeforePreflightRuns pins the ordering the
// #4893 review asked for: the live re-read must run BEFORE preflight. A
// default that moved to a program preflight rejects must still land on the
// field first — otherwise the refusal names a program the form never showed.
func TestMovedDefaultUpdatesTheFieldBeforePreflightRuns(t *testing.T) {
	h := configFollowingHome(t)
	prevRequired := localPrereqsRequired
	localPrereqsRequired = func(session.InstanceOptions, string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { localPrereqsRequired = prevRequired })
	t.Cleanup(SetLocalSessionPreflightForTest(func(_ *config.Config, program string) error {
		if program == tmux.ProgramCodex {
			return errors.New("codex is not installed")
		}
		return nil
	}))
	// The real config carries the moved default — the re-read and preflight
	// resolve the same value, so the field the refusal names is the field the
	// form showed.
	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramCodex)
	require.NoError(t, err)
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram, "seeded with the old default")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the re-read runs before preflight — the field now shows the program any refusal will name")
	assert.Empty(t, got.Title, "the moved default holds the submit for review")

	// The review Enter now reaches preflight — which refuses the program the
	// form has been showing since the first press.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "preflight refuses the unrunnable program the field displayed")
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram)
}

// TestMovedLocalDefaultClearsAndRefetchesTheAccount pins the #4893 review
// pairing: a local default that moves agents must drop the old agent's
// account preselect and re-ask for the new one — leaving "work" on the field
// while the display says codex advertises a pair the wire cannot honor.
func TestMovedLocalDefaultClearsAndRefetchesTheAccount(t *testing.T) {
	h := configFollowingHome(t)
	defaults := twoAgentsWithAccounts()
	defaults.Defaults = map[string]string{"claude": "work", "codex": "ci"}
	calls, _ := stubAccounts(t, defaults, nil)
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return tmux.ProgramCodex, nil
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	h.pendingAccount = "work" // the claude preselect the form-open fetch applied
	require.False(t, h.pendingAccountChosen)

	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram, "the field refreshes to the moved default")
	assert.Empty(t, h.pendingAccount, "the old agent's preselect cannot pair with the new program")
	assert.False(t, h.pendingAccountChosen)
	assert.Equal(t, 1, *calls, "the correction re-asks for the new program's default")

	for _, msg := range msgs {
		if am, ok := msg.(accountDefaultMsg); ok {
			_, _ = h.handleAccountDefault(am)
		}
	}
	assert.Equal(t, "ci", h.pendingAccount, "the refetch preselects the LIVE program's default")
}

// TestMovedDefaultHoldsUntilTheRefetchAnswers pins the #4893 review gap on the
// correction's async half: a moved default clears the account field and
// refires fetchAccountDefault, but an Enter landing BEFORE that reply must not
// proceed on the empty field — the answer may name a default the form has to
// show, and "" would resolve it silently anyway.
func TestMovedDefaultHoldsUntilTheRefetchAnswers(t *testing.T) {
	h := configFollowingHome(t)
	// The account lookup never answers — the refetch stays in flight.
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		select {}
	}))
	prev := resolveConfiguredProgramContext
	resolveConfiguredProgramContext = func(context.Context, string) (string, error) {
		return tmux.ProgramCodex, nil
	}
	t.Cleanup(func() { resolveConfiguredProgramContext = prev })
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	h.pendingAccount = "work" // the claude preselect

	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram, "the field refreshed to the moved default")
	require.Empty(t, h.pendingAccount, "the old agent's preselect was cleared")
	require.True(t, h.accountLookupPending, "the correction's refetch is in flight")
	_ = msgs

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title,
		"an empty field with a lookup still out is unverified — the submit must hold")

	// The reply lands: it names the new agent's default and discharges both
	// flags — the display the user reviewed is what the next Enter sends.
	_, _ = h.handleAccountDefault(accountDefaultMsg{
		naming:  h.namingInstance,
		agent:   "codex",
		program: tmux.ProgramCodex,
		seq:     h.accountDefaultSeq,
		resp:    daemon.ListAccountsResponse{Agents: []string{"codex"}, Defaults: map[string]string{"codex": "ci"}},
	})
	require.False(t, h.accountLookupPending)
	require.Equal(t, "ci", h.pendingAccount)

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title, "the reviewed display may now submit implicitly")
	assert.Empty(t, got.Account, "unchosen — the daemon resolves the live default")
}

// TestRemoteImplicitSubmitReverifiesTheDefault pins the #4893 review finding
// on the remote half of submit-time staleness: the form-open ListPrograms
// answer proves nothing about the daemon's LIVE default when Enter finally
// lands, so the remote path re-queries — and a moved answer corrects the
// display before anything submits.
func TestRemoteImplicitSubmitReverifiesTheDefault(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	default_ := tmux.ProgramClaude
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{Default: default_}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	h := configFollowingHome(t)
	got := recordStartRequest(t)

	_, cmd := h.startNewInstance()
	settleRemoteForm(t, h, cmd)
	require.Equal(t, tmux.ProgramClaude, h.pendingProgram)
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	// Another client moved the remote default after the form opened.
	default_ = tmux.ProgramCodex

	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "the first Enter re-queries the remote default and holds")
	require.True(t, h.remoteDefaultPending)

	deliver(t, h, msgs)
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the moved answer corrected the display before the submit")
	require.False(t, h.remoteDefaultPending)

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title, "the verified display submits")
	assert.Equal(t, tmux.ProgramCodex, got.Program,
		"the verified display pins the wire — a late remote move cannot substitute an unseen agent (#4893 review)")
}

// TestRemoteProgramPickerWaitsForTheCatalog pins the #4893 review finding on
// the picker's menu provenance: while the remote ListPrograms answer is still
// out, Tab must not offer the client-local enum — a client-only pick would be
// sent concretely to a daemon that may not support it.
func TestRemoteProgramPickerWaitsForTheCatalog(t *testing.T) {
	clearDaemonTarget(t)
	stubRemoteTarget(t)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		select {} // the remote answer never lands — the catalog stays unverified
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	h := configFollowingHome(t)

	_, cmd := h.startNewInstance()
	_ = drainCmd(t, cmd, 200*time.Millisecond)
	require.True(t, h.remoteDefaultPending, "the remote catalog is still out")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, stateNew, h.state,
		"the picker must not open on the local enum while the remote catalog is pending")
	assert.Nil(t, h.selectionOverlay)
}

// TestFailedAccountLookupStaysOwed is the error half of the in-flight gate
// (#4893 review): a FAILED account read is not an answer — it leaves the
// display unsettled, so the lookup stays owed. The next Enter re-asks, and
// only a good reply lets the implicit submit through; an explicit pick
// remains the way out while the service is down.
func TestFailedAccountLookupStaysOwed(t *testing.T) {
	h := configFollowingHome(t)
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Defaults: map[string]string{"claude": "ops"}}, nil
	}))
	got := recordStartRequest(t)

	startNaming(t, h, "demo")
	h.namingInstance.Program = ""
	require.NoError(t, h.namingInstance.SetTitle("demo"))

	// A lookup is out and its answer arrives FAILED: the display is still
	// unsettled — the read settled into a failure the next Enter must retry
	// rather than a pending flag nothing will ever answer.
	h.accountLookupPending = true
	m, _ := h.handleAccountDefault(accountDefaultMsg{
		naming:  h.namingInstance,
		seq:     h.accountDefaultSeq,
		agent:   tmux.ProgramClaude,
		program: tmux.ProgramClaude,
		err:     errors.New("daemon unreachable"),
	})
	h = m.(*home)
	require.False(t, h.accountLookupPending, "the dead fetch is not in flight")
	require.True(t, h.accountLookupFailed, "a failed read leaves the lookup owed")

	msgs := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Title, "an unverified display cannot submit implicitly")
	require.True(t, h.accountVerifyPending, "the held Enter refired the lookup")

	// The retry lands a real answer — the verified display then submits.
	deliver(t, h, msgs)
	require.Equal(t, "ops", h.pendingAccount, "the retry's answer applied")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "demo", got.Title)
	assert.Empty(t, got.Account,
		"the submit stays implicit — an empty wire resolves the same verified default the field shows")
}
