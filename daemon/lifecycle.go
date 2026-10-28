package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// DaemonPhase is the daemon's startup/admission state reported by Ping. It is
// deliberately separate from process liveness: the control socket answers in
// every phase, but only a daemon that has armed its scheduler, watchers, status
// poll, and home watcher reaches DaemonPhaseReady. Mutation admission is gated on
// the released flag, NOT on this phase — a released upgrade candidate is
// DaemonPhaseHandoffPending, which admits work yet is honestly not ready.
type DaemonPhase string

const (
	DaemonPhaseWarming          DaemonPhase = "warming"
	DaemonPhaseUpgradeProbation DaemonPhase = "upgrade_probation"
	// DaemonPhaseHandoffPending is a released upgrade candidate. It ran ad-hoc
	// through probation and is parked in RunDaemon's probation branch, so its
	// scheduler, watcher supervisor, status poll, and home watcher are NOT armed and
	// never will be — it exists only to be replaced by the post-commit hand-off.
	// Reporting it as DaemonPhaseReady (which markReady documents as the full
	// operational barrier) would be a fabricated health signal that hides a
	// failed/skipped hand-off; it carries its own phase so Ping / af daemon status /
	// af doctor / the HTTP health surface render it honestly (#2212 R2a).
	DaemonPhaseHandoffPending DaemonPhase = "handoff_pending"
	DaemonPhaseReady          DaemonPhase = "ready"
	// DaemonPhaseQuiescing is a formerly-ready daemon that has authorized an upgrade
	// activation and is stopping admitting new work before it exits to free the
	// socket for the validated candidate. It is NOT ready — it is refusing mutations
	// and about to go away — so, by the same principle as DaemonPhaseHandoffPending,
	// it must not keep reporting DaemonPhaseReady: a daemon stuck mid-hand-off has to
	// be VISIBLE to `af daemon status` / `af doctor`, not look healthy (#2212 R2b).
	DaemonPhaseQuiescing DaemonPhase = "quiescing"
)

// DaemonListenerStatus reports which auxiliary HTTP surfaces this daemon
// actually bound. The control socket is omitted because receiving Ping already
// proves that listener. TCPListenAddr is the configured address;
// TCPBoundAddr is the concrete address (and port) returned by the kernel.
type DaemonListenerStatus struct {
	HTTPUnixBound bool   `json:"http_unix_bound"`
	TCPConfigured bool   `json:"tcp_configured"`
	TCPBound      bool   `json:"tcp_bound"`
	TCPListenAddr string `json:"tcp_listen_addr,omitempty"`
	TCPBoundAddr  string `json:"tcp_bound_addr,omitempty"`
	// TCPRefusalReason is non-empty only while the daemon is REFUSING to bind
	// the configured control listener: a non-loopback listen_addr with
	// network.require_token off and no network.allow_unauthenticated_network
	// opt-in (#5137). It distinguishes "configured but the bind failed or was
	// never attempted" (the not-bound case) from "deliberately not bound" — and
	// carries the shared remediation sentence verbatim, so every surface that
	// renders it (af daemon status, af doctor, Ping's listeners block) states
	// the same three fixes. A bound listener can never carry one: setTCPBound
	// clears it.
	TCPRefusalReason string `json:"tcp_refusal_reason,omitempty"`
	// Preview* mirror the TCP* fields for the web-tab preview listener (#1856),
	// the second TCP listener bound from preview_listen_addr. PreviewConfigured is
	// whether preview_listen_addr is set; PreviewBound / PreviewBoundAddr report
	// whether it actually bound and on which concrete address.
	PreviewConfigured bool   `json:"preview_configured"`
	PreviewBound      bool   `json:"preview_bound"`
	PreviewListenAddr string `json:"preview_listen_addr,omitempty"`
	PreviewBoundAddr  string `json:"preview_bound_addr,omitempty"`
	// TCPBindError / PreviewBindError carry the last bind failure while that
	// listener is not accepting, and BindRetrying says the daemon is still
	// retrying it in the background (#5571 W9.9), so `af daemon status` shows a
	// listener that lost a startup race (a tailnet IP not yet assigned) as
	// pending rather than silently absent.
	TCPBindError     string `json:"tcp_bind_error,omitempty"`
	PreviewBindError string `json:"preview_bind_error,omitempty"`
	BindRetrying     bool   `json:"bind_retrying,omitempty"`
}

type daemonLifecycleSnapshot struct {
	bootID        string
	transactionID string
	phase         DaemonPhase
	listeners     DaemonListenerStatus
}

// daemonLifecycle is the single source of truth for health and mutation
// admission. Keeping those together prevents a candidate from reporting one
// phase while enforcing another.
type daemonLifecycle struct {
	mu sync.RWMutex

	bootID        string
	transactionID string
	phase         DaemonPhase
	restored      bool
	// released is set when the previous-binary supervisor lifts this candidate's
	// probation. It gates mutation ADMISSION; transactionID is kept for the whole
	// boot as the supervisor's IDENTITY (#1947), so the two never conflate.
	released bool
	// releaseRequested records a release that arrived while the instance restore
	// was still running. markRestoreComplete applies it, so released still
	// implies restored and admission never opens over a half-loaded manager —
	// but the supervisor's single approve call is accepted instead of refused,
	// which used to fail the actor until its restart budget ran out and wedge
	// the upgrade permanently (#5693).
	releaseRequested bool
	// quiescing is set when this daemon has authorized an upgrade activation and is
	// handing off. It closes admission (the daemon is about to exit) and drives the
	// reported DaemonPhaseQuiescing so a stuck hand-off is visible, not silent.
	quiescing bool
	listeners DaemonListenerStatus
}

const daemonBootIDBytes = 16

func newDaemonLifecycle(transactionID, tcpListenAddr, previewListenAddr string) (*daemonLifecycle, error) {
	if transactionID != "" && strings.TrimSpace(transactionID) == "" {
		return nil, fmt.Errorf("upgrade transaction ID cannot be blank")
	}
	bootID, err := generateDaemonBootID()
	if err != nil {
		return nil, err
	}
	lifecycle := &daemonLifecycle{
		bootID:        bootID,
		transactionID: transactionID,
		phase:         DaemonPhaseWarming,
		listeners: DaemonListenerStatus{
			TCPConfigured:     tcpListenAddr != "",
			TCPListenAddr:     tcpListenAddr,
			PreviewConfigured: previewListenAddr != "",
			PreviewListenAddr: previewListenAddr,
		},
	}
	return lifecycle, nil
}

func generateDaemonBootID() (string, error) {
	random := make([]byte, daemonBootIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate daemon boot ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

// markRestoreComplete moves an upgrade launch into probation — or straight to
// DaemonPhaseHandoffPending when its supervisor's release already arrived during
// the restore (#5693). A normal launch remains warming until RunDaemon has armed
// its scheduler, watchers, and poll loop; a restored Manager is not by itself a
// fully ready daemon.
func (l *daemonLifecycle) markRestoreComplete() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restored = true
	if l.transactionID == "" {
		return
	}
	if l.releaseRequested {
		l.released = true
		l.phase = DaemonPhaseHandoffPending
		return
	}
	l.phase = DaemonPhaseUpgradeProbation
}

func (l *daemonLifecycle) markReady() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.restored {
		return fmt.Errorf("cannot mark daemon ready before instance restore completes")
	}
	if l.inProbationLocked() {
		return fmt.Errorf("cannot mark a probationary daemon ready without a transaction supervisor")
	}
	l.phase = DaemonPhaseReady
	return nil
}

// markQuiescing moves a daemon into terminal quiescence: it stops admitting new
// mutations and reports DaemonPhaseQuiescing. Upgrade activation calls it after
// authorization; ordinary shutdown calls it before draining the control planes.
// A daemon stuck in either drain is therefore visible rather than reporting ready.
// Idempotent.
func (l *daemonLifecycle) markQuiescing() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.quiescing = true
	l.phase = DaemonPhaseQuiescing
}

// releaseUpgradeProbation lifts an upgrade candidate's probation once its
// previous-binary supervisor has validated it, so it admits ordinary daemon work.
// expectedTransactionID must match the probation this daemon is under — a mismatch
// means the request is for a different candidate and is refused, so a stale or
// misdirected release can never arm the wrong daemon.
//
// It sets a `released` flag; it does NOT clear transactionID. That id is the
// supervisor's identity for this candidate boot and must be reported by Ping for
// the WHOLE boot, including after release, so the supervisor can still reject a
// different daemon that answers the same socket (#1947, control_types.go). Erasing
// it would make the supervisor's post-commit re-runs of StartCandidate/
// ValidateCandidate — PhaseCommitted is fsynced before this and only cleared by
// Cleanup, so any re-entry replays them — miss the candidate and never converge.
// Admission is gated on `released` instead of on the id. Idempotent: a second
// release for the same transaction is a no-op, so a re-run cannot fail.
//
// The phase becomes DaemonPhaseHandoffPending, NOT DaemonPhaseReady: this candidate
// is parked and never arms its operational loops (markReady is unreachable for it),
// so reporting it ready would let a skipped or failed post-commit hand-off pass as
// a healthy daemon. The distinct phase keeps the readiness surfaces honest while
// admission opens.
//
// A release that arrives BEFORE the instance restore completes is accepted and
// deferred, not refused (#5693): markRestoreComplete applies it, and until then
// the phase stays warming and admission stays closed. The supervisor sends it
// once, right after Commit — the irreversible verdict — and a refusal failed the
// whole recovery actor, whose only retry is a service-manager restart budget
// sized for a crash loop, not for a slow restore. The supervisor is the PREVIOUS
// binary, so whether it waits for restore before approving is out of this
// binary's hands; accepting the early release is what keeps any supervisor from
// wedging this candidate.
func (l *daemonLifecycle) releaseUpgradeProbation(expectedTransactionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.transactionID == "" {
		return fmt.Errorf("daemon is not an upgrade candidate")
	}
	if expectedTransactionID != l.transactionID {
		return fmt.Errorf("upgrade probation release for transaction %q does not match this daemon's transaction %q",
			expectedTransactionID, l.transactionID)
	}
	if l.released {
		return nil // idempotent: already released for this transaction
	}
	if !l.restored {
		l.releaseRequested = true // applied by markRestoreComplete
		return nil
	}
	l.released = true
	l.phase = DaemonPhaseHandoffPending
	return nil
}

// inProbationLocked reports whether this daemon is an upgrade candidate that has
// NOT yet been released. It is the admission predicate — separate from the
// transaction identity, which is retained for the whole boot. Caller holds l.mu.
func (l *daemonLifecycle) inProbationLocked() bool {
	return l.transactionID != "" && !l.released
}

func (l *daemonLifecycle) isUpgradeProbation() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inProbationLocked()
}

// isQuiescing reports whether the daemon has closed mutation admission and is
// draining toward exit. Task delivery's shutdown classifier (#5503) reads it:
// an in-flight create whose transport error arrives after this flips is the
// shutdown interrupting the run, not a task failure.
func (l *daemonLifecycle) isQuiescing() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.quiescing
}

// mutationAdmissionError blocks every mutation for an upgrade candidate until its
// previous-binary supervisor releases probation. Normal warm-up deliberately
// remains allowed here: disk-backed task/config writes have always been safe
// during restore, while session-state mutations are separately gated on
// Manager.Ready by requireStateMutationAdmission.
func (l *daemonLifecycle) mutationAdmissionError() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.quiescing {
		return errDaemonQuiescing()
	}
	if l.inProbationLocked() {
		return errDaemonUpgradeProbation(l.transactionID)
	}
	return nil
}

func (l *daemonLifecycle) snapshot() daemonLifecycleSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return daemonLifecycleSnapshot{
		bootID:        l.bootID,
		transactionID: l.transactionID,
		phase:         l.phase,
		listeners:     l.listeners,
	}
}

func (l *daemonLifecycle) setHTTPUnixBound(bound bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.HTTPUnixBound = bound
}

func (l *daemonLifecycle) setTCPBound(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPBound = true
	l.listeners.TCPBoundAddr = addr
	// A listener that just bound cannot be in the refused posture — the refusal
	// means "no listener". Clearing here keeps the two states exclusive even if
	// a posture flip raced a rebind.
	l.listeners.TCPRefusalReason = ""
}

// setTCPRefused records the one configured-but-not-bound state that is a
// DECISION rather than a failure: the #5137 refusal of a tokenless non-loopback
// control listener. It is a single locked update — configured, unbound, and
// reasoned together — so a concurrent /v1/health or `af daemon status` snapshot
// can never render the pair half-applied: a refused listener always reports
// TCPConfigured=true with TCPBound=false and the remediation text beside them.
func (l *daemonLifecycle) setTCPRefused(addr, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPConfigured = true
	l.listeners.TCPListenAddr = addr
	l.listeners.TCPBound = false
	l.listeners.TCPBoundAddr = ""
	l.listeners.TCPRefusalReason = reason
}

func (l *daemonLifecycle) clearTCPBound() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPBound = false
	l.listeners.TCPBoundAddr = ""
}

// setTCPConfigured re-syncs the configured half of the TCP listener pair to the
// address a *successful* live ApplyConfig just established. It is the configured
// counterpart of setTCPBound/clearTCPBound: those track what the kernel returned,
// this tracks what the operator asked for. Before #2480 PR2 the configured fields
// were written only by newDaemonLifecycle and never again, which was correct while
// no live rebind existed; the live rebind path updated the bound half on every
// rebind but left the configured half frozen at the boot-time value, so a
// successful `af config set network.listen_addr` that crossed the
// configured↔unconfigured boundary left TCPConfigured/TCPListenAddr contradicting
// TCPBound for the rest of the boot — exactly the stale snapshot `af daemon status`
// keys on (`daemoncmd.go` checks TCPConfigured first) and `/v1/health` serialises.
// Call this ONLY from the config-driven branches of bindWebLocked: after a
// successful bind (with the configured address, not the kernel-resolved one),
// on the addr=="" opt-out teardown, and on the no-retained-socket pre-bind
// resync that leaves the refused posture (a failed bind there is honestly
// "configured, not bound" — never a stale refusal). The
// unexpected-listener-death closure must NOT
// call it — network.listen_addr is still set there, so the configured half must
// stay so `af daemon status` renders "<addr> (not bound)" rather than "disabled".
// The refusal half clears here too: every caller of this setter is a posture
// that ISN'T refused (an allowed bind, or the opt-out), so a reason left over
// from a refused generation would be stale.
func (l *daemonLifecycle) setTCPConfigured(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPConfigured = addr != ""
	l.listeners.TCPListenAddr = addr
	l.listeners.TCPRefusalReason = ""
}

func (l *daemonLifecycle) setPreviewBound(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewBound = true
	l.listeners.PreviewBoundAddr = addr
}

func (l *daemonLifecycle) setTCPBindError(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPBindError = msg
}

func (l *daemonLifecycle) setPreviewBindError(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewBindError = msg
}

func (l *daemonLifecycle) setBindRetrying(retrying bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.BindRetrying = retrying
}

func (l *daemonLifecycle) clearPreviewBound() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewBound = false
	l.listeners.PreviewBoundAddr = ""
}

// setPreviewConfigured is setTCPConfigured for the web-tab preview listener
// (#1856): it re-syncs the configured half on a successful live preview_listen_addr
// rebind. Same scoping as setTCPConfigured — called only from the config-driven
// success branches of bindPreviewLocked, never from the listener-death closure.
func (l *daemonLifecycle) setPreviewConfigured(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewConfigured = addr != ""
	l.listeners.PreviewListenAddr = addr
}

func (l *daemonLifecycle) clearHTTPListeners() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.HTTPUnixBound = false
	l.listeners.TCPBound = false
	l.listeners.TCPBoundAddr = ""
	l.listeners.PreviewBound = false
	l.listeners.PreviewBoundAddr = ""
}
