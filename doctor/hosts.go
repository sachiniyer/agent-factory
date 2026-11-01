package doctor

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// Per-host readiness (#5571 W2.3). For each host of the operator's global
// `hosts` list, doctor asks the DAEMON to probe it (daemon.CheckHost) — the same
// probe a create runs, which also refreshes the readiness cache the pickers
// read. Doctor does not dial the host itself: the daemon owns the ssh posture
// and the cache.
//
// Silent when no host is listed, so a local-only box sees no new output. An
// unreachable host or an unanswerable daemon is a WARN that says so, never a
// pass: "could not check" is not "ready".

// checkListedHosts reports each listed host's probe result.
func checkListedHosts(ctx *scanContext, report *Report, cfg *config.Config, health daemon.HealthStatus) {
	if cfg == nil || len(cfg.Hosts) == 0 {
		return
	}
	if health.PingErr != nil {
		report.Warn(sectionRemote, "hosts", fmt.Sprintf("%d listed host(s) were not probed: the daemon is not answering (%v)", len(cfg.Hosts), health.PingErr),
			"start the daemon with `af daemon start`, then rerun `af doctor`", false)
		report.markIncomplete("hosts")
		return
	}
	probe := ctx.opts.checkHosts
	if probe == nil {
		probe = daemon.CheckHost
	}
	resp, err := probe(daemon.CheckHostRequest{})
	if err != nil {
		report.Warn(sectionRemote, "hosts", fmt.Sprintf("listed hosts were not probed: %v", err),
			"if the daemon predates host checks, run `af daemon restart`, then rerun `af doctor`", false)
		report.markIncomplete("hosts")
		return
	}
	for _, h := range resp.Hosts {
		name := "host " + h.Name
		switch {
		case !h.Probed:
			// Another probe held the host's guard, so this run established nothing.
			// That is unknown, never a pass or a bare note, and a cached block is
			// surfaced rather than hidden behind it.
			report.Warn(sectionRemote, name, "unknown: a probe was already running, so this run did not check it"+cachedHostState(h.Readiness),
				"rerun `af doctor` once the running check finishes", false)
			report.markIncomplete("hosts")
		case !h.Reachable:
			detail := "could not be probed"
			if h.Error != "" {
				detail += ": " + h.Error
			}
			report.Warn(sectionRemote, name, detail,
				fmt.Sprintf("check that `ssh` to %q works non-interactively (keys, host-key trust, network), then rerun `af doctor`", h.Name), false)
			report.markIncomplete("hosts")
		case len(h.Problems) > 0:
			report.Warn(sectionRemote, name, "cannot run a session: "+strings.Join(h.Problems, "; "),
				"install what is missing on the host, then rerun `af doctor`", true)
		case !h.Complete:
			report.Warn(sectionRemote, name, "unknown: the host answered but not with a recognisable git, tmux and gh report"+cachedHostState(h.Readiness),
				fmt.Sprintf("check that a non-interactive `ssh` to %q prints no banner or extra output for a plain command, then rerun `af doctor`", h.Name), false)
			report.markIncomplete("hosts")
		default:
			detail := "ready (git, tmux and gh answered)"
			if len(h.Warnings) > 0 {
				detail = "ready · " + strings.Join(h.Warnings, "; ")
			}
			report.Pass(sectionRemote, name, detail)
		}
		reportPushCredentialBlockers(report, name, h.PushCredentialBlockers)
	}
	reportPushCredentialBlockers(report, "sandbox.ssh target", resp.SandboxPushCredentialBlockers)
}

// reportPushCredentialBlockers names each session that keeps a push-licensed
// target from getting push credentials (#5571 W7.8): while it runs there, no
// session on that machine is answered.
func reportPushCredentialBlockers(report *Report, target string, blockers []string) {
	for _, b := range blockers {
		report.Warn(sectionRemote, target+" push credentials", "withheld: "+b,
			"remove that session (`af sessions kill <id>`), then rerun `af doctor`", false)
	}
}

// cachedHostState words the daemon's cached verdict for an unknown probe, so a
// stale "blocked" is visible next to the "unknown".
func cachedHostState(readiness string) string {
	if readiness == "" {
		return ""
	}
	return "; cached state: " + readiness
}
