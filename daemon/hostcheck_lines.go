package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The ONE rendering of a host probe verdict (#5571 W10.5). Every surface that
// can run CheckHost shows the same arms: busy → unreachable → blocked-with-
// problems → answered-unrecognisably → ready, with the operator-owned remedy
// under the first three. The copy used to live in commands/hostscmd.go as
// renderHostCheck; the web mirrors it in TypeScript because it cannot share Go
// (web/src/hosts.ts hostCheckLines). The TUI's Hosts section made a second Go
// consumer, so the arms moved here — two Go copies of one verdict is how the
// surfaces come to disagree about what "ready" means.
//
// The parameters carry the only things the surfaces legitimately phrase
// differently: who ran the probe (origin — "this machine" locally, "the daemon
// host" when the TUI administers a remote daemon), and how to re-run it
// (retry — "run `af hosts check <name>`" on the CLI, "check it again" on a UI
// whose verb sits under the row). Which arm fires, the problems list, the ssh
// remedy argv and the opens-no-tunnel sentence are shared.

// HostCheckLines renders r as display lines, one string per line, already
// indented for a listing (`  name:` / `    fix:`). entry is the saved entry the
// probe ran against and globalPosture the daemon's ssh.host_key_verification —
// the two inputs the ssh remedy is composed from.
func HostCheckLines(r HostCheckResult, entry config.HostConfig, globalPosture, origin, retry string) []string {
	var lines []string
	switch {
	case !r.Probed:
		lines = append(lines, fmt.Sprintf("  %s: another probe is already running (cached state: %s); rerun in a moment", r.Name, r.Readiness))
	case !r.Reachable:
		line := fmt.Sprintf("  %s: could not be probed", r.Name)
		if r.Error != "" {
			line += ": " + r.Error
		}
		lines = append(lines, line,
			fmt.Sprintf("    fix: make `%s` succeed from %s (bring the tailnet or VPN up, load your key, trust the host key), then %s. af opens no tunnel and edits no ssh config.", HostRemedyCommand(entry, globalPosture), origin, retry))
	case len(r.Problems) > 0:
		lines = append(lines, fmt.Sprintf("  %s: cannot run a session yet", r.Name))
		for _, p := range r.Problems {
			lines = append(lines, "    fix: "+p)
		}
		lines = append(lines, "    then "+retry)
	case !r.Complete:
		// The host answered something af could not read (truncated/garbled
		// output, banner or profile noise on a probe line, a broken tmux -V).
		// That is unknown, never ready — the same arm doctor renders (#5841).
		line := fmt.Sprintf("  %s: unknown: the host answered but not with a recognisable git, tmux and gh report", r.Name)
		if r.Readiness != "" {
			line += "; cached state: " + r.Readiness
		}
		lines = append(lines, line,
			fmt.Sprintf("    fix: check that a non-interactive `%s` prints no banner or extra output for a plain command, then %s", HostRemedyCommand(entry, globalPosture), retry))
	default:
		lines = append(lines, fmt.Sprintf("  %s: ready (git, tmux and gh answered)", r.Name))
	}
	for _, warn := range r.Warnings {
		lines = append(lines, "    note: "+warn)
	}
	return lines
}

// HostRemedyCommand is the command a host verdict prints for the operator to
// test the connection: af's own ssh invocation for the entry
// (session.OperatorSSHCommand), so every shape the list accepts — a bare host,
// a user, a custom port, a key — is valid as printed. Never `ssh
// user@host:port`, which OpenSSH reads as the hostname "host:port" on port 22
// (#5852).
func HostRemedyCommand(entry config.HostConfig, globalPosture string) string {
	cmd, err := session.OperatorSSHCommand(entry, globalPosture)
	if err != nil {
		// An entry that cannot compose cannot be saved either (ValidateHostEntry).
		return "ssh <the saved host> true"
	}
	return cmd + " true"
}

// HostCheckReady reports a probed host that answered recognisably (git, tmux
// and gh each gave a recognised answer) with no blocking problem. A reachable
// host whose probe output was garbled/truncated has Complete false and is
// unknown, not ready — the same contract doctor honors (#5841). Absent from an
// older daemon's reply, Complete decodes false, so it too reads as not ready.
func HostCheckReady(r HostCheckResult) bool {
	return r.Probed && r.Reachable && r.Complete && len(r.Problems) == 0
}
