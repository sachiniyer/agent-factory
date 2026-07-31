package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// The naming form's backend field (#1933).
//
// The daemon has always accepted a backend on create (CreateSessionRequest.Backend,
// the CLI's `--backend`), and #1968 gave the web a picker over the daemon's
// ListBackends catalog. The TUI was the surface left out: `N` reaches the hook
// backend and nothing else, so a docker or ssh session could not be created from
// the primary interface at all — a user had to edit the repo's checked-in `backend`
// key, which is all-or-nothing per repo, or drop to the CLI.
//
// Two rules this file exists to hold, both borrowed from its web twin
// (web/src/backends.ts), because a second implementation of either is how the two
// pickers would start disagreeing:
//
//  1. THE TUI KNOWS NO BACKEND NAMES. There is no local enum here and no
//     name→label map: every row is built from the daemon's response, so a backend
//     added server-side is offered by this picker with no change to this file.
//  2. AVAILABILITY IS THE DAEMON'S ANSWER, VERBATIM. Its tri-state
//     (available / unavailable+reason / unknown+reason) is rendered as three
//     outcomes, never collapsed into two. "Could not check" is not "fine".

// repoDefaultBackend is the sentinel for the picker's "repo default" row. It is
// the EMPTY STRING on purpose: it is exactly what the create request omits on, so
// choosing the default sends no backend and the repo's own config decides. Any
// other sentinel would eventually be transmitted as a literal backend name.
const repoDefaultBackend = ""

// backendChoice is one row of the picker.
type backendChoice struct {
	// value is what goes on CreateSessionRequest.Backend: repoDefaultBackend, or a
	// backend name to send verbatim.
	value string
	// label is the daemon-supplied presentation text (BackendOption.Label), which is
	// repo-aware — the hook backend names the launcher the repo configured.
	label string
	// status is the daemon's checked answer for this choice.
	status daemon.BackendAvailability
	// reason is the actionable explanation whenever status is not available — the
	// same sentence the CLI prints at create time. Empty when available.
	reason string
}

// backendChoicesFrom turns the daemon's catalog into the picker's rows: the
// repo-default row first, labelled with the backend it actually resolves to, then
// every backend the daemon listed in its canonical order.
//
// Unusable backends are LISTED, not hidden. A user who read docs/backends.md and
// looks for docker deserves the reason it is unusable here; a mystery absence
// leaves them re-reading their config for something that is not wrong with it.
func backendChoicesFrom(catalog daemon.ListBackendsResponse) []backendChoice {
	label := "Repo default"
	if catalog.Default != "" {
		// Name the resolved backend from the catalog's own label for it, so a repo
		// defaulting to docker says so without the user having to pick docker.
		resolved := catalog.Default
		for _, opt := range catalog.Backends {
			if opt.Name == catalog.Default {
				resolved = opt.Label
				break
			}
		}
		label = fmt.Sprintf("Repo default (%s)", resolved)
	}
	// A repo whose `backend` key names something unrecognized has NO default: such a
	// create fails rather than falling back to local, so the row stays "Repo default"
	// with no parenthetical and carries the daemon's misconfiguration reason.
	choices := []backendChoice{{
		value:  repoDefaultBackend,
		label:  label,
		status: catalog.DefaultStatus,
		reason: catalog.DefaultReason,
	}}
	if catalog.DefaultStatus == daemon.BackendAvailable {
		choices[0].reason = ""
	}

	for _, opt := range catalog.Backends {
		choice := backendChoice{value: opt.Name, label: opt.Label, status: opt.Status, reason: opt.Reason}
		if opt.Status == daemon.BackendAvailable {
			choice.reason = ""
		}
		choices = append(choices, choice)
	}
	return choices
}

// item is the row as the selection overlay renders it: the label plus a marker for
// a status that is not available.
//
// The marker exists so the LIST is honest before a keypress — a bare "docker"
// among usable rows is the picker-as-a-promise problem the catalog's tri-state was
// built to avoid. The reason itself is deliberately not inlined: the overlay
// truncates each row to its text rect, so a sentence naming a config key and a
// file would be cut mid-word. It is shown in full when the row is chosen.
func (c backendChoice) item() string {
	switch c.status {
	case daemon.BackendUnavailable:
		return c.label + " — unavailable"
	case daemon.BackendUnknown:
		return c.label + " — cannot check"
	default:
		return c.label
	}
}

// backendCatalogMsg carries a fetched catalog back onto the event loop. naming is
// the instance whose form asked for it: the fetch is async, so by the time it
// lands the user may have submitted, cancelled, or started naming a different
// session, and the picker must not open over an unrelated form.
type backendCatalogMsg struct {
	naming  *session.Instance
	catalog daemon.ListBackendsResponse
	err     error
}

// listBackendsThroughDaemon is the fetch seam, mirroring startSessionThroughDaemon:
// a package var so the unit suite can answer with a fixture instead of a daemon.
var listBackendsThroughDaemon = func(repoPath string) (daemon.ListBackendsResponse, error) {
	var resp daemon.ListBackendsResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.ListBackends(daemon.ListBackendsRequest{RepoPath: repoPath})
		return e
	})
	if err != nil {
		return daemon.ListBackendsResponse{}, err
	}
	return resp, nil
}

// SetBackendListerForTest swaps the catalog seam so a test can answer with a
// fixture — including backends no local enum knows about, which is how the
// "the TUI knows no backend names" property is provable rather than asserted.
func SetBackendListerForTest(f func(repoPath string) (daemon.ListBackendsResponse, error)) func() {
	prev := listBackendsThroughDaemon
	listBackendsThroughDaemon = f
	return func() { listBackendsThroughDaemon = prev }
}

// openBackendPicker starts the round trip that opens the backend field. The
// catalog is fetched on demand rather than prefetched with every `n`: most creates
// never touch this field, and a stale catalog is worse than a fresh one — a repo's
// config can change between opening the form and opening the field.
func (m *home) openBackendPicker() (tea.Model, tea.Cmd) {
	naming := m.namingInstance
	if naming == nil {
		return m, nil
	}
	repoPath := naming.Path
	fetch := listBackendsThroughDaemon
	return m, func() tea.Msg {
		catalog, err := fetch(repoPath)
		return backendCatalogMsg{naming: naming, catalog: catalog, err: err}
	}
}

// handleBackendCatalog opens the picker over a delivered catalog.
//
// The staleness guard is the whole reason this is a message rather than a direct
// call: naming may have ended (Enter/Esc/ctrl+c) or moved to a different session
// while the fetch was in flight, and an overlay opened over that would be a modal
// the user never asked for, editing a create that no longer exists.
func (m *home) handleBackendCatalog(msg backendCatalogMsg) (tea.Model, tea.Cmd) {
	if m.state != stateNew || m.namingInstance == nil || m.namingInstance != msg.naming {
		return m, nil
	}
	if msg.err != nil {
		// Lead with what failed and what it blocks; the daemon's own error text
		// follows. The naming form stays open — the field is optional, so a catalog
		// we could not read must not cost the user their half-typed create.
		return m, m.handleError(fmt.Errorf("cannot list backends for this repo: %w", msg.err))
	}
	choices := backendChoicesFrom(msg.catalog)
	if len(choices) == 0 {
		// Defensive: the response always carries at least the repo-default row. Say
		// so rather than opening an empty modal the user has to escape from.
		return m, m.handleError(errors.New("the daemon returned no backends for this repo"))
	}

	selected := 0
	for i, choice := range choices {
		if choice.value == m.pendingBackend {
			selected = i
			break
		}
	}
	m.backendPickerChoices = choices
	items := make([]string, len(choices))
	for i, choice := range choices {
		items[i] = choice.item()
	}
	m.selectionOverlay = overlay.NewSelectionOverlay("Select backend", items)
	m.selectionOverlay.SetSelectedIndex(selected)
	// layoutSelectionOverlay sizes it to the same 60% box the program field opens
	// to, so the naming form's three fields are one form and not three dialogs.
	m.layoutSelectionOverlay()
	m.state = stateSelectBackend
	return m, nil
}

// handleStateSelectBackend handles key events while the backend field is open.
// Mirrors handleStateSelectProgram: the field is part of the create form, so
// submitting or escaping returns to naming rather than abandoning the create.
//
// An unusable choice is REFUSED here, at pick time, with the daemon's reason. The
// web's picker keeps such a row selectable and blocks the submit instead; a modal
// TUI overlay has nowhere to show a blocking explanation, and refusing on choice
// tells the user what to fix at the moment they asked for it — while their create
// is still open and retryable. Either way the invariant is the same: no surface
// offers a backend it was told would fail.
func (m *home) handleStateSelectBackend(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.selectionOverlay == nil {
		m.backendPickerChoices = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		return m, nil
	}
	if !m.selectionOverlay.HandleKeyPress(msg) {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	choices := m.backendPickerChoices

	m.selectionOverlay = nil
	m.backendPickerChoices = nil
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}
	choice := choices[idx]
	if choice.status != daemon.BackendAvailable {
		// The reason is the daemon's, verbatim: it names the key and the file to fix,
		// and it is the SAME sentence a create would have failed with, so the picker
		// cannot describe a precondition differently from the thing that enforces it.
		//
		// Only BackendUnavailable is a designed refusal — a precondition that was
		// CHECKED and failed, which the user fixes by editing the repo's config. The
		// other two non-available shapes are failures wearing a refusal's clothes and
		// keep ERROR severity:
		//
		//   - no reason at all violates BackendOption's contract (every non-available
		//     status carries one), so the daemon's answer is malformed. It would also
		//     otherwise flash an empty notice — the keypress must always say
		//     something (#2020).
		//   - BackendUnknown means the preconditions could not be EVALUATED, i.e.
		//     reading or parsing the in-repo config failed. daemon/backends.go keeps
		//     that distinct from "unavailable" precisely so a client does not report
		//     an I/O failure as a settled no.
		//
		// So the notice is gated on BackendUnavailable POSITIVELY, not on "anything
		// that isn't unknown". A wire value this build does not recognize — a
		// version-skewed daemon, a status added server-side — is another broken
		// response, and defaulting it to a notice would silently mute whatever a
		// future status means. Downgrade only what we can name.
		if choice.reason == "" {
			return m, m.handleError(fmt.Errorf("backend %q is not usable for this repo, and the daemon gave no reason", choice.label))
		}
		if choice.status != daemon.BackendUnavailable {
			return m, m.handleError(errors.New(choice.reason))
		}
		return m, m.handleNotice(errors.New(choice.reason))
	}
	m.pendingBackend = choice.value
	m.menu.SetNamingBackend(m.pendingBackend != repoDefaultBackend)
	return m, nil
}
