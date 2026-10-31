package daemon

import (
	"fmt"
	"strings"
)

// WebListenerUnbound reports whether the daemon is configured to serve the web
// UI/API on a TCP address but a bind has FAILED (typically "address already in
// use"), so nothing is serving it. A listener that has simply not bound yet has
// no BindError and is not reported, and a listener the daemon deliberately
// refused (TCPRefusalReason) has its own, louder row.
func (s DaemonListenerStatus) WebListenerUnbound() bool {
	return s.TCPConfigured && !s.TCPBound && s.TCPRefusalReason == "" && s.TCPBindError != ""
}

// WebListenerRemedy is the one remedy sentence every surface (af doctor, the
// TUI) shows beside WebListenerUnbound.
const WebListenerRemedy = "free that port (another process or another AGENT_FACTORY_HOME's daemon holds it), " +
	"or pick a free address with `af config set network.listen_addr 127.0.0.1:<port>`"

// WebListenerNotice is the one-line description of an unbound web listener, or
// "" when there is nothing to report. It names the configured address and the
// bind error, so the user is not left browsing to a URL that is not served.
func (s DaemonListenerStatus) WebListenerNotice() string {
	if !s.WebListenerUnbound() {
		return ""
	}
	note := fmt.Sprintf("web UI not served: %s is not bound (%s)", s.TCPListenAddr, bindReason(s.TCPBindError))
	if s.BindRetrying {
		note += "; the daemon keeps retrying"
	}
	return note
}

// bindReason is the part of a bind error a reader acts on. The daemon wraps the
// kernel's message in several layers of context, which would push the cause off
// a one-line surface; `af daemon status` still prints the full error.
func bindReason(bindErr string) string {
	for _, known := range []string{
		"address already in use",
		"permission denied",
		"cannot assign requested address",
	} {
		if strings.Contains(bindErr, known) {
			return known
		}
	}
	return bindErr
}
