package config

import (
	"fmt"
	"strconv"
	"strings"
)

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
// drifting apart is how a security check rots, so there is only one. "Loopback"
// means exactly 127.0.0.0/8, ::1, or localhost: every wildcard, private, and
// Tailscale (100.64.0.0/10) address is NON-loopback — a tailnet is a network
// like any other and gets no special case (#5137).
//
// This predicate is the ONE definition of the exposure. Every surface that
// mentions it — the daemon's startup warning (daemon/listener_reload.go),
// `af config set` (exposureWarning), `af doctor`, `af daemon status` — asks it
// rather than re-deriving the answer.
func ListenerServesUnauthenticatedNetwork(listenAddr string, requireToken bool) bool {
	if listenAddr == "" {
		return false // web server disabled — nothing is served at all
	}
	return !requireToken && !IsLoopbackListenAddr(listenAddr)
}

// ListenerBindRefusal returns the reason the daemon REFUSES to bind cfg's
// control-plane TCP listener, or "" when the posture is allowed (#5137).
//
// The refusal is exactly the exposure predicate above plus one term: the
// operator's explicit opt-in, network.allow_unauthenticated_network. A
// non-loopback bind with the token off is therefore refused BY DEFAULT —
// #2168 Phase 0's warn-and-serve posture is reversed by owner decision, because
// a warning is not a boundary: a listener that answers DeliverPrompt
// unauthenticated runs instructions through the operator's agents for anyone
// who can route to the address.
//
// The refusal is scoped to the TCP listener only. The daemon itself still
// starts, and the unix control socket is unaffected, so the TUI, CLI, and
// sessions keep working — breaking the whole install over a network posture
// would be worse than the exposure (and is exactly what crash-looped the
// autostart unit under #2090's process-level refusal, #2168 §1.2). Returning
// the reason as a string rather than a bool keeps the message identical on
// every surface that reports it — the startup log line, the `af config set`
// refusal, the `af doctor` FAIL, and `af daemon status` — so the remediation
// can never drift between them.
func ListenerBindRefusal(cfg *Config) string {
	if cfg == nil || !ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) || cfg.AllowUnauthenticatedNetwork {
		return ""
	}
	return fmt.Sprintf("network.listen_addr %q is reachable from the network and network.require_token is false, so af's "+
		"full control API — including DeliverPrompt, which runs instructions through your agents — would be served to "+
		"anyone who can reach that address, with no authentication and no TLS · the TCP listener is refused; the daemon "+
		"itself still starts and the unix control socket still works, so the TUI, CLI, and sessions are unaffected · "+
		"fix one of: `af config set network.require_token true` to require a bearer token (`af token show` prints it), "+
		"`af config set network.listen_addr 127.0.0.1:8443` to serve this machine only, or `af config set "+
		"network.allow_unauthenticated_network true` to accept the risk explicitly", cfg.ListenAddr)
}

// ListenerPostureWriteExposure reports whether a `config set` of key=value
// could make a daemon SERVE the control API unauthenticated when the daemon
// performing the write predates the #5137 refusal — i.e. whether a remote
// write must take the guarded route that only refusal-capable daemons serve
// (apiclient.SetConfigValue). The refusal itself lives in the daemon's writer
// (listenerWriteRefusal); a daemon without it accepts the write and binds the
// listener this build declines to.
//
// The rule is inverted from "which keys can create the exposure": EVERY write
// an old daemon accepts ends in Manager.ApplyConfig, which loads the WHOLE
// file — so a config.toml already hand-edited into the refused posture binds
// the tokenless network listener on a default_program save just as surely as
// on the listen_addr write that created it. The client cannot read the remote
// file to distinguish, so the only writes kept on the plain route are the
// ones that force the listener posture safe by themselves — the token coming
// ON, or listen_addr going loopback/empty — because an old daemon applying
// the result can only bind safer than the file was. Everything else goes
// guarded and fails closed (404) on daemons that predate the refusal.
//
// Writes of network.allow_unauthenticated_network itself join the guarded
// default: the flag is meaningless to a pre-#5137 writer (rejected by its
// allowlist), but the apply it triggers is not — and the guarded route's 404
// names the upgrade path either way.
func ListenerPostureWriteExposure(key, value string) bool {
	switch canonicalConfigKey(key) {
	case "network.require_token":
		// Only an ON write forces the whole file safe — the apply binds
		// whatever listen_addr it holds under the token. Parse the way the
		// receiving writer does — canonicalizeScalar trims before ParseBool —
		// and treat an unparseable value as exposure-capable rather than
		// routing it around the guarded path.
		on, err := strconv.ParseBool(strings.TrimSpace(value))
		return err != nil || !on
	case "network.listen_addr":
		// Loopback or empty forces the listener safe regardless of the rest
		// of the file: nothing off-box can reach the bind at all.
		return value != "" && !IsLoopbackListenAddr(value)
	}
	return true
}

// ListenerPostureUnsetExposure is ListenerPostureWriteExposure for `config
// unset`, under the same inverted rule: the only unset that forces the
// listener safe is removing network.listen_addr, which restores the loopback
// default. Every other unset — require_token most of all — leaves the file's
// posture for the old daemon's whole-file apply to enforce.
func ListenerPostureUnsetExposure(key string) bool {
	return canonicalConfigKey(key) != "network.listen_addr"
}

// ListenerExposureNotice returns the one-line operator notice for a config that
// serves the control API unauthenticated on a network interface (#2090), or ""
// when the posture is safe.
//
// Since #5137 this posture is reached ONLY through the explicit opt-in,
// network.allow_unauthenticated_network — without it the TCP listener is
// refused outright (ListenerBindRefusal), so this notice never describes a bind
// the operator did not deliberately ask for. The notice still matters on the
// opted-in bind: the exposure is real (the API this listener serves includes
// DeliverPrompt, which types instructions into a running agent and submits
// them, and an agent runs with the user's shell permissions), so it is still
// SAID — once, plainly, with the way to add auth.
//
// A string, not an error: every caller reports it rather than acting on it.
// The acting-on-it decision lives in ListenerBindRefusal — keeping this a
// string is what stops it growing a second refusal channel.
//
// Callers must emit this AT MOST ONCE per daemon start. It is deliberately not
// wired into any per-request or per-connection path: a warning repeated on
// every call is a warning nobody reads.
func ListenerExposureNotice(cfg *Config) string {
	// A refused bind serves nothing, so "af serves its full control API" would
	// be a lie: the refusal reason (ListenerBindRefusal) is the notice for that
	// posture, and surfaces pick it first. Gate here too so a caller that
	// forgets can never print the exposure half of a refused bind.
	if cfg == nil || !ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) || ListenerBindRefusal(cfg) != "" {
		return ""
	}
	return fmt.Sprintf("network.listen_addr %q is reachable from the network and network.require_token is false, so af serves its "+
		"full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can "+
		"reach that address, with no authentication and no TLS · run `af config set network.require_token true` to require "+
		"a bearer token (`af token show` prints it), or `af config set network.listen_addr 127.0.0.1:8443` to serve this "+
		"machine only", cfg.ListenAddr)
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
// Emits on three channels: the bind-time daemon log, the apply-time
// transition warning, and the per-write warning on every exposed save of
// this key.
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
