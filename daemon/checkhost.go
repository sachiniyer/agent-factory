package daemon

import (
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

func probeOne(name string, hosts map[string]config.HostConfig, posture string) HostCheckResult {
	res := HostCheckResult{Name: name}
	if !daemonHostProbes.tryAcquire(name) {
		res.Readiness = string(session.ReadHostReadiness(session.HostReadinessKey(name)).State)
		return res
	}
	defer daemonHostProbes.release(name)
	c, err := probeHostsFn(name, hosts, posture, session.HostCheckTimeout)
	res.Probed = true
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Reachable, res.Error, res.Problems, res.Warnings = c.Reachable, c.Error, c.Problems, c.Warnings
	}
	res.Readiness = string(session.ReadHostReadiness(session.HostReadinessKey(name)).State)
	return res
}

// CheckHost probes req.Host (or every listed host) and records each verdict in
// the readiness cache.
func (s *controlServer) CheckHost(req CheckHostRequest, resp *CheckHostResponse) error {
	// Blocked while the daemon is an upgrade candidate in probation: the probe
	// dials the operator's hosts and may learn host keys (see the ledger entry in
	// lifecycle_test.go). It needs no manager state, so only the admission gate.
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("cannot read the host list: %w", err)
	}
	name := strings.TrimSpace(req.Host)
	if name != "" {
		if err := config.ValidateHostPick(cfg.Hosts, name); err != nil {
			return err
		}
		resp.Hosts = []HostCheckResult{probeOne(name, cfg.Hosts, cfg.SSHHostKeyVerification)}
		return nil
	}
	for _, n := range config.SortedHostNames(cfg.Hosts) {
		resp.Hosts = append(resp.Hosts, probeOne(n, cfg.Hosts, cfg.SSHHostKeyVerification))
	}
	return nil
}

// hostRefreshInterval is how often the supervisor re-probes the listed hosts.
// Under hostReadinessFreshFor (15m) so a healthy host never reads stale.
const hostRefreshInterval = 10 * time.Minute

// startHostReadinessRefresh re-probes the listed hosts on a jittered timer so
// `af doctor` and the pickers read a recent verdict without a create. It does
// nothing while no hosts are listed, and it shares CheckHost's per-host guard so
// it never stacks on an in-flight probe. An unreachable or incomplete probe
// records nothing (see session.CheckHost), so a blocked verdict is never
// overwritten by an unanswered one.
func startHostReadinessRefresh(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// +/-10% jitter so many daemons do not hit a shared host together.
			d := hostRefreshInterval + time.Duration(rand.Int63n(int64(hostRefreshInterval/5))) - hostRefreshInterval/10
			select {
			case <-stopCh:
				return
			case <-time.After(d):
			}
			// A candidate in upgrade probation dials nothing and learns no host
			// keys, as the CheckHost RPC refuses for the same window.
			if manager.lifecycle != nil && manager.lifecycle.isUpgradeProbation() {
				continue
			}
			cfg, err := config.LoadConfig()
			if err != nil || len(cfg.Hosts) == 0 {
				continue
			}
			for _, n := range config.SortedHostNames(cfg.Hosts) {
				select {
				case <-stopCh:
					return
				default:
				}
				if r := probeOne(n, cfg.Hosts, cfg.SSHHostKeyVerification); r.Probed && !r.Reachable {
					log.InfoLog.Printf("host %q readiness refresh: %s", n, r.Error)
				}
			}
		}
	}()
}
