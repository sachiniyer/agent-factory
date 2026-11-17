package proctree

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ArgvIsAFDaemon is the kill-path's positive daemon identification: af (or
// source-built agent-factory) argv[0] AND the --daemon flag where af's own
// root parser puts it (#7494 item 3) — a flags-only zone, no token past "--",
// no positional. Both halves are required — a daemon-flagged non-af binary is
// not ours, and an af binary without the flag is a client.
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
		{"a positional makes the flag the job's", []string{"af", "x", "--daemon"}, false},
		{"a token after -- is the job's argv", []string{"af", "--", "--daemon"}, false},
		{"a daemon flag then job argv", []string{"af", "--daemon", "--", "sh", "-c", "job", "--daemon"}, false},
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
