package doctor

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// checkListenerPosture reports the #5137 listener/auth rows: the disk
// posture check (refused bind, opted-in exposure) and the running-vs-disk
// drift row for a daemon still enforcing a superseded posture. Live answers
// — the bound address and the responder's own token gate — are
// authoritative for what is serving NOW; the file is authoritative only for
// what the next start will do. Called from checkDaemonHealth so the rows land
// beside the daemon liveness check they interpret.
func checkListenerPosture(report *Report, h daemon.HealthStatus, cfg *config.Config) {
	// The #2090 exposure posture is a REFUSED bind since #5137: a non-loopback
	// listen_addr with network.require_token off and no explicit
	// allow_unauthenticated_network opt-in means the daemon starts but the TCP
	// listener does not — a configured web/API surface that is dead by policy is
	// a FAIL, not a warning, because the operator almost certainly meant for it
	// to be up. The three fixes are named verbatim from ListenerBindRefusal so
	// the row cannot drift from the startup log or the `af config set` refusal.
	//
	// The opted-in posture is a different row and stays a Warn: the listener is
	// bound and serving unauthenticated exactly as the operator asked, so it
	// never fails the run — it is the exposure made explicit, still worth saying
	// because the posture is real (problem=false: `af doctor` exits 0 on it).
	//
	// Reported independently of daemon liveness, unlike the old row: the posture
	// matters whether or not the daemon is up — refused means the next start
	// binds no listener, opted-in means the next start serves unauthenticated.
	// The separate daemon-config row compares this disk value with the posture
	// returned by the running daemon itself (#2168 Phase 4).
	//
	// cfg is nil when the config could not be loaded at all (doctor.Run passes
	// what it got). Say nothing then rather than guessing a posture — the load
	// failure has its own row, and inventing either answer here would be worse
	// than the silence.
	if cfg != nil {
		if refusal := config.ListenerBindRefusal(cfg); refusal != "" {
			switch {
			case h.PingErr == nil && h.Listeners.TCPBound:
				// A socket still BOUND under a refused disk posture is served by
				// the daemon's last-applied config, not the file: a build that
				// enforces the refusal retires the socket on apply, so bound
				// here means a pre-refusal daemon — or a hand-edit no apply has
				// reconciled. BootConfig carries that live posture (nil from a
				// responder that predates the field, which also predates the
				// refusal and is therefore serving the refused-looking disk
				// posture it loaded). Calling this row "refused" would be a
				// false safe conclusion against a live socket — report what is
				// actually serving instead.
				addr := h.Listeners.TCPBoundAddr
				if addr == "" {
					addr = h.Listeners.TCPListenAddr
				}
				// Classify the socket actually answering (addr), not the live
				// config's listen_addr: a retained socket after a failed rebind
				// deliberately diverges the two, so BootConfig can name loopback
				// while the socket still answers on the network address. A
				// responder that predates #5137 omits BootConfig entirely — the
				// unknown-auth assumption there must only fire for an address
				// the network can actually reach: a loopback-bound socket is
				// safe whatever its daemon enforces.
				liveTokened := h.BootConfig != nil && h.BootConfig.RequireToken
				if config.ListenerServesUnauthenticatedNetwork(addr, liveTokened) {
					report.Fail(sectionDaemon, "listener",
						fmt.Sprintf("the running daemon predates the unauthenticated-listener refusal (or has not "+
							"applied the refused file) and is still serving %s unauthenticated — af's full control "+
							"API, including DeliverPrompt, is reachable by anyone who can route to that address", addr),
						"restart the daemon so the listener is refused (`af daemon restart`), or close the exposure "+
							"now with `af config set network.require_token true` or `af config set "+
							"network.listen_addr 127.0.0.1:8443`")
				} else {
					// Loopback-bound needs no token to be safe; a network
					// bound socket is safe because its config requires one.
					safeWhy := "under its last-applied config that socket is safe"
					if config.IsLoopbackListenAddr(addr) {
						safeWhy = "it is bound to loopback, which nothing off-box can reach"
					}
					report.Warn(sectionDaemon, "listener",
						fmt.Sprintf("the running daemon still serves %s, but %s — the config on disk now refuses "+
							"the bind entirely", addr, safeWhy),
						"restart the daemon so the disk posture applies: af daemon restart", true)
				}
			default:
				report.Fail(sectionDaemon, "listener", refusal,
					"then restart the daemon so the fixed posture takes effect: af daemon restart")
			}
		} else if config.ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
			// The disk posture opts in, but whether anything is SERVING it is
			// the live socket's answer, not the file's: a pending restart can
			// leave the responder enforcing its last-applied safe config, and a
			// live refusal serves nothing at all (the drift row below owns the
			// refusal case). Classify the socket actually answering — the same
			// bound-address/token pair the refused-disk branch uses.
			switch {
			case h.PingErr == nil && h.Listeners.TCPBound:
				addr := h.Listeners.TCPBoundAddr
				if addr == "" {
					addr = h.Listeners.TCPListenAddr
				}
				// A responder that predates #5137 omits BootConfig — the field
				// did not exist — so the missing report reads as tokenless.
				liveTokened := h.BootConfig != nil && h.BootConfig.RequireToken
				if config.ListenerServesUnauthenticatedNetwork(addr, liveTokened) {
					report.Warn(sectionDaemon, "listener",
						fmt.Sprintf("network.allow_unauthenticated_network is set, and the running daemon serves %s from the network "+
							"with no authentication — the control API, including DeliverPrompt, is reachable by anyone who can "+
							"route to that address", addr),
						"if that is not what you want, run `af config set network.require_token true` to require a bearer token (`af token "+
							"show` prints it), `af config set network.listen_addr 127.0.0.1:8443` to serve this machine only, or "+
							"`af config set network.allow_unauthenticated_network false` to drop the opt-in",
						false)
				} else {
					// The opt-in is written but the responder still enforces
					// its last-applied safe posture — restart-pending drift,
					// not an active exposure.
					report.Warn(sectionDaemon, "listener",
						fmt.Sprintf("network.allow_unauthenticated_network is set, but the running daemon still serves %s under "+
							"its last-applied safe posture — nothing is exposed yet, and restarting exposes the control API "+
							"(including DeliverPrompt) with no authentication", addr),
						"if that is not what you want, run `af config set network.require_token true` or `af config set "+
							"network.allow_unauthenticated_network false` before restarting",
						true)
				}
			case h.PingErr == nil && h.Listeners.TCPRefusalReason != "":
				// Live refusal: nothing is serving — the running-vs-disk drift
				// row below owns the report for the unapplied opt-in.
			default:
				report.Warn(sectionDaemon, "listener",
					fmt.Sprintf("network.allow_unauthenticated_network is set, so network.listen_addr %q is reachable from the network "+
						"and serves the control API (including DeliverPrompt, which runs instructions through your agents) "+
						"with no authentication", cfg.ListenAddr),
					"if that is not what you want, run `af config set network.require_token true` to require a bearer token (`af token "+
						"show` prints it), `af config set network.listen_addr 127.0.0.1:8443` to serve this machine only, or "+
						"`af config set network.allow_unauthenticated_network false` to drop the opt-in",
					false)
			}
		}
	}
	// A refused RUNNING listener is a separate fact from the disk posture above:
	// the file may already carry the fix while the daemon still refuses, because
	// the apply that would rebind it has not run yet (a hand-edit is not an
	// apply). A Warn, not a second Fail — the disk check owns the verdict, this
	// row only says the running daemon has not caught up with it.
	if h.PingErr == nil && h.Listeners.TCPRefusalReason != "" && cfg != nil && config.ListenerBindRefusal(cfg) == "" {
		posture := ""
		if config.ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
			posture = ", and once restarted that opted-in listener serves the control API with no authentication"
		}
		report.Warn(sectionDaemon, "listener",
			fmt.Sprintf("the running daemon refused to bind the TCP listener on %s, but the config on disk no longer "+
				"triggers the refusal%s", h.Listeners.TCPListenAddr, posture),
			"restart the daemon so the listener binds with the fixed config: af daemon restart", true)
	}
}
