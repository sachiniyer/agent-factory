package doctor

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// checkListenerPosture reports the #5137 listener/auth row: what the config on
// disk will do at the next daemon start. It is deliberately a DISK check — the
// file is authoritative for the next start, and a running daemon's own live
// listener state is already reported by the daemon-liveness rows beside it.
// Reconciling the two (a responder still serving a now-refused file, or still
// refusing under a now-fixed one) is follow-up (#5185).
//
// The #2090 exposure posture is a REFUSED bind since #5137: a non-loopback
// listen_addr with network.require_token off and no explicit
// allow_unauthenticated_network opt-in means the daemon starts but the TCP
// listener does not — a configured web/API surface that is dead by policy is
// a FAIL, not a warning, because the operator almost certainly meant for it
// to be up. The three fixes are named verbatim from ListenerBindRefusal so
// the row cannot drift from the startup log or the `af config set` refusal.
//
// The opted-in posture is a different answer and stays a Warn: the listener
// binds and serves unauthenticated exactly as the operator asked, so it never
// fails the run — it is the exposure made explicit, still worth saying because
// the posture is real (problem=false: `af doctor` exits 0 on it).
//
// Reported independently of daemon liveness: the posture matters whether or
// not the daemon is up — refused means the next start binds no listener,
// opted-in means the next start serves unauthenticated.
//
// cfg is nil when the config could not be loaded at all (doctor.Run passes
// what it got). Say nothing then rather than guessing a posture — the load
// failure has its own row, and inventing either answer here would be worse
// than the silence.
func checkListenerPosture(report *Report, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if refusal := config.ListenerBindRefusal(cfg); refusal != "" {
		report.Fail(sectionDaemon, "listener", refusal,
			"then restart the daemon so the fixed posture takes effect: af daemon restart")
		return
	}
	if config.ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
		// Unrefused unauthenticated-network serving is reachable ONLY through
		// the explicit opt-in — ListenerBindRefusal already returned, so this
		// posture exists because network.allow_unauthenticated_network is set.
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
