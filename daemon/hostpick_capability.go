package daemon

import "fmt"

// HostPickSurface names one wire field a host pick rides on and the Ping bit
// that says the daemon honors it there. A daemon that predates a field drops it
// silently — the session or task would then run on the repo's default backend, on
// a different machine than the one the user pinned it to — so EVERY client (CLI,
// TUI, web) checks the bit before it sends the field, and refuses when the daemon
// does not affirm it (#5571 W6.3).
type HostPickSurface struct {
	// Ignored says what an older daemon would do with a pick it drops.
	Ignored string
	// Bit reads the capability from a Ping reply.
	Bit func(PingResponse) bool
}

var (
	// HostPickOnCreate: CreateSessionRequest.Host.
	HostPickOnCreate = HostPickSurface{
		Ignored: "create the session on the repo's default backend",
		Bit:     func(r PingResponse) bool { return r.HostPick },
	}
	// HostPickOnDeliver: DeliverPromptRequest.Host (send-prompt --create).
	HostPickOnDeliver = HostPickSurface{
		Ignored: "create the session on the repo's default backend",
		Bit:     func(r PingResponse) bool { return r.DeliverHostPick },
	}
	// HostPickOnTask: Task.Host / TaskUpdate.Host.
	HostPickOnTask = HostPickSurface{
		Ignored: "store the task without a host, so every run would use the repo's default backend",
		Bit:     func(r PingResponse) bool { return r.TaskHost },
	}
)

// Require returns nil only when the daemon affirmed the capability. label is how
// the caller's surface names the pick ("--host" on the CLI, "the host pick" in
// the TUI). A Ping that could not be answered (pingErr) refuses too: "could not
// verify" must never mean "send it anyway".
func (s HostPickSurface) Require(label string, resp PingResponse, pingErr error, host string) error {
	if pingErr != nil {
		return fmt.Errorf("%s %q was not sent: could not confirm the daemon supports host picks: %w", label, host, pingErr)
	}
	if s.Bit(resp) {
		return nil
	}
	version := resp.Version
	if version == "" {
		version = "an unknown version that predates version reporting"
	} else {
		version = "v" + version
	}
	return fmt.Errorf("the daemon (%s) does not support %s here: it would ignore %q and %s. "+
		"Upgrade it and run `af daemon restart`, then retry; nothing was created or changed", version, label, host, s.Ignored)
}
