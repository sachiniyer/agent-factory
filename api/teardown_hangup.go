package api

import (
	"github.com/sachiniyer/agent-factory/internal/hangupshield"
)

// ignoreTeardownHangup holds SIGHUP non-terminating until the returned func
// restores the caller's previous disposition — see internal/hangupshield for
// why a Notify channel and not signal.Ignore (#5182). The CLI's kill and
// archive writes always go over the LOCAL control socket — they do not honor
// --daemon-url (Codex on #5186) — so each teardown this wraps can genuinely
// hang up the caller's own tty and the shield applies regardless of
// remote-target configuration.
func ignoreTeardownHangup() func() {
	return hangupshield.Hold()
}
