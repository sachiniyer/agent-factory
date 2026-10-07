package api

import (
	"github.com/sachiniyer/agent-factory/internal/hangupshield"
)

// ignoreTeardownHangup holds SIGHUP non-terminating until the returned func
// restores the caller's previous disposition — see internal/hangupshield for
// why a Notify channel and not signal.Ignore (#5182).
func ignoreTeardownHangup() func() {
	return hangupshield.Hold()
}
