package config

import "fmt"

// ListenerServesUnauthenticatedNetwork reports whether cfg would serve the
// daemon's full control API to network peers with NO authentication — the
// #2090 exposure.
//
// The predicate is deliberately two-term, not three. A reader reaching for
// network.require_loopback_token as a third safety term will be wrong, and the mistake
// is not obvious, so it is spelled out here:
//
//	daemon.webListenerPolicy sets tokenDisabled = !RequireToken, and
//	tokenDisabled SHORT-CIRCUITS the gate — it overrides loopbackExempt
//	(daemon/tcpserver.go, daemon/httpauth.go). So while network.require_token is
//	false, NOTHING authenticates anyone: network.require_loopback_token only ever
//	withdraws an exemption that a disabled token already made irrelevant.
//	Treating network.require_loopback_token = true as making a network bind safe
//	would report a listener that is wide open as a listener that is fine.
//
// So on a non-loopback bind the one question that matters is whether the token
// is on. Loopback binds are exempt (nothing off-box can reach them — the
// same-host trust the unix socket already grants), and an empty network.listen_addr
// disables the web server outright, exposing nothing.
//
// The loopback test is IsLoopbackListenAddr, the SAME predicate the daemon's
// token gate derives its policy from. Two definitions of "is this loopback"
// drifting apart is how a security check rots, so there is only one.
//
// This predicate is the ONE definition of the exposure. Every surface that
// mentions it — the daemon's startup warning (daemon/tcpserver.go), `af config
// set` (exposureWarning), `af doctor`, `af daemon status` — asks it rather than
// re-deriving the answer.
func ListenerServesUnauthenticatedNetwork(listenAddr string, requireToken bool) bool {
	if listenAddr == "" {
		return false // web server disabled — nothing is served at all
	}
	return !requireToken && !IsLoopbackListenAddr(listenAddr)
}

// ListenerExposureNotice returns the one-line operator notice for a config that
// serves the control API unauthenticated on a network interface (#2090), or ""
// when the posture is safe.
//
// This posture is ALLOWED. #2090 originally made it a refusal — the daemon would
// not start — and #2168 reverses that by owner decision: "just allow binding to
// 0.0.0.0 without a token. Assume users are safe and will do the right thing."
// The exposure is real (the API this listener serves includes DeliverPrompt,
// which types instructions into a running agent and submits them, and an agent
// runs with the user's shell permissions), so it is still SAID — once, plainly,
// with the way to add auth. It is no longer decided on the user's behalf.
//
// The refusal also had a failure mode the warning does not: a config the daemon
// rejects on every attempt is not a transient failure, but the autostart unit's
// Restart=on-failure could not tell the difference, so a hand-edit to
// 0.0.0.0 + network.require_token = false crash-looped the unit indefinitely (#2168 §1.2).
// A warning cannot crash-loop.
//
// A string, not an error: every caller now reports it rather than acting on it,
// and an error return is an invitation to `if err != nil { return err }` — which
// is exactly the refusal being removed.
//
// Callers must emit this AT MOST ONCE per daemon start. It is deliberately not
// wired into any per-request or per-connection path: a warning repeated on every
// call is a warning nobody reads.
func ListenerExposureNotice(cfg *Config) string {
	if cfg == nil || !ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
		return ""
	}
	return fmt.Sprintf("network.listen_addr %q is reachable from the network and network.require_token is false, so af serves its "+
		"full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can "+
		"reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token "+
		"(`af token show` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only",
		cfg.ListenAddr)
}

// PreviewListenerExposureNotice returns the one-line operator notice for the
// web-tab preview listener (#1856) when network.preview_listen_addr binds a network
// interface, or "" when it is unset or loopback-only.
//
// This is deliberately NOT ListenerExposureNotice, and the difference is the
// point of the whole feature. That notice warns that the daemon's control API —
// DeliverPrompt and the rest — is exposed. The preview listener NEVER serves the
// control API: it is a separate origin that exists precisely so preview content
// cannot reach the SPA's token or the control plane. So it must never borrow the
// control-plane warning, which would be false and would train an operator to
// ignore the real one.
//
// It also does not gate on network.require_token. That key governs the control-plane
// listener's bearer token; the preview origin's own auth is a separate concern —
// each tab is served on its OWN unguessable hostname, and that hostname is the
// credential (#1856 step 3b), so the control listener's posture says nothing about
// who may read a preview.
//
// What the notice must say on a network bind is that binding one is pointless as
// well as exposed, and it must say BOTH halves. Per-tab origins live under
// *.localhost, which a browser resolves to ITS OWN loopback — so a remote viewer
// cannot reach a per-tab origin however the port is bound, and keeps the sandboxed
// same-origin preview served from network.listen_addr.
//
// The second half is the one that cost this a bug report (#3045). *.localhost is a
// browser CONVENTION, not a restriction on the port: anything that can reach the
// address can send `Host: <label>.localhost` itself, and on this listener that
// label is the only credential checked (there is no bearer token here — see the
// network.require_token note above). So a network bind turns every tab hostname into a
// network-reachable capability: a label that leaks through a log, a screenshot, or
// browser history stops being usable only from this machine.
//
// Saying only "a network bind gains nothing" is worse than saying nothing at all.
// The notice exists so an operator can make an informed choice, and one that
// understates the exposure converts an unexamined default into an examined and
// approved one.
//
// Same emit-at-most-once-per-daemon-start discipline as ListenerExposureNotice:
// a string, reported by the one startup site, never on a per-request path.
func PreviewListenerExposureNotice(cfg *Config) string {
	if cfg == nil || cfg.PreviewListenAddr == "" || IsLoopbackListenAddr(cfg.PreviewListenAddr) {
		return ""
	}
	return fmt.Sprintf("network.preview_listen_addr %q is reachable from the network · it is the web-tab preview origin, "+
		"kept separate from network.listen_addr so it never serves the daemon control API · it serves your previewed dev "+
		"servers, each on its own hostname · a remote browser gains nothing from the bind: per-tab origins are "+
		"*.localhost names, which a browser resolves to its own machine, so remote viewers keep the same-origin "+
		"preview on network.listen_addr · but *.localhost is a browser convention, not a restriction — anything that "+
		"reaches this address can send Host: <tab>.localhost itself, and a tab's hostname is the only credential "+
		"this listener checks, so a hostname that leaks through a log, a screenshot, or browser history becomes "+
		"readable from the network instead of only from this machine · editor tabs are withheld entirely while "+
		"this is network-bound · set it to a loopback address such as 127.0.0.1:8444, or \"\" to disable it",
		cfg.PreviewListenAddr)
}
