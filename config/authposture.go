package config

import "fmt"

// ListenerServesUnauthenticatedNetwork reports whether cfg would serve the
// daemon's full control API to network peers with NO authentication — the
// #2090 exposure.
//
// The predicate is deliberately two-term, not three. A reader reaching for
// require_loopback_token as a third safety term will be wrong, and the mistake
// is not obvious, so it is spelled out here:
//
//	daemon.webListenerPolicy sets tokenDisabled = !RequireToken, and
//	tokenDisabled SHORT-CIRCUITS the gate — it overrides loopbackExempt
//	(daemon/tcpserver.go, daemon/httpauth.go). So while require_token is
//	false, NOTHING authenticates anyone: require_loopback_token only ever
//	withdraws an exemption that a disabled token already made irrelevant.
//	Treating require_loopback_token = true as making a network bind safe
//	would report a listener that is wide open as a listener that is fine.
//
// So on a non-loopback bind the one question that matters is whether the token
// is on. Loopback binds are exempt (nothing off-box can reach them — the
// same-host trust the unix socket already grants), and an empty listen_addr
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
// 0.0.0.0 + require_token = false crash-looped the unit indefinitely (#2168 §1.2).
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
	return fmt.Sprintf("listen_addr %q is reachable from the network and require_token is false, so af serves its "+
		"full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can "+
		"reach that address, with no authentication and no TLS · set require_token = true to require a bearer token "+
		"(`af token show` prints it), or set listen_addr to 127.0.0.1:8443 to serve this machine only",
		cfg.ListenAddr)
}

// PreviewListenerExposureNotice returns the one-line operator notice for the
// web-tab preview listener (#1856) when preview_listen_addr binds a network
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
// It also does not gate on require_token. That key governs the control-plane
// listener's bearer token; the preview origin's own auth is a separate concern
// (a preview-scoped credential, wired in a later step). Today this listener
// serves NOTHING — the preview routes have not moved onto it yet — so the honest
// notice on a network bind is that the origin is reachable and currently inert,
// not a warning about content that is not served. It exists now so the posture
// is established at the seam and the later step only has to change the message,
// never discover it needs one.
//
// Same emit-at-most-once-per-daemon-start discipline as ListenerExposureNotice:
// a string, reported by the one startup site, never on a per-request path.
func PreviewListenerExposureNotice(cfg *Config) string {
	if cfg == nil || cfg.PreviewListenAddr == "" || IsLoopbackListenAddr(cfg.PreviewListenAddr) {
		return ""
	}
	return fmt.Sprintf("preview_listen_addr %q is reachable from the network · it is the web-tab preview origin, "+
		"kept separate from listen_addr so it never serves the daemon control API · it serves no content yet "+
		"(web-tab previews move onto it in a later step) · set it to a loopback address such as 127.0.0.1:8444, "+
		"or \"\" to disable it, if it should not be network-reachable",
		cfg.PreviewListenAddr)
}
