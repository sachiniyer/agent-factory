package proctree

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ArgvIsAFDaemon is the kill-path's positive daemon identification: af (or
// source-built agent-factory) argv[0] AND a discrete --daemon flag. Both
// halves are required — a daemon-flagged non-af binary is not ours, and an af
// binary without the flag is a client.
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
		{"flag as an unparseable form", []string{"af", "--daemon=foo"}, true},
		{"--daemon=false is a client, explicitly", []string{"af", "--daemon=false"}, false},
		{"an af client without the flag", []string{"af", "sessions", "list"}, false},
		{"a non-af binary carrying the flag", []string{"/usr/bin/sleep", "--daemon"}, false},
		{"a substring flag is not the flag", []string{"af", "--daemonize"}, false},
		{"a substring flag with a value is not the flag", []string{"af", "--daemon-mode=strict"}, false},
		{"empty argv", nil, false},
		{"flag-shaped argv[0] is not the binary", []string{"--daemon", "af"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ArgvIsAFDaemon(tc.args))
		})
	}
}
