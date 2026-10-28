package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// CheckHost: probing a LISTED host on demand (#5571 W2.3).
//
// The create-time probe is the only thing that ever fills the readiness cache,
// so a host nobody has created on reads "unprobed" forever. CheckHost is the
// same one-round-trip probe, run against a named host of the operator's global
// list, so `af doctor`, the supervisor and a future `af hosts add` can learn a
// host's state without creating anything on it.
//
// It dials only what the operator listed: a name outside the list is refused
// with config.ValidateHostPick before any ssh command exists, so no caller —
// including a caller of the daemon RPC — can aim af at an arbitrary machine.

// HostReadinessKey is the readiness-cache key for a listed host. Listed hosts
// are keyed by name, not by composed ssh command: the command carries per-create
// pins that vary, the name is what every picker already holds.
func HostReadinessKey(name string) string { return "host:" + strings.TrimSpace(name) }

// HostCheckTimeout bounds one CheckHost probe.
const HostCheckTimeout = sshShortStepTimeout

// HostCheck is one CheckHost outcome.
type HostCheck struct {
	// Reachable is false when the probe could not run (ssh failed or timed out).
	// Nothing is recorded in that case: an unanswered probe says nothing about
	// the host's tools.
	Reachable bool
	// Error is why the probe could not run; empty when Reachable.
	Error string
	// Problems block a session (a missing git or tmux, an old tmux).
	Problems []string
	// Warnings never block.
	Warnings []string
	// Complete is true when git, tmux and gh each gave a recognised answer.
	Complete bool
}

// Blocked reports whether the host answered and cannot run a session.
func (c HostCheck) Blocked() bool { return c.Reachable && len(c.Problems) > 0 }

// hostCheckShell builds the shell a probe of h runs through. A seam for tests.
var hostCheckShell = func(ctx context.Context, h config.HostConfig, globalPosture string) (sandboxShell, error) {
	cfg := sshConfigForHost(h)
	posture := hostPosture(h, globalPosture)
	if err := verifySSHIdentityFile(cfg); err != nil {
		return nil, err
	}
	cmd, err := sshCommandForHost(h, globalPosture, "", 0)
	if err != nil {
		return nil, err
	}
	if !h.UseSSHConfig {
		if err := prepareSSHHostKeyStore(cfg, posture); err != nil {
			return nil, err
		}
	}
	p := newSSHSandboxProvisioner(ProvisionSpec{}, cmd, "", "")
	p.runCtx = ctx
	return p, nil
}

// CheckHost probes the listed host name and records the verdict under
// HostReadinessKey(name) by the same rule the create path uses. An unlisted or
// malformed name is an error before anything is dialed. Cancelling ctx kills the
// ssh child; a probe abandoned that way records nothing.
func CheckHost(ctx context.Context, name string, hosts map[string]config.HostConfig, globalPosture string, timeout time.Duration) (HostCheck, error) {
	name = strings.TrimSpace(name)
	if err := config.ValidateHostPick(hosts, name); err != nil {
		return HostCheck{}, err
	}
	entry := hosts[name]
	if err := config.ValidateHostEntry(name, entry); err != nil {
		return HostCheck{}, err
	}
	shell, err := hostCheckShell(ctx, entry, globalPosture)
	if err != nil {
		return HostCheck{}, fmt.Errorf("host %q: %w", name, err)
	}
	report, err := runHostPrereqProbe(shell, timeout)
	if err != nil {
		return HostCheck{Error: err.Error()}, nil
	}
	_ = recordHostPrereqVerdict(HostReadinessKey(name), report)
	return HostCheck{
		Reachable: true,
		Problems:  report.Problems,
		Warnings:  report.Warnings,
		Complete:  report.Complete,
	}, nil
}

// CheckAllHosts probes every listed host in name order, skipping the names in
// skip. It returns each host's result; a refusal for one host does not stop the
// others.
func CheckAllHosts(ctx context.Context, hosts map[string]config.HostConfig, globalPosture string, timeout time.Duration, skip func(name string) bool) (map[string]HostCheck, map[string]error) {
	out := map[string]HostCheck{}
	errs := map[string]error{}
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if skip != nil && skip(name) {
			continue
		}
		c, err := CheckHost(ctx, name, hosts, globalPosture, timeout)
		if err != nil {
			errs[name] = err
			continue
		}
		out[name] = c
	}
	return out, errs
}
