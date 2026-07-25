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
	// Preview* mirror the TCP* fields for the web-tab preview listener (#1856),
	// the second TCP listener bound from preview_listen_addr. PreviewConfigured is
	// whether preview_listen_addr is set; PreviewBound / PreviewBoundAddr report
	// whether it actually bound and on which concrete address.
	PreviewConfigured bool   `json:"preview_configured"`
	PreviewBound      bool   `json:"preview_bound"`
	PreviewListenAddr string `json:"preview_listen_addr,omitempty"`
	PreviewBoundAddr  string `json:"preview_bound_addr,omitempty"`
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
	released  bool
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

// markRestoreComplete moves an upgrade launch into probation. A normal launch
// remains warming until RunDaemon has armed its scheduler, watchers, and poll
// loop; a restored Manager is not by itself a fully ready daemon.
func (l *daemonLifecycle) markRestoreComplete() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restored = true
	if l.transactionID != "" {
		l.phase = DaemonPhaseUpgradeProbation
	}
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
		return fmt.Errorf("cannot release upgrade probation before instance restore completes")
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

// mutationAdmissionError blocks every mutation for an upgrade candidate until its
// previous-binary supervisor releases probation. Normal warm-up deliberately
// remains allowed here: disk-backed task/config writes have always been safe
// during restore, while session-state mutations are separately gated on
// Manager.Ready by requireStateMutationAdmission.
func (l *daemonLifecycle) mutationAdmissionError() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
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
}

func (l *daemonLifecycle) clearTCPBound() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.TCPBound = false
	l.listeners.TCPBoundAddr = ""
}

func (l *daemonLifecycle) setPreviewBound(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewBound = true
	l.listeners.PreviewBoundAddr = addr
}

func (l *daemonLifecycle) clearPreviewBound() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listeners.PreviewBound = false
	l.listeners.PreviewBoundAddr = ""
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
