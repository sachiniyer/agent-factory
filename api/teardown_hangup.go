package api

import (
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/internal/hangupshield"
)

// ignoreTeardownHangup holds SIGHUP non-terminating until the returned func
// restores the caller's previous disposition — see internal/hangupshield for
// why a Notify channel and not signal.Ignore (#5182). A remote-target teardown
// kills remote panes, never this caller's tty, so there is nothing to shield
// against and holding the signal would only swallow a real hangup.
func ignoreTeardownHangup() func() {
	if apiclient.IsRemoteTarget() {
		return func() {}
	}
	return hangupshield.Hold()
}
