package proctree

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/afcli"
	"github.com/stretchr/testify/require"
)

// ArgvIsAFDaemon is the kill-path's positive daemon identification: af (or
// source-built agent-factory) argv[0] AND cobra's own decision that the
// argv reaches af's root RunE with --daemon (#7494 item 3, #7496 R1).
// Positional argv AFTER "--" is the root's own ignored tail — the daemon
// still runs — while a positional BEFORE "--" might name a subcommand the
// recognizer's root does not carry, so it is undecidable: not a "yes" here,
// but protected on the reap path, never refused. Both halves are required —
// a daemon-flagged non-af binary is not ours, and an af binary without the
// flag is a client.
func TestArgvIsAFDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"installed binary with the flag", []string{"/usr/local/bin/af", "--daemon"}, true},
		{"source-built binary with the flag", []string{"/repo/agent-factory", "--daemon"}, true},
		{"bare binary name with the flag", []string{"af", "--daemon"}, true},
		{"binary under a spaced path", []string{"/home/John Smith/.local/bin/af", "--daemon"}, true},
		{"flag as --daemon=true", []string{"af", "--daemon=true"}, true},
		{"other root flags before the daemon flag", []string{"af", "--program", "claude", "--daemon"}, true},
		{"shorthand value flag before the daemon flag", []string{"af", "-p", "claude", "--daemon"}, true},
		{"shorthand value attached to its flag", []string{"af", "-pclaude", "--daemon"}, true},
		{"shorthand value attached with =", []string{"af", "-p=claude", "--daemon"}, true},
		{"a false help shorthand keeps the claim", []string{"af", "-h=false", "--daemon"}, true},
		{"a false help shorthand after the claim", []string{"af", "--daemon", "-h=false"}, true},
		{"a bare help shorthand exits", []string{"af", "-h", "--daemon"}, false},
		{"a true help shorthand exits", []string{"af", "-h=true", "--daemon"}, false},
		{"a bad help shorthand bool exits", []string{"af", "-h=foo", "--daemon"}, false},
		{"a help shorthand in a cluster exits", []string{"af", "-hp", "claude", "--daemon"}, false},
		{"a false version shorthand keeps the claim", []string{"af", "-v=false", "--daemon"}, true},
		{"a bare version shorthand exits", []string{"af", "-v", "--daemon"}, false},
		{"a bad version shorthand bool exits", []string{"af", "-v=foo", "--daemon"}, false},
		{"remote flags before the daemon flag", []string{"af", "--daemon-url", "https://x", "--token", "t", "--daemon"}, true},
		{"upgrade probation keeps the daemon flag", []string{"af", "--upgrade-transaction", "tx1", "--daemon"}, true},
		{"the upgrade candidate's real spawn order", []string{"af", "--daemon", "--upgrade-transaction", "tx1"}, true},
		{"the last --daemon wins", []string{"af", "--daemon=false", "--daemon"}, true},
		{"flag as an unparseable form exits af", []string{"af", "--daemon=foo"}, false},
		{"--daemon=false is a client, explicitly", []string{"af", "--daemon=false"}, false},
		{"an af client without the flag", []string{"af", "sessions", "list"}, false},
		{"a positional may name a subcommand — undecidable, not ours", []string{"af", "x", "--daemon"}, false},
		{"a token after -- is the job's argv", []string{"af", "--", "--daemon"}, false},
		{"post-dash argv is the root's own ignored tail — the daemon runs", []string{"af", "--daemon", "--", "sh", "-c", "job", "--daemon"}, true},
		{"the lifeline's job flag is not af's", []string{"af", "__watch-lifeline", "v1", "0", "--", "sh", "-c", "job", "--daemon"}, false},
		{"the lifeline's flag before -- is still not af's", []string{"af", "__watch-lifeline", "--daemon"}, false},
		{"the recovery marker's flag is not af's", []string{"af", "__upgrade-recovery", "--daemon"}, false},
		{"a repeated flag's last value wins", []string{"af", "--daemon", "--help", "--help=false"}, true},
		{"last value wins for the daemon flag too", []string{"af", "--daemon", "--help", "--help=false", "--daemon=false"}, false},
		{"--help exits before the daemon runs", []string{"af", "--help", "--daemon"}, false},
		{"--version exits before the daemon runs", []string{"af", "--version", "--daemon"}, false},
		{"an unknown flag is not a live af", []string{"af", "--bogus", "--daemon"}, false},
		{"a missing flag value is not a live af", []string{"af", "--program", "--daemon"}, false},
		{"a bare terminator is a live daemon", []string{"af", "--daemon", "--"}, true},
		{"a non-af binary carrying the flag", []string{"/usr/bin/sleep", "--daemon"}, false},
		{"a substring flag is not the flag", []string{"af", "--daemonize"}, false},
		{"a substring flag with a value is not the flag", []string{"af", "--daemon-mode=strict"}, false},
		{"the daemon flag is not the daemon-url flag", []string{"af", "--daemon-url", "https://x"}, false},
		{"empty argv", nil, false},
		{"flag-shaped argv[0] is not the binary", []string{"--daemon", "af"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ArgvIsAFDaemon(tc.args))
		})
	}
}

// ArgvDaemonClaim is the reap path's tri-state: Yes is a decided daemon
// claim, No a decided refusal, and Undecidable the answer for argv this
// build cannot decide — a positional before "--" that may name a
// subcommand the recognizer's throwaway root does not carry (on the REAL
// root that positional either routes to a real verb — not a daemon — or
// falls through to the root RunE as stray text the daemon ignores), an
// unknown flag, or a malformed value. Undecidable must NEVER collapse to
// No on the reap path: `af --daemon -- x` reading Not is exactly how the
// previous pflag mirror unprotected a real daemon (#7496 R1).
func TestArgvDaemonClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want afcli.DaemonClaim
	}{
		{"the ordinary daemon", []string{"af", "--daemon"}, afcli.DaemonClaimYes},
		{"daemon with a post-dash tail runs", []string{"af", "--daemon", "--", "x"}, afcli.DaemonClaimYes},
		{"daemon with post-dash job argv runs", []string{"af", "--daemon", "--", "sh", "-c", "job", "--daemon"}, afcli.DaemonClaimYes},
		{"bare terminator keeps the claim", []string{"af", "--daemon", "--"}, afcli.DaemonClaimYes},
		{"false shorthand keeps the claim", []string{"af", "-h=false", "--daemon"}, afcli.DaemonClaimYes},
		{"help exits first — decided, not a daemon", []string{"af", "--help", "--daemon"}, afcli.DaemonClaimNo},
		{"version exits first — decided, not a daemon", []string{"af", "--version", "--daemon"}, afcli.DaemonClaimNo},
		{"explicit false is a decided client", []string{"af", "--daemon=false"}, afcli.DaemonClaimNo},
		{"post-dash daemon token is a positional — decided, not a daemon", []string{"af", "--", "--daemon"}, afcli.DaemonClaimNo},
		{"a client invocation is decided", []string{"af", "sessions", "list"}, afcli.DaemonClaimNo},
		{"a non-af binary is decided out first", []string{"/usr/bin/sleep", "--daemon"}, afcli.DaemonClaimNo},
		{"empty argv is decided out", nil, afcli.DaemonClaimNo},
		// #7496 R1: what the throwaway root cannot decide is Undecidable —
		// never No. A positional before "--" may name a subcommand the
		// recognizer does not carry; a flag this build does not know may be
		// a newer af's; a malformed bool exited THAT af but says nothing
		// about another build's. On the reap path all of these stay
		// protected and blocking.
		{"pre-dash positional could be a subcommand", []string{"af", "x", "--daemon"}, afcli.DaemonClaimUndecidable},
		{"the R1 argv: a positional wrapped in shorthand noise", []string{"af", "-h", "x", "-h=false", "--daemon"}, afcli.DaemonClaimUndecidable},
		{"the lifeline marker pre-empts cobra in main — can't tell", []string{"af", "__watch-lifeline", "--daemon"}, afcli.DaemonClaimUndecidable},
		{"the recovery marker likewise", []string{"af", "__upgrade-recovery", "--daemon"}, afcli.DaemonClaimUndecidable},
		{"an unknown flag may be a newer build's", []string{"af", "--bogus", "--daemon"}, afcli.DaemonClaimUndecidable},
		{"a malformed bool is undecidable, not refused", []string{"af", "--daemon=foo"}, afcli.DaemonClaimUndecidable},
		{"a missing flag value is undecidable", []string{"af", "--program"}, afcli.DaemonClaimUndecidable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ArgvDaemonClaim(tc.args))
			// The stop/kill paths read "ours" as the decided claim alone:
			// undecidable is never a yes to a signaller (#7496 R1).
			require.Equal(t, tc.want == afcli.DaemonClaimYes, ArgvIsAFDaemon(tc.args))
			require.Equal(t, tc.want == afcli.DaemonClaimYes, ArgvHasDaemonFlag(tc.args))
		})
	}
}
