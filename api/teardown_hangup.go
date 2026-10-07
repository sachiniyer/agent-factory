package api

import (
	"os"
	"os/signal"
	"syscall"
)

// ignoreTeardownHangup holds SIGHUP non-terminating until the returned func
// restores the caller's previous disposition.
//
// A teardown command issued from inside the session being destroyed runs in
// the pane's tty session: `tmux kill-session` closes the pane's pty and the
// kernel SIGHUPs every process in that session — this CLI included — while it
// is still blocked on the daemon's reply (#5182). The daemon spares the
// requester from its own reaper, but nothing on the daemon side can shield a
// caller from its own terminal dying; catching the hangup for the call's
// duration is what lets a controlling-tty caller live long enough to read the
// answer it asked for. A detached caller (an agent tool call, `setsid`) has no
// controlling terminal and nothing arrives to catch.
//
// The shield is a private Notify channel rather than signal.Ignore because
// Reset can only undo Notify — an Ignore'd signal stays ignored. The undo is
// signal.Stop, which faithfully restores the default disposition; a caller
// born under nohup already ignores SIGHUP and is left exactly as found.
func ignoreTeardownHangup() func() {
	if signal.Ignored(syscall.SIGHUP) {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	return func() { signal.Stop(ch) }
}
