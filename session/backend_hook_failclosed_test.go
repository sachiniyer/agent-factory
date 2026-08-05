package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2845: whether a stdout line is the endpoint record or a tunnel's log is
// undecidable while both share the stream. af refuses rather than guessing,
// because guessing "prose" on a record dials a URL and bearer token taken from
// a log line.

// Both pairs from #2845 must REFUSE — that is the point: they are the inputs no
// rule can classify, so neither member may be resolved by picking.
func TestHookLaunchRefusesAmbiguousStdout(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{"pair 1: open log array", []string{`[INVALID,`, `{"url":"http://wrong.invalid","token":"logged-secret"}`}},
		{"pair 1: bracket prose with open brace", []string{`[INFO] opening {config`}},
		{"pair 2: separator on its own line", []string{`{"url":"http://wrong.invalid","token":"logged-secret"}`, `,"endpoint":`}},
		{"pair 2: closer on its own line", []string{`{"url":"http://wrong.invalid","token":"logged-secret"}`, `] tunnel closed`}},
		{"open object record", []string{`{"level":INVALID,"endpoint":`, `{"url":"http://wrong.invalid","token":"logged-secret"}`}},
		{"balanced malformed then continuation", []string{`{level:INVALID}`, `,"endpoint":`, `{"url":"http://wrong.invalid","token":"logged-secret"}`}},
		{"trailing opener that never closes", []string{`{"level":"info"} {"level":INVALID,"endpoint":`, `{"url":"http://wrong.invalid","token":"logged-secret"}`}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := "printf '%s\\n'"
			for _, line := range test.lines {
				script += " '" + line + "'"
			}
			script += "\n" + `echo '{"url":"http://10.0.0.7:8080","token":"secret"}'` + "\nexit 0\n"
			h := newHookState(t, script, "")

			_, err := newHookProvisioner(h, "ambiguous "+test.name).provisionOrReap()
			require.Error(t, err, "ambiguous stdout must be refused, never resolved by guessing")

			// Actionable: name the line, and give the one-line fix.
			assert.Contains(t, err.Error(), "af will not guess between them")
			assert.Contains(t, err.Error(), ">/dev/null 2>&1",
				"the refusal must tell the operator how to fix it")
			assert.Contains(t, err.Error(), "docs/remote-hooks.md")
			assert.NotContains(t, err.Error(), "logged-secret",
				"the refusal must not leak a token from the ambiguous output")
		})
	}
}

// Refusing must stay narrow: an unambiguous launch is untouched, including one
// whose tunnel shares stdout with well-formed chatter.
func TestHookLaunchStillAcceptsUnambiguousStdout(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{"endpoint alone", []string{`{"url":"http://10.0.0.7:8080","token":"secret"}`}},
		{"tunnel prose around it", []string{`tunnel forwarding`, `{"url":"http://10.0.0.7:8080","token":"secret"}`, `tunnel still forwarding`}},
		{"bracketed log prefixes", []string{`[INFO] tunnel forwarding`, `[WARN] retrying`, `{"url":"http://10.0.0.7:8080","token":"secret"}`}},
		{"numeric bracket prefix", []string{`[2026] tunnel forwarding`, `{"url":"http://10.0.0.7:8080","token":"secret"}`}},
		{"self-contained log record", []string{`{"level":"info","msg":"up"}`, `{"url":"http://10.0.0.7:8080","token":"secret"}`}},
		{"balanced malformed line", []string{`{config} loaded`, `{"url":"http://10.0.0.7:8080","token":"secret"}`}},
		{"indented endpoint", []string{`  {"url":"http://10.0.0.7:8080","token":"secret"}`}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := "printf '%s\\n'"
			for _, line := range test.lines {
				script += " '" + line + "'"
			}
			script += "\nexit 0\n"
			h := newHookState(t, script, "")

			res, err := newHookProvisioner(h, "clear "+test.name).provisionOrReap()
			require.NoError(t, err, "an unambiguous launch must not be refused")
			require.NotNil(t, res.Endpoint)
			assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
			assert.Equal(t, "secret", res.Endpoint.Token)
			assert.False(t, h.deleteRan(t), "a working sandbox must not be reaped")
		})
	}
}

// A pretty-printed endpoint spans lines and is still unambiguous.
func TestHookLaunchAcceptsPrettyPrintedEndpointUnderFailClosed(t *testing.T) {
	h := newHookState(t, `
printf '%s\n' '{' '  "url": "http://10.0.0.7:8080",' '  "token": "secret"' '}'
exit 0
`, "")
	res, err := newHookProvisioner(h, "pretty").provisionOrReap()
	require.NoError(t, err)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
}

// The refusal names the offending line so the operator knows what to redirect.
func TestHookLaunchRefusalNamesTheAmbiguousLine(t *testing.T) {
	h := newHookState(t, `
echo '[INFO] opening {config'
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	_, err := newHookProvisioner(h, "named line").provisionOrReap()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[INFO] opening {config", "the refusal must quote the line it could not classify")
	assert.True(t, strings.Contains(err.Error(), "launch_cmd"), "and name the hook that printed it")
}
