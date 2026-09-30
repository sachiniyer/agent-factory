package app

import (
	"errors"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namingFormWithConflict opens the naming form holding a title an existing
// session already has, and submits it: #4123's repro. The title is long enough
// that the conflict notice clips at 80 columns.
func namingFormWithConflict(t *testing.T) (*home, *session.Instance, string) {
	t.Helper()
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	const title = "todo-core-with-a-longer-title"

	existing, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	h.store.AddInstance(existing)
	naming, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	h.state = stateNew
	h.pendingProgram = "claude"
	h.namingInstance = naming

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "precondition: the form refuses the title and stays open")
	want := fmt.Sprintf("a session titled %q conflicts with existing session %q", title, title)
	require.Contains(t, h.errBox.FullError(), want, "precondition: the conflict notice is up")
	return h, naming, want
}

// TestNamingFormNoticeStaysWhileFormOpen: the reason the form refused its
// input stays on the bar for as long as the form holds that input, and expires
// normally once the form closes (#4123).
func TestNamingFormNoticeStaysWhileFormOpen(t *testing.T) {
	h, _, want := namingFormWithConflict(t)

	_, cmd := h.Update(hideErrMsg{noticeID: h.transientNoticeID})
	assert.Contains(t, h.errBox.FullError(), want, "the 3s timer must not expire a notice the open form raised")
	assert.NotNil(t, cmd, "the timer re-arms, so the notice expires once the form closes")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, h.namingInstance, "precondition: esc closed the form")
	_, _ = h.Update(hideErrMsg{noticeID: h.transientNoticeID})
	assert.Empty(t, h.errBox.FullError(), "with the form gone the notice expires as usual")
}

// TestNamingFormCtrlEOpensNoticeDetailsAndReturns: inside the form E is a
// title character, so ctrl+e opens the full notice, the clipped bar says so,
// and closing the details goes back to the form with its input intact (#4123).
func TestNamingFormCtrlEOpensNoticeDetailsAndReturns(t *testing.T) {
	h, naming, want := namingFormWithConflict(t)
	_ = h.View()
	assert.Contains(t, h.errBox.String(), "ctrl+e details",
		"the clipped notice advertises the key that works inside the form")
	assert.NotContains(t, h.errBox.String(), "E details")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlE})
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "conflicts with existing session")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, stateNew, h.state, "closing the details returns to the form")
	assert.Same(t, naming, h.namingInstance, "the form's input survives reading why it was refused")
	assert.Equal(t, "todo-core-with-a-longer-title", naming.Title)
	assert.Contains(t, h.errBox.FullError(), want)
}

// deliverFormKey presses a naming-form key and hands every message it produced
// back through Update, the way the event loop delivers a daemon answer.
func deliverFormKey(t *testing.T, h *home, msg tea.KeyMsg) {
	t.Helper()
	for _, produced := range pressFormKey(t, h, msg) {
		_, _ = h.Update(produced)
	}
}

// TestNamingFormNestedFieldNoticesStayWhileFormOpen: a nested field refuses a
// pick, or finds nothing to offer, by closing back to the form and raising the
// reason — from its own key handler or from the daemon answer it waited on.
// That reason is about the form still open, so it stays too (#4123).
func TestNamingFormNestedFieldNoticesStayWhileFormOpen(t *testing.T) {
	registrationOnly := daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{{Agent: "claude", Name: "unproven",
			Dir: "/h/accounts/claude/unproven", RegistrationOnly: true, LoggedIn: true}},
		Agents: []string{"claude"},
	}
	cases := []struct {
		name  string
		raise func(t *testing.T, h *home)
		want  string
	}{
		{"unavailable backend picked", func(t *testing.T, h *home) {
			stubBackends(t, twoUsableBackends(), nil)
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlR})
			pickBackend(t, h, "ssh")
		}, "ssh.host"},
		{"backend catalog failed", func(t *testing.T, h *home) {
			stubBackends(t, daemon.ListBackendsResponse{}, errors.New("daemon is not running"))
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlR})
		}, "cannot list backends"},
		{"registration-only account picked", func(t *testing.T, h *home) {
			stubAccounts(t, registrationOnly, nil)
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlO})
			pickAccount(t, h, "unproven")
		}, "cannot scope a session"},
		{"no account registered", func(t *testing.T, h *home) {
			stubAccounts(t, daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{}, Agents: []string{"claude"}}, nil)
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlO})
		}, "af accounts add claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(300, 1)
			startNaming(t, h, "nested-field-notice")

			tc.raise(t, h)
			require.Equal(t, stateNew, h.state, "precondition: back on the form, still open")
			require.Contains(t, h.errBox.FullError(), tc.want, "precondition: the notice is up")

			_, cmd := h.Update(hideErrMsg{noticeID: h.transientNoticeID})
			assert.Contains(t, h.errBox.FullError(), tc.want,
				"the 3s timer must not expire a notice a nested field raised onto the open form")
			assert.NotNil(t, cmd, "the timer re-arms, so the notice expires once the form closes")

			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
			require.Nil(t, h.namingInstance, "precondition: esc closed the form")
			_, _ = h.Update(hideErrMsg{noticeID: h.transientNoticeID})
			assert.Empty(t, h.errBox.FullError(), "with the form gone the notice expires as usual")
		})
	}
}

// TestNamingFormDetailsHoldsInFlightReplies: a daemon reply for the form that
// lands while ctrl+e has its details open is held and delivered on the way
// back, not dropped by the reply's own form-state guard. A dropped backend
// catalog promised by new_remote left backendPickerPending set, so the form
// answered "Loading backends…" to every submit until the user discarded it.
func TestNamingFormDetailsHoldsInFlightReplies(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *home)
		reply func(naming *session.Instance) tea.Msg
		check func(t *testing.T, h *home)
	}{
		{"backend catalog promised by new_remote", func(h *home) { h.backendPickerPending = true },
			func(naming *session.Instance) tea.Msg {
				return backendCatalogMsg{naming: naming, catalog: twoUsableBackends()}
			}, func(t *testing.T, h *home) {
				assert.Equal(t, stateSelectBackend, h.state, "the promised picker opens once the form is back")
				assert.False(t, h.backendPickerPending, "the form must not stay stuck loading")
			}},
		{"account registry", func(*home) {},
			func(naming *session.Instance) tea.Msg {
				return accountRegistryMsg{naming: naming, agent: "claude", resp: twoAgentsWithAccounts()}
			}, func(t *testing.T, h *home) {
				assert.Equal(t, stateSelectAccount, h.state, "the requested picker opens once the form is back")
			}},
		{"project default account", func(*home) {},
			func(naming *session.Instance) tea.Msg {
				return accountDefaultMsg{naming: naming, agent: "claude", resp: withDefaults(map[string]string{"claude": "work"})}
			}, func(t *testing.T, h *home) {
				assert.Equal(t, stateNew, h.state)
				assert.Equal(t, "work", h.pendingAccount, "the default must still be preselected")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(300, 1)
			naming := startNaming(t, h, "details-mid-fetch")
			_ = h.handleNotice(errors.New("an earlier notice to read"))
			tc.setup(h)

			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlE})
			require.Equal(t, stateHelp, h.state, "precondition: the details are open over the form")
			_, _ = h.Update(tc.reply(naming))
			require.Equal(t, stateHelp, h.state, "the reply must not act while the details are open")

			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
			require.Same(t, naming, h.namingInstance, "precondition: back on the same form")
			tc.check(t, h)
		})
	}
}

// TestNamingFormNestedFieldsAdvertiseNoDetailsKey: the pinned notice stays on
// the bar while a nested field is open, but no key opens the details there —
// the prompt types E and uses ctrl+e as end-of-line, the pickers answer
// neither — so the clipped notice must not advertise one.
func TestNamingFormNestedFieldsAdvertiseNoDetailsKey(t *testing.T) {
	cases := []struct {
		name  string
		open  func(t *testing.T, h *home)
		state state
	}{
		{"program", func(t *testing.T, h *home) {
			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
		}, stateSelectProgram},
		{"prompt", func(t *testing.T, h *home) {
			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
		}, statePromptInput},
		{"backend", func(t *testing.T, h *home) {
			stubBackends(t, twoUsableBackends(), nil)
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlR})
		}, stateSelectBackend},
		{"account", func(t *testing.T, h *home) {
			stubAccounts(t, twoAgentsWithAccounts(), nil)
			deliverFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlO})
		}, stateSelectAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := namingFormWithConflict(t)
			tc.open(t, h)
			require.Equal(t, tc.state, h.state, "precondition: the nested field is open")
			_ = h.View()
			bar := h.errBox.String()
			assert.Contains(t, bar, "conflicts with", "the pinned notice stays visible")
			assert.NotContains(t, bar, "details", "no details key works in a nested field")

			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
			require.Equal(t, stateNew, h.state, "precondition: esc returns to the form")
			_ = h.View()
			assert.Contains(t, h.errBox.String(), "ctrl+e details", "back on the form, ctrl+e is advertised again")
		})
	}
}

// TestNamingFormDetailsOverlayAdvertisesNoDetailsKey: with ctrl+e's details
// open over the form, the pinned notice is still on the bar behind them, and
// the overlay never dispatches E, so the bar must not advertise it.
func TestNamingFormDetailsOverlayAdvertisesNoDetailsKey(t *testing.T) {
	h, _, _ := namingFormWithConflict(t)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlE})
	require.Equal(t, stateHelp, h.state, "precondition: the details are open over the form")
	_ = h.View()
	bar := h.errBox.String()
	assert.Contains(t, bar, "conflicts with", "the pinned notice stays on the bar")
	assert.NotContains(t, bar, "details", "no details key works while the details are open")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateNew, h.state)
	_ = h.View()
	assert.Contains(t, h.errBox.String(), "ctrl+e details", "back on the form, ctrl+e is advertised again")
}
