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
			report.Info(sectionRemote, name, "a probe was already running; cached state: "+h.Readiness)
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
		default:
			detail := "ready (git, tmux and gh answered)"
			if len(h.Warnings) > 0 {
				detail = "ready · " + strings.Join(h.Warnings, "; ")
			}
			report.Pass(sectionRemote, name, detail)
		}
	}
}
