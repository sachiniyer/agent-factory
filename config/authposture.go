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
//	would leave the listener wide open under a config that reads secure.
//
// So on a non-loopback bind the one question that matters is whether the token
// is on. Loopback binds are exempt (nothing off-box can reach them — the
// same-host trust the unix socket already grants), and an empty listen_addr
// disables the web server outright, exposing nothing.
//
// The loopback test is IsLoopbackListenAddr, the SAME predicate the daemon's
// token gate derives its policy from. Two definitions of "is this loopback"
// drifting apart is how a security check rots, so there is only one.
func ListenerServesUnauthenticatedNetwork(listenAddr string, requireToken bool) bool {
	if listenAddr == "" {
		return false // web server disabled — nothing is served at all
	}
	return !requireToken && !IsLoopbackListenAddr(listenAddr)
}

// ValidateListenerAuthPosture returns a non-nil error when cfg would serve the
// control API unauthenticated on a network interface (#2090). The daemon calls
// it as a REFUSAL — it will not start in this configuration — so the message is
// written for someone who just got stopped and needs to choose a way forward.
//
// Why refuse rather than warn: the API this listener exposes includes
// DeliverPrompt, which types instructions into a running agent and submits
// them. An agent runs with the user's shell permissions, so an unauthenticated
// network listener is remote code execution against everyone who can route to
// it. A log line (which is all this used to be — daemon/httpserver.go) is not a
// proportionate response to that, and it scrolled past unread on the box that
// filed #2090.
//
// Why not auto-correct to loopback instead: silently overriding a listen_addr
// the operator explicitly set would break the deliberate "serve the web UI to
// my LAN/Tailscale" setup with no signal, trading a security bug for a
// availability one. Refusing keeps the operator in control of which of the two
// safe postures they want, which is also what makes this survivable as an
// upgrade: the config that used to boot now stops with instructions.
func ValidateListenerAuthPosture(cfg *Config) error {
	if cfg == nil || !ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
		return nil
	}
	return fmt.Errorf("refusing to start: listen_addr %q is reachable from the network but require_token is false, "+
		"so af would serve its full control API — including DeliverPrompt, which runs arbitrary instructions "+
		"through your agents — to anyone who can reach that address, with no authentication and no TLS.\n\n"+
		"Choose one:\n"+
		"  af config set require_token true            keep the network bind, require a bearer token (`af token` prints it)\n"+
		"  af config set listen_addr 127.0.0.1:8443    serve the web UI to this machine only\n"+
		"  af config set listen_addr \"\"                turn the web server off entirely",
		cfg.ListenAddr)
}
