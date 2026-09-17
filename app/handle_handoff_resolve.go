package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// handoffResolveAction names the verb a resolve-picker row dispatches (#4429).
// The two answers to "delivery was not confirmed" are not interchangeable —
// resend can double-deliver a mission that already landed, mark-delivered can
// retire one that never did — so the picker makes the operator choose after
// inspecting the pane rather than hiding one behind a flag.
type handoffResolveAction int

const (
	// handoffResolveResend dispatches the existing ResumeFromLimit path.
	// handoffResolveResendKind says what that call does on this row, and the
	// row's label says the same.
	handoffResolveResend handoffResolveAction = iota
	// handoffResolveConfirm retires the mission on the operator's attestation
	// that the pane already shows it landed — no resend.
	handoffResolveConfirm
)

// handoffResolveResendKind picks the label for the picker's resume row. Both
// kinds dispatch the same ResumeFromLimit call; what that call does differs,
// and the label has to say which.
type handoffResolveResendKind int

const (
	// handoffResolveNoResend offers no resume row: the daemon would refuse it.
	handoffResolveNoResend handoffResolveResendKind = iota
	// handoffResolveResendMission: the daemon re-verifies the pane and submits
	// the pending mission again.
	handoffResolveResendMission
	// handoffResolveResumeLimit: the row is only usage-limited, so the daemon
	// runs the plain limit resume. It un-stalls the agent and leaves the
	// pending mission unsent.
	handoffResolveResumeLimit
)

// handoffResolveResendLabels maps each resume kind to its picker row.
var handoffResolveResendLabels = map[handoffResolveResendKind]string{
	handoffResolveResendMission: "Retry send — submit the pending mission again",
	handoffResolveResumeLimit:   "Resume from limit — restart the stalled agent; the pending mission is not resent",
}

// handoffResolveState is the resolve picker's retained context, held on home
// alongside the overlay for the same reason handoffChoices is (#4429): the
// offered set is filtered by the row's verdict, so the selected index cannot
// be mapped back through a fixed enum order; and the target is the immutable
// session identity captured at open — a resend submitted against a reused
// title could double-deliver a mission to the wrong session (#2322).
type handoffResolveState struct {
	actions []handoffResolveAction
	target  sessionActionTarget
}

// openHandoffResolvePicker replaces a bare `c` with the two-verb choice when the
// selected session carries a confirmable pending delivery. A row that only
// supports resend never sees this picker — `c` stays the single keystroke it
// has always been. A row that only supports confirm STILL sees it, with one
// entry: the bar hint says "retry", and silently marking delivered under that
// label would be the same class of surprise this picker exists to remove.
func (m *home) openHandoffResolvePicker(selected *session.Instance, target sessionActionTarget, resend handoffResolveResendKind) (tea.Model, tea.Cmd) {
	actions := make([]handoffResolveAction, 0, 2)
	items := make([]string, 0, 2)
	if label, ok := handoffResolveResendLabels[resend]; ok {
		actions = append(actions, handoffResolveResend)
		items = append(items, label)
	}
	actions = append(actions, handoffResolveConfirm)
	items = append(items, "Mark delivered — retire the pending mission without resending (the pane already shows it landed)")

	m.handoffResolve = handoffResolveState{actions: actions, target: target}
	m.selectionOverlay = overlay.NewSelectionOverlay(
		fmt.Sprintf("Resolve delivery for '%s' — inspect the pane first", selected.Title), items)
	m.state = stateSelectHandoffResolve
	return m, nil
}

// handleStateSelectHandoffResolve drives the resolve picker. Submit dispatches
// the captured action against the captured identity — never the sidebar's
// current selection, which snapshots may have moved while the modal owned the
// keyboard (#2322).
func (m *home) handleStateSelectHandoffResolve(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.selectionOverlay == nil {
		m.state = stateDefault
		return m, nil
	}
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if !shouldClose {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	actions := m.handoffResolve.actions
	target := m.handoffResolve.target

	m.selectionOverlay = nil
	m.handoffResolve = handoffResolveState{}
	m.state = stateDefault
	m.menu.SetState(ui.StateDefault)

	if !submitted || idx < 0 || idx >= len(actions) {
		return m, nil
	}
	// Re-resolve the retained identity: a same-title row replaced while the
	// picker was open must not inherit the choice.
	if m.resolveSessionActionTarget(target) == nil {
		return m, nil
	}
	if actions[idx] == handoffResolveConfirm {
		return m, m.confirmHandoffDeliveryCmd(target)
	}
	return m, m.resumeFromLimitCmd(target)
}

// handoffDeliveryConfirmedMsg reports the async confirm's outcome, mirroring
// limitRetriedMsg so the event loop never blocks on the daemon call.
type handoffDeliveryConfirmedMsg struct {
	target sessionActionTarget
	err    error
}

// confirmHandoffDeliveryCmd runs the daemon confirm off the event loop — the
// same shape as resumeFromLimitCmd, but the daemon retires the mission WITHOUT
// touching the composer.
func (m *home) confirmHandoffDeliveryCmd(target sessionActionTarget) tea.Cmd {
	confirm := confirmHandoffDeliveryThroughDaemon
	return func() tea.Msg {
		if err := confirm(target.confirmHandoffDeliveryRequest()); err != nil {
			if !apiclient.IsMutationCommitted(err) {
				log.ErrorLog.Printf("could not confirm handoff delivery for %q: %v", target.title, err)
			}
			return handoffDeliveryConfirmedMsg{target: target, err: err}
		}
		return handoffDeliveryConfirmedMsg{target: target}
	}
}

// handleHandoffDeliveryConfirmed finalizes the async confirm. The daemon
// settled the fence, lifted startup-unknown, and retired the mission in one
// commit and already persisted; the TUI is a projection, so the next snapshot
// reconciles the row — here we only surface the outcome. A committed warning
// means the settle happened but its record may lag, so it reads as
// success-with-warning rather than failure.
func (m *home) handleHandoffDeliveryConfirmed(msg handoffDeliveryConfirmedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil && !apiclient.IsMutationCommitted(msg.err) {
		return m, m.handleError(fmt.Errorf("failed to confirm delivery for '%s': %w", msg.target.title, msg.err))
	}
	if msg.err != nil {
		return m, m.showTransientMessage(fmt.Sprintf("Delivery for '%s' confirmed, with warning: %v", msg.target.title, msg.err))
	}
	return m, m.showTransientMessage(fmt.Sprintf("Delivery for '%s' marked delivered — pending mission retired", msg.target.title))
}
