package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// #1933: no surface but the CLI could choose a session's backend. The web half
// landed in #1968 (a picker over the daemon's ListBackends catalog); the TUI was
// the surface left out — `N` forces the hook backend and nothing else, so a docker
// or ssh session could not be started from the primary interface at all.
//
// These tests drive the REAL key path (handleKeyPress plus the async catalog
// message), because every interesting failure in this flow lives in the hops: a
// key swallowed on the way to handleStateNew, a picker opened over a form that
// closed while the fetch was in flight, a choice the daemon said would fail.

// stubBackends points the catalog seam at a fixture and returns how many times it
// was asked, so a test can prove the fetch happens on demand rather than never.
func stubBackends(t *testing.T, catalog daemon.ListBackendsResponse, err error) *int {
	t.Helper()
	calls := 0
	t.Cleanup(SetBackendListerForTest(func(string) (daemon.ListBackendsResponse, error) {
		calls++
		return catalog, err
	}))
	return &calls
}

// twoUsableBackends is the ordinary answer: local and docker usable, ssh not
// configured for this repo, and local as the resolved default.
func twoUsableBackends() daemon.ListBackendsResponse {
	return daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: config.BackendLocal, Label: "local", Status: daemon.BackendAvailable},
			{Name: config.BackendDocker, Label: "docker", Status: daemon.BackendAvailable},
			{Name: config.BackendSSH, Label: "ssh", Status: daemon.BackendUnavailable,
				Reason: "backend=ssh requires ssh.host to be set in this repo's .agent-factory/config.json"},
		},
		Default:       config.BackendLocal,
		DefaultStatus: daemon.BackendAvailable,
	}
}

// openBackendField presses ctrl+r and delivers the catalog message the press
// produced, the way the event loop would. It returns the messages the press
// produced so a test can assert on a fetch that never happened.
func openBackendField(t *testing.T, h *home) []tea.Msg {
	t.Helper()
	produced := pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlR})
	for _, msg := range produced {
		if catalog, ok := msg.(backendCatalogMsg); ok {
			h.handleBackendCatalog(catalog)
		}
	}
	return produced
}

// pickBackend moves the picker's cursor to the row whose label matches and
// submits it.
//
// The submit goes through handleKeyPress but its cmd is discarded rather than
// drained: a modal state takes no highlight re-emit hop, and a REFUSED choice
// answers with a transient notice whose cmd is the clear TIMER — draining that
// would just wait out a deadline.
func pickBackend(t *testing.T, h *home, label string) {
	t.Helper()
	require.Equal(t, stateSelectBackend, h.state, "the backend field must be open")
	idx := -1
	for i, choice := range h.backendPickerChoices {
		if choice.label == label {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "picker has no row labelled %q", label)
	h.selectionOverlay.SetSelectedIndex(idx)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
}

// pressExpectingNotice presses a naming-form key that answers with a transient
// notice. It replays the highlight re-emit hop like pressFormKey — the hop is
// where a swallowed key would hide — but never waits on the notice's own cmd.
func pressExpectingNotice(t *testing.T, h *home, msg tea.KeyMsg) {
	t.Helper()
	_, cmd := h.handleKeyPress(msg)
	for _, produced := range drainCmd(t, cmd, time.Second) {
		if km, ok := produced.(tea.KeyMsg); ok {
			_, _ = h.handleKeyPress(km)
		}
	}
}

// TestNamingFormBackendReachesSessionStartRequest is the #1933 regression guard
// for the TUI half: a backend picked in the naming form must arrive on the request
// the TUI hands the daemon. Before this field existed the request had no Backend
// member at all, so this asserted nothing could carry it.
func TestNamingFormBackendReachesSessionStartRequest(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	calls := stubBackends(t, twoUsableBackends(), nil)
	startNaming(t, h, "in-a-container")

	openBackendField(t, h)
	require.Equal(t, 1, *calls, "opening the field must ask the daemon for the catalog")
	pickBackend(t, h, "docker")
	require.Equal(t, stateNew, h.state, "submitting the field returns to naming, not out of the create")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, config.BackendDocker, got.Backend,
		"the picked backend must ride the sessionStartRequest to the daemon")
	// Guard the siblings: a change that populated Backend by rebuilding the struct
	// must not drop another field on the way.
	assert.Equal(t, "in-a-container", got.Title)
	assert.Equal(t, "claude", got.Program)
}

// TestNamingFormWithoutBackendSendsEmpty pins the unchanged default: a user who
// never opens the field submits exactly what they submitted before this field
// existed, and the repo's own config decides. This is the half that keeps
// "populate Backend" from becoming "always send a backend", which would freeze
// today's resolution into every request and ignore a later config change.
func TestNamingFormWithoutBackendSendsEmpty(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	stubBackends(t, twoUsableBackends(), nil)
	startNaming(t, h, "repo-default")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, got.Backend, "an untouched backend field must send no backend")
	assert.Equal(t, "repo-default", got.Title)
}

// TestBackendFieldRepoDefaultRowSendsNoBackend covers the sentinel: choosing the
// "repo default" row explicitly must be identical to never opening the field. A
// non-empty sentinel here would eventually be transmitted as a literal backend
// name and pin the create to whatever the repo resolved to today.
func TestBackendFieldRepoDefaultRowSendsNoBackend(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	catalog := twoUsableBackends()
	catalog.Default = config.BackendDocker
	stubBackends(t, catalog, nil)
	startNaming(t, h, "explicit-default")

	openBackendField(t, h)
	require.Contains(t, h.backendPickerChoices[0].label, "docker",
		"the repo-default row must name the backend it actually resolves to")
	pickBackend(t, h, h.backendPickerChoices[0].label)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, got.Backend,
		"the repo-default row must send NO backend, letting the repo config decide")
}

// TestBackendPickerRefusesAnUnusableChoice pins the promise a picker makes: every
// row it lets you keep is one the daemon said would work. The refusal carries the
// daemon's own reason, which is the same sentence a create would have failed
// with — so the picker cannot describe a precondition differently from the code
// that enforces it.
func TestBackendPickerRefusesAnUnusableChoice(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	got := recordStartRequest(t)
	stubBackends(t, twoUsableBackends(), nil)
	startNaming(t, h, "no-ssh-here")

	openBackendField(t, h)
	pickBackend(t, h, "ssh")

	assert.Equal(t, stateNew, h.state, "a refused choice returns to the form, still retryable")
	require.NotNil(t, h.namingInstance, "a refused choice must not cancel the create")
	assert.Empty(t, h.pendingBackend, "a refused choice must not be attached")
	assert.Contains(t, h.errBox.FullError(), "ssh.host",
		"the refusal must name what to fix, in the daemon's own words")
	// BackendUnavailable is the ONE designed refusal here: a precondition that was
	// checked and failed, which the user fixes by editing the repo config.
	assert.Contains(t, info.String(), "ssh.host",
		"a checked-and-failed precondition is user feedback, so it belongs at INFO")
	assert.Empty(t, errorLogs.String(),
		"a designed precondition refusal must not read as an operation failure")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, got.Backend, "a refused choice must not reach the daemon")
}

// TestBackendPickerRefusesAnUncheckableChoice is the tri-state half. "unknown"
// means the daemon COULD NOT CHECK (a repo config that would not parse), which is
// a different answer from yes and from no. Collapsing it into "available" would
// make the picker promise something nobody verified.
func TestBackendPickerRefusesAnUncheckableChoice(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	stubBackends(t, daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: config.BackendLocal, Label: "local", Status: daemon.BackendAvailable},
			{Name: config.BackendDocker, Label: "docker", Status: daemon.BackendUnknown,
				Reason: "cannot tell whether this repo can use backend=docker: its .agent-factory/config.json could not be read"},
		},
		Default:       config.BackendLocal,
		DefaultStatus: daemon.BackendAvailable,
	}, nil)
	startNaming(t, h, "unparseable-config")

	openBackendField(t, h)
	require.Contains(t, h.backendPickerChoices[2].item(), "cannot check",
		"an uncheckable row must be marked as such in the list, before any keypress")
	pickBackend(t, h, "docker")

	assert.Empty(t, h.pendingBackend, "an uncheckable backend must not be attached")
	assert.Contains(t, h.errBox.FullError(), "could not be read",
		"the notice must say what stopped the check, not that docker is unavailable")
	// ...and unlike an unavailable precondition, an uncheckable one is a real
	// failure: the in-repo config could not be read or parsed. Filing it as
	// ordinary user feedback would hide config I/O breakage from ERROR monitoring.
	assert.Contains(t, errorLogs.String(), "could not be read",
		"an evaluation that FAILED is an operation failure, not a designed refusal")
	assert.NotContains(t, info.String(), "could not be read",
		"an uncheckable backend must not be downgraded to a notice")
}

// TestBackendPickerReasonlessRefusalIsAnError covers the defensive fallback.
// BackendOption promises an actionable reason with every non-available status, so
// a reasonless one is a MALFORMED daemon response — the message even says the
// daemon failed to explain itself. That is a contract violation to investigate,
// not something the user can act on, so it must reach ERROR.
func TestBackendPickerReasonlessRefusalIsAnError(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	stubBackends(t, daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: config.BackendLocal, Label: "local", Status: daemon.BackendAvailable},
			// Non-available with no reason: the shape the contract forbids.
			{Name: config.BackendDocker, Label: "docker", Status: daemon.BackendUnavailable},
		},
		Default:       config.BackendLocal,
		DefaultStatus: daemon.BackendAvailable,
	}, nil)
	startNaming(t, h, "reasonless-refusal")

	openBackendField(t, h)
	pickBackend(t, h, "docker")

	assert.Empty(t, h.pendingBackend, "a reasonless refusal must not attach the backend")
	assert.Contains(t, h.errBox.FullError(), "gave no reason",
		"the keypress must still say something (#2020)")
	assert.Contains(t, errorLogs.String(), "gave no reason",
		"a malformed daemon response must stay visible to ERROR monitoring")
	assert.Empty(t, info.String(),
		"a broken response contract must not be filed as ordinary user feedback")
}

// TestBackendPickerUnrecognizedStatusIsAnError is the version-skew half. The
// picker deliberately renders whatever the daemon lists, which means a status
// this build does not know is reachable — a newer daemon, a status added
// server-side. Defaulting an unnameable status to a notice would silently mute
// whatever it turns out to mean, so the downgrade is gated POSITIVELY on
// BackendUnavailable: only a precondition we can name as checked-and-failed is
// user feedback.
func TestBackendPickerUnrecognizedStatusIsAnError(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	info, errorLogs := captureHomeMessageLogs(t)
	stubBackends(t, daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: config.BackendLocal, Label: "local", Status: daemon.BackendAvailable},
			// A wire value from a daemon newer than this build.
			{Name: config.BackendDocker, Label: "docker",
				Status: daemon.BackendAvailability("quarantined"),
				Reason: "docker is quarantined by a policy this client has never heard of"},
		},
		Default:       config.BackendLocal,
		DefaultStatus: daemon.BackendAvailable,
	}, nil)
	startNaming(t, h, "skewed-daemon")

	openBackendField(t, h)
	pickBackend(t, h, "docker")

	assert.Empty(t, h.pendingBackend, "an unrecognized status must not be treated as usable")
	assert.Contains(t, h.errBox.FullError(), "quarantined",
		"the daemon's reason is still shown verbatim")
	assert.Contains(t, errorLogs.String(), "quarantined",
		"a status this build cannot name must stay visible to ERROR monitoring")
	assert.Empty(t, info.String(),
		"only a checked-and-failed precondition may be downgraded to a notice")
}

// TestBackendPickerOffersWhateverTheDaemonListed is the anti-drift property, and
// the reason this picker reads the catalog instead of config.SupportedBackends: a
// backend added server-side must be offered here with no change to app/. The
// fixture names a backend no enum in this process knows, so a local list — or a
// name→label map — would drop it or render it blank.
func TestBackendPickerOffersWhateverTheDaemonListed(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	stubBackends(t, daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: "moonbase", Label: "Moonbase (experimental)", Status: daemon.BackendAvailable},
		},
		Default:       "moonbase",
		DefaultStatus: daemon.BackendAvailable,
	}, nil)
	startNaming(t, h, "future-backend")

	openBackendField(t, h)
	pickBackend(t, h, "Moonbase (experimental)")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "moonbase", got.Backend,
		"a backend the TUI has never heard of must still be selectable and sent verbatim")
}

// TestBackendPickerSkipsTheLocalAgentPreflight guards against re-creating #2592's
// shape in the TUI: local prerequisites (the agent binary on this box) do not apply
// to a session whose agent runs in a container or on another host. The naming
// placeholder cannot answer that — it is built when the form opens, before the
// choice — so the pending choice has to.
func TestBackendPickerSkipsTheLocalAgentPreflight(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	got := recordStartRequest(t)
	stubBackends(t, twoUsableBackends(), nil)
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error {
		return errors.New("claude is not installed on this machine")
	}))
	startNaming(t, h, "agentless-host")

	// Precondition: with no backend picked, the local preflight still gates.
	pressExpectingNotice(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "a local create must still fail the local preflight")
	require.Contains(t, h.errBox.FullError(), "not installed")

	openBackendField(t, h)
	pickBackend(t, h, "docker")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateDefault, h.state, "a docker create must not be gated on a local agent binary")
	assert.Equal(t, config.BackendDocker, got.Backend)
}

// TestStaleBackendCatalogNeverOpensAPicker is why the catalog arrives as a message
// rather than being fetched inline. The round trip is async, so by the time it
// lands the user may have submitted, cancelled, or begun naming a different
// session — and a modal opened over that edits a create that no longer exists.
func TestStaleBackendCatalogNeverOpensAPicker(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, h *home)
	}{
		{"the form was submitted", func(t *testing.T, h *home) {
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
		}},
		{"the form was cancelled", func(t *testing.T, h *home) {
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
		}},
		{"a different session is being named", func(t *testing.T, h *home) {
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
			startNaming(t, h, "someone-else")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(120, 1)
			recordStartRequest(t)
			stubBackends(t, twoUsableBackends(), nil)
			naming := startNaming(t, h, "abandoned")

			// The keypress that started the fetch, then the form ends before it lands.
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlR})
			tc.end(t, h)

			h.handleBackendCatalog(backendCatalogMsg{naming: naming, catalog: twoUsableBackends()})

			assert.NotEqual(t, stateSelectBackend, h.state, "a stale catalog must not open a modal")
			assert.Nil(t, h.selectionOverlay)
			assert.Nil(t, h.backendPickerChoices)
		})
	}
}

// TestBackendCatalogFailureKeepsTheFormOpen: the field is optional, so a catalog
// the daemon could not produce must cost the user a notice, not their half-typed
// create. The notice leads with what failed — the transient notice clips its TAIL
// at real terminal widths (#1973).
func TestBackendCatalogFailureKeepsTheFormOpen(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	stubBackends(t, daemon.ListBackendsResponse{}, errors.New("daemon is not running"))
	startNaming(t, h, "no-daemon")

	openBackendField(t, h)

	assert.Equal(t, stateNew, h.state, "a failed fetch must leave the naming form open")
	require.NotNil(t, h.namingInstance)
	full := h.errBox.FullError()
	assert.Contains(t, full, "cannot list backends")
	assert.Contains(t, full, "daemon is not running", "the daemon's own error must survive")
	assert.Less(t, strings.Index(full, "cannot list backends"), strings.Index(full, "daemon is not running"),
		"the cause must precede the detail: the notice clips its tail")
}

// TestBackendDoesNotLeakIntoNextCreate is the leak guard. pendingBackend is
// home-scoped state that outlives one create, so a submitted or cancelled create
// must not hand its backend to the next session the user makes — which would
// silently put a session somewhere the user never chose.
func TestBackendDoesNotLeakIntoNextCreate(t *testing.T) {
	cases := []struct {
		name string
		end  tea.KeyMsg
	}{
		{"submitted", tea.KeyMsg{Type: tea.KeyEnter}},
		{"cancelled with esc", tea.KeyMsg{Type: tea.KeyEsc}},
		{"cancelled with ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(120, 1)
			got := recordStartRequest(t)
			stubBackends(t, twoUsableBackends(), nil)

			startNaming(t, h, "first")
			openBackendField(t, h)
			pickBackend(t, h, "docker")
			pressFormKey(t, h, tc.end)
			require.Empty(t, h.pendingBackend, "leaving the naming form must clear the pending backend")

			*got = sessionStartRequest{}
			startNaming(t, h, "second")
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

			assert.Empty(t, got.Backend,
				"the previous create's backend must not ride the next session's request")
			assert.Equal(t, "second", got.Title)
		})
	}
}

// TestStartNewInstanceResetsTheBackendField covers the authoritative half of the
// leak guard: whatever route the previous create exited by, beginning a new one
// starts from the repo default.
func TestStartNewInstanceResetsTheBackendField(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	h.repoRoot = repoDir
	h.errBox.SetSize(120, 1)
	h.pendingBackend = config.BackendDocker

	model, cmd := h.startNewInstance(false)
	require.Same(t, h, model)
	require.Nil(t, cmd)
	require.Equal(t, stateNew, h.state)

	assert.Empty(t, h.pendingBackend, "beginning a create must start from the repo default")
}

// TestNamingMenuAdvertisesTheBackendField covers discoverability, which for a
// modal field is the whole game: the status bar is the only surface that can tell
// a user the field exists, and the only confirmation that the create is going
// somewhere other than the repo default before they press Enter.
func TestNamingMenuAdvertisesTheBackendField(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	h.menu.SetSize(200, 3)
	stubBackends(t, twoUsableBackends(), nil)
	startNaming(t, h, "hints")

	require.Contains(t, h.menu.String(), "backend",
		"the naming form must advertise its backend field")
	require.NotContains(t, h.menu.String(), "backend ✓", "precondition: repo default")

	openBackendField(t, h)
	pickBackend(t, h, "docker")

	assert.Contains(t, h.menu.String(), "backend ✓",
		"the hint must confirm a non-default backend is attached")
}

// TestBackendHintClickOpensTheField pins the mouse half. The hint registers a
// click zone every frame, and a zone whose key string no handler recognizes is
// decoration that swallows the click — the shape of the dead × in #2467.
func TestBackendHintClickOpensTheField(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	stubBackends(t, twoUsableBackends(), nil)
	startNaming(t, h, "clicked")

	msg, ok := keyMsgFromString("ctrl+r")
	require.True(t, ok, "the backend hint's key must map back to a key message")
	_, cmd := h.handleHintClick("ctrl+r")
	require.NotNil(t, cmd, "clicking the hint must start the catalog fetch")
	require.Equal(t, tea.KeyCtrlR, msg.Type)
}

// TestNamingFormBackendAndForceRemoteAgree pins the interaction with `N`, the
// legacy hook selector. Both can be set on one request; the daemon's precedence is
// explicit-backend-first (session/instance_factory.go resolveBackendKind), so the
// field must win over ForceRemote rather than being masked by it.
func TestNamingFormBackendAndForceRemoteAgree(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	stubBackends(t, twoUsableBackends(), nil)

	// `N` is now recorded on the model, not inferred from the naming row's
	// provisioned runtime (#2599 — the row provisions nothing at all any more), so
	// this is what startNewInstance(true) leaves behind. The end-to-end wiring from
	// the keypress to this field is covered by
	// TestStartNewRemoteThreadsForceRemoteFromTheKeypress; here it is set directly
	// so the assertion stays about the request, not about how the flag got set.
	remote, err := session.NewInstance(session.InstanceOptions{
		Title:   "forced-remote",
		Path:    t.TempDir(),
		Program: "claude",
		Backend: session.BackendLocal,
	})
	require.NoError(t, err)
	h.store.AddInstance(remote)
	h.namingInstance = remote
	h.pendingProgram = "claude"
	h.pendingForceRemote = true
	h.state = stateNew

	openBackendField(t, h)
	pickBackend(t, h, "docker")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, config.BackendDocker, got.Backend, "the picked backend must be sent")
	assert.True(t, got.ForceRemote,
		"N's selector is still reported; the daemon resolves the explicit backend first")
}

// TestBackendChoicesFromDescribesTheDefaultHonestly covers the pure mapping,
// including the case the web's twin exists to get right: a repo whose `backend`
// key names something unrecognized has NO default — such a create FAILS rather
// than falling back to local — so the row must not invent one.
func TestBackendChoicesFromDescribesTheDefaultHonestly(t *testing.T) {
	t.Run("names the resolved default", func(t *testing.T) {
		catalog := twoUsableBackends()
		catalog.Default = config.BackendDocker
		choices := backendChoicesFrom(catalog)
		require.NotEmpty(t, choices)
		assert.Equal(t, repoDefaultBackend, choices[0].value)
		assert.Equal(t, "Repo default (docker)", choices[0].label)
		assert.Equal(t, daemon.BackendAvailable, choices[0].status)
		assert.Empty(t, choices[0].reason)
	})

	t.Run("invents no default for a misconfigured repo", func(t *testing.T) {
		const reason = `this repo's .agent-factory/config.json sets backend = "dokcer", which is not a known backend`
		choices := backendChoicesFrom(daemon.ListBackendsResponse{
			Backends:      []daemon.BackendOption{{Name: config.BackendLocal, Label: "local", Status: daemon.BackendAvailable}},
			Default:       "",
			DefaultStatus: daemon.BackendUnavailable,
			DefaultReason: reason,
		})
		require.NotEmpty(t, choices)
		assert.Equal(t, "Repo default", choices[0].label, "no parenthetical when there is no default")
		assert.Equal(t, daemon.BackendUnavailable, choices[0].status)
		assert.Equal(t, reason, choices[0].reason)
		assert.Contains(t, choices[0].item(), "unavailable")
	})

	t.Run("uses the daemon's label for a repo-aware backend", func(t *testing.T) {
		label := "Remote sandbox · launch.sh (hook)"
		choices := backendChoicesFrom(daemon.ListBackendsResponse{
			Backends:      []daemon.BackendOption{{Name: config.BackendHook, Label: label, Status: daemon.BackendAvailable}},
			Default:       config.BackendHook,
			DefaultStatus: daemon.BackendAvailable,
		})
		require.Len(t, choices, 2)
		assert.Equal(t, fmt.Sprintf("Repo default (%s)", label), choices[0].label)
		assert.Equal(t, label, choices[1].label, "the hook row must read as the launcher the repo configured")
		assert.Equal(t, config.BackendHook, choices[1].value, "…while still sending the literal wire key")
	})
}
