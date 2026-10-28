package daemon

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// CheckHost: probe the operator's listed hosts on demand and on a timer
// (#5571 W2.3). The create-time probe was the only writer of the readiness
// cache, so a host nobody had created on read "unprobed" forever. This is the
// same probe, run without creating anything.

// CheckHostRequest names the listed host to probe. Empty probes every listed
// host. A name outside the global host list is refused before anything is
// dialed (config.ValidateHostPick), so this RPC cannot aim af at an arbitrary
// machine.
type CheckHostRequest struct {
	Host string `json:"host,omitempty"`
}

// HostCheckResult is one host's probe outcome.
type HostCheckResult struct {
	Name string `json:"name"`
	// Probed is false when this host was skipped because another probe of it was
	// already running; Readiness then carries the cached verdict.
	Probed bool `json:"probed"`
	// Reachable is false when the probe could not run; Error says why. Nothing is
	// recorded for an unreachable host.
	Reachable bool     `json:"reachable"`
	Error     string   `json:"error,omitempty"`
	Problems  []string `json:"problems,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	// Complete is true only when git, tmux and gh each gave a recognised answer.
	// A reachable host with Complete false answered something af could not read,
	// so it is unknown, not ready (#5841). Absent from an older daemon's reply,
	// which therefore also reads as unknown.
	Complete bool `json:"complete"`
	// Readiness is the cached verdict after the probe: ready | blocked | stale |
	// unprobed.
	Readiness string `json:"readiness"`
}

// CheckHostResponse lists the probed hosts in name order.
type CheckHostResponse struct {
	Hosts []HostCheckResult `json:"hosts"`
}

// hostProbes serializes probes per host: a second request for a host already
// being probed is skipped rather than stacked behind a slow ssh.
type hostProbes struct {
	mu       sync.Mutex
	inFlight map[string]bool
}

var daemonHostProbes = &hostProbes{inFlight: map[string]bool{}}

func (h *hostProbes) tryAcquire(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[name] {
		return false
	}
	h.inFlight[name] = true
	return true
}

func (h *hostProbes) release(name string) {
	h.mu.Lock()
	delete(h.inFlight, name)
	h.mu.Unlock()
}

// probeHostsFn is the seam tests replace; production probes over ssh.
var probeHostsFn = session.CheckHost

// hostProbeFunc is the shape of the probe seam (session.CheckHost).
type hostProbeFunc = func(ctx context.Context, name string, hosts map[string]config.HostConfig, posture string, timeout time.Duration) (session.HostCheck, error)

// probeOne probes one host with the given probe. The caller passes the probe in
// rather than this function reading probeHostsFn, so a background loop that
// outlives its starter never touches the package variable after it starts.
func probeOne(ctx context.Context, probe hostProbeFunc, name string, hosts map[string]config.HostConfig, posture string) HostCheckResult {
	res := HostCheckResult{Name: name}
	if !daemonHostProbes.tryAcquire(name) {
		res.Readiness = string(session.ReadHostReadiness(session.HostReadinessKey(name)).State)
		return res
	}
	defer daemonHostProbes.release(name)
	c, err := probe(ctx, name, hosts, posture, session.HostCheckTimeout)
	res.Probed = true
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Reachable, res.Error, res.Problems, res.Warnings, res.Complete = c.Reachable, c.Error, c.Problems, c.Warnings, c.Complete
	}
	res.Readiness = string(session.ReadHostReadiness(session.HostReadinessKey(name)).State)
	return res
}

// CheckHost probes req.Host (or every listed host) and records each verdict in
// the readiness cache. The probe runs under the daemon's advisory scope, so a
// shutdown kills its ssh child instead of waiting for it (#5839): a control
// handler still in flight would otherwise hold closeControl, and with it the
// final checkpoint, for up to the 30s probe bound.
func (s *controlServer) CheckHost(req CheckHostRequest, resp *CheckHostResponse) error {
	// Blocked while the daemon is an upgrade candidate in probation: the probe
	// dials the operator's hosts and may learn host keys (see the ledger entry in
	// lifecycle_test.go). It needs no manager state, so only the admission gate.
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	ctx := s.manager.advisoryContext()
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("cannot read the host list: %w", err)
	}
	name := strings.TrimSpace(req.Host)
	if name != "" {
		if err := config.ValidateHostPick(cfg.Hosts, name); err != nil {
			return err
		}
		resp.Hosts = []HostCheckResult{probeOne(ctx, probeHostsFn, name, cfg.Hosts, cfg.SSHHostKeyVerification)}
		return nil
	}
	for _, n := range config.SortedHostNames(cfg.Hosts) {
		if ctx.Err() != nil {
			break
		}
		resp.Hosts = append(resp.Hosts, probeOne(ctx, probeHostsFn, n, cfg.Hosts, cfg.SSHHostKeyVerification))
	}
	return nil
}

// hostRefreshInterval is how often the supervisor re-probes the listed hosts.
// Under hostReadinessFreshFor (15m) so a healthy host never reads stale. A var
// so a test can shorten it.
var hostRefreshInterval = 10 * time.Minute

// refreshListedHosts probes every listed host once under ctx, with probe.
func refreshListedHosts(ctx context.Context, manager *Manager, probe hostProbeFunc) {
	// A candidate in upgrade probation dials nothing and learns no host keys, as
	// the CheckHost RPC refuses for the same window.
	if manager != nil && manager.lifecycle != nil && manager.lifecycle.isUpgradeProbation() {
		return
	}
	cfg, err := config.LoadConfig()
	if err != nil || len(cfg.Hosts) == 0 {
		return
	}
	for _, n := range config.SortedHostNames(cfg.Hosts) {
		if ctx.Err() != nil {
			return
		}
		if r := probeOne(ctx, probe, n, cfg.Hosts, cfg.SSHHostKeyVerification); r.Probed && !r.Reachable && ctx.Err() == nil {
			log.InfoLog.Printf("host %q readiness refresh: %s", n, r.Error)
		}
	}
}

// startHostReadinessRefresh re-probes the listed hosts on a jittered timer so
// `af doctor` and the pickers read a recent verdict without a create. It does
// nothing while no hosts are listed, and it shares CheckHost's per-host guard so
// it never stacks on an in-flight probe. An unreachable or incomplete probe
// records nothing (see session.CheckHost), so a blocked verdict is never
// overwritten by an unanswered one.
//
// ADVISORY, so deliberately NOT joined by the drain waitgroup (#5839). A probe
// can block on ssh for 30s, longer than the stopper's SIGKILL grace, and joining
// it put that wait in front of the final SaveInstancesForShutdown. It stops on
// the advisory scope (cancelled first thing in drainDaemon, which kills the ssh
// child) and on stopCh; if the process exits before it notices, nothing is lost —
// its only output is an in-memory cache.
//
// Because it outlives its starter, it reads the package seams (the interval and
// the probe) ONCE, here, and the goroutine uses only those copies: a later write
// to a seam, such as a test restoring it, cannot race with the loop. The
// returned channel closes when the loop has exited. It is NOT the drain
// waitgroup — shutdown never waits on it — and exists so a caller that wants to
// know the loop is gone (a test, before it restores state) can wait.
func startHostReadinessRefresh(manager *Manager, stopCh <-chan struct{}) <-chan struct{} {
	ctx := manager.advisoryContext()
	interval, probe := hostRefreshInterval, probeHostsFn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			// +/-10% jitter so many daemons do not hit a shared host together.
			d := interval + time.Duration(rand.Int63n(int64(interval/5))) - interval/10
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			refreshListedHosts(ctx, manager, probe)
		}
	}()
	return done
}
