package session

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// The per-host readiness cache (#5571 W2.3 part B). ListBackends is documented
// as read-only and dialing nothing, so it cannot ask a sandbox host whether it
// can run a session. It reads this instead: the verdict of the last REAL
// contact with the host (today the create-time prerequisite probe), so the
// answer is as fresh as af's last conversation with the host and never costs a
// dial. A create always re-runs the live check, so a wrong cache entry costs an
// actionable error at create, never a silent bad provision.

// HostReadinessState is how a cached verdict should be read.
type HostReadinessState string

const (
	// HostReadinessUnprobed: af has not contacted the host in this daemon's life.
	HostReadinessUnprobed HostReadinessState = "unprobed"
	// HostReadinessReady: the last probe passed and is recent.
	HostReadinessReady HostReadinessState = "ready"
	// HostReadinessBlocked: the last probe found the host unable to run a session
	// and is recent. Reason names what to fix.
	HostReadinessBlocked HostReadinessState = "blocked"
	// HostReadinessStale: there is a verdict, but it is too old to be presented
	// as current. Never shown as plain available.
	HostReadinessStale HostReadinessState = "stale"
)

const (
	// hostReadinessFreshFor bounds how long a passing verdict reads as current.
	hostReadinessFreshFor = 15 * time.Minute
	// hostReadinessBlockFor bounds how long a failing verdict keeps the backend
	// marked unavailable. It is short on purpose: a blocked backend cannot be
	// selected, so nothing would re-probe it to clear the verdict after the
	// operator fixes the host.
	hostReadinessBlockFor = 5 * time.Minute
)

// HostReadiness is one host's cached verdict as read at a moment.
type HostReadiness struct {
	State HostReadinessState
	// Reason is the actionable text when State is blocked (and the last failure
	// when stale).
	Reason string
	// Checked is when the verdict was recorded; zero when unprobed.
	Checked time.Time
	// Age is time since Checked at read time.
	Age time.Duration
}

type hostVerdict struct {
	reason  string
	checked time.Time
}

var (
	hostReadinessMu    sync.Mutex
	hostReadinessCache = map[string]hostVerdict{}
	hostReadinessNow   = time.Now
)

// RecordHostReadiness stores the outcome of a real contact with the host
// identified by key (the sandbox ssh command). A nil blocked means the host
// passed.
func RecordHostReadiness(key string, blocked error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	v := hostVerdict{checked: hostReadinessNow()}
	if blocked != nil {
		v.reason = blocked.Error()
	}
	hostReadinessMu.Lock()
	hostReadinessCache[key] = v
	hostReadinessMu.Unlock()
}

// ReadHostReadiness returns the cached verdict for key. It never dials.
func ReadHostReadiness(key string) HostReadiness {
	key = strings.TrimSpace(key)
	hostReadinessMu.Lock()
	v, ok := hostReadinessCache[key]
	hostReadinessMu.Unlock()
	if !ok {
		return HostReadiness{State: HostReadinessUnprobed}
	}
	age := hostReadinessNow().Sub(v.checked)
	r := HostReadiness{Reason: v.reason, Checked: v.checked, Age: age}
	switch {
	case v.reason != "" && age <= hostReadinessBlockFor:
		r.State = HostReadinessBlocked
	case age > hostReadinessFreshFor:
		r.State = HostReadinessStale
	case v.reason != "":
		r.State = HostReadinessStale
	default:
		r.State = HostReadinessReady
	}
	return r
}

// Describe is the one-line, surface-neutral note clients render beside the
// backend. Empty for ready: only the cases that must not read as plain
// available carry text.
func (r HostReadiness) Describe() string {
	switch r.State {
	case HostReadinessUnprobed:
		return "not probed yet — the host is checked when a session is created"
	case HostReadinessStale:
		return fmt.Sprintf("last checked %s ago — the host is re-checked when a session is created", r.Age.Round(time.Minute))
	case HostReadinessBlocked:
		return fmt.Sprintf("checked %s ago", r.Age.Round(time.Second))
	}
	return ""
}
