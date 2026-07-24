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
// every phase, but only DaemonPhaseReady admits ordinary daemon work.
type DaemonPhase string

const (
	DaemonPhaseWarming          DaemonPhase = "warming"
	DaemonPhaseUpgradeProbation DaemonPhase = "upgrade_probation"
	DaemonPhaseReady            DaemonPhase = "ready"
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
	listeners     DaemonListenerStatus
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
	if l.transactionID != "" {
		return fmt.Errorf("cannot mark a probationary daemon ready without a transaction supervisor")
	}
	l.phase = DaemonPhaseReady
	return nil
}

func (l *daemonLifecycle) isUpgradeProbation() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.transactionID != ""
}

// mutationAdmissionError blocks every mutation for an upgrade candidate until
// its previous-binary supervisor releases probation. Normal warm-up deliberately
// remains allowed here: disk-backed task/config writes have always been safe
// during restore, while session-state mutations are separately gated on
// Manager.Ready by requireStateMutationAdmission.
func (l *daemonLifecycle) mutationAdmissionError() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transactionID != "" {
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
