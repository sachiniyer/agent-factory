package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2845, resolved by schema: launch_cmd's stdout carries the {"url","token"}
// endpoint record and nothing else.
//
// docs/remote-hooks.md used to let a backgrounded tunnel inherit the stream and
// keep logging into it. Under that contract "is this line the endpoint or a
// log?" was UNDECIDABLE — #2845 exhibits two input pairs identical on every
// property a classifier can inspect that require opposite handling, after seven
// rules each closed one counterexample and admitted the next. Reserving stdout
// deletes the question: the endpoint is a parse now, not a guess.
//
// This is a BREAKING change for a script whose tunnel shares stdout, so the
// refusal has to be worth meeting at 2am — it names the stream, quotes what was
// on it, and gives the redirect.

const contractEndpoint = `{"url":"http://10.0.0.7:8080","token":"secret"}`

// Everything that is not the endpoint record is REFUSED, whichever side of the
// record it sits on. Every case here LAUNCHES on master: that is the breaking
// change, stated as a test.
func TestHookLaunchRefusesStdoutBeyondTheEndpoint(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		// quoted is the text the error's first line must show the operator.
		quoted string
	}{
		{
			name:   "prose before the endpoint",
			lines:  []string{`tunnel forwarding`, contractEndpoint},
			quoted: "tunnel forwarding",
		},
		{
			name:   "prose after the endpoint",
			lines:  []string{contractEndpoint, `tunnel closed`},
			quoted: "tunnel closed",
		},
		{
			name:   "bracketed log prefix",
			lines:  []string{`[INFO] tunnel forwarding`, contractEndpoint},
			quoted: "[INFO] tunnel forwarding",
		},
		{
			name:   "log record before the endpoint",
			lines:  []string{`{"level":"info","token":"logged-secret"}`, contractEndpoint},
			quoted: `{"level":"info"`,
		},
		{
			name:   "log record after the endpoint",
			lines:  []string{contractEndpoint, `{"level":"info","token":"logged-secret"}`},
			quoted: `{"level":"info"`,
		},
		{
			name:   "a second endpoint-shaped object",
			lines:  []string{contractEndpoint, `{"url":"http://wrong.invalid","token":"logged-secret"}`},
			quoted: "http://wrong.invalid",
		},
		{
			// The excerpt must survive a record that spans lines: quoting its first
			// physical line would show the operator a bare `{`.
			name:   "pretty-printed log record before the endpoint",
			lines:  []string{`{`, `  "level": "info",`, `  "token": "logged-secret"`, `}`, contractEndpoint},
			quoted: `{"level":"info"`,
		},
		{
			name:   "balanced malformed line",
			lines:  []string{`{config} loaded`, contractEndpoint},
			quoted: "{config} loaded",
		},
		{
			name:   "trailing text on the endpoint's own line",
			lines:  []string{contractEndpoint + ` started`},
			quoted: "started",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHookState(t, hookPrintfScript(test.lines), "")

			_, err := newHookProvisioner(h, "contract "+test.name).provisionOrReap()
			require.Error(t, err, "stdout beyond the endpoint record must be refused")

			headline, _, _ := strings.Cut(err.Error(), "\n")
			assert.Contains(t, headline, "printed something other than its endpoint on stdout",
				"the refusal must say the contract was violated")
			assert.Contains(t, headline, test.quoted,
				"and quote what was on stdout that was not the endpoint record")
			assert.Contains(t, err.Error(), ">/dev/null 2>&1", "and give the one-line remedy")
			assert.Contains(t, err.Error(), "write progress to stderr", "and name the stream to move logging to")
			assert.Contains(t, err.Error(), "docs/remote-hooks.md")
			assert.NotContains(t, err.Error(), "logged-secret",
				"the refusal must not leak a token out of the offending output")

			assert.True(t, h.deleteRan(t),
				"a refused launch may have provisioned, so it must still be reaped")
		})
	}
}

// The refusal stays narrow: an endpoint-only stdout still launches, in every
// spelling a shell produces. Whitespace is not another writer on the stream.
func TestHookLaunchAcceptsEndpointOnlyStdout(t *testing.T) {
	tests := []struct{ name, script string }{
		{"one line", "echo '" + contractEndpoint + "'\n"},
		{"indented", "printf '%s\\n' '  " + contractEndpoint + "'\n"},
		{"no trailing newline", "printf '%s' '" + contractEndpoint + "'\n"},
		{"trailing blank lines", "printf '%s\\n\\n\\n' '" + contractEndpoint + "'\n"},
		{"leading blank line", "printf '\\n%s\\n' '" + contractEndpoint + "'\n"},
		{"crlf line ending", "printf '%s\\r\\n' '" + contractEndpoint + "'\n"},
		{
			"pretty printed",
			"printf '%s\\n' '{' '  \"url\": \"http://10.0.0.7:8080\",' '  \"token\": \"secret\"' '}'\n",
		},
		{
			// stderr is UNCHANGED: it is still the script's narrative, and a tunnel
			// may still inherit it. Only stdout became exclusive.
			"stderr may still carry anything",
			"echo '[INFO] tunnel forwarding' >&2\necho '" + contractEndpoint + "'\necho '[INFO] done' >&2\n",
		},
		{
			// The legacy field is still accepted-and-ignored, so an old script that
			// kept it does not meet a contract error on top of the migration.
			"legacy tls_fingerprint field",
			`echo '{"url":"http://10.0.0.7:8080","token":"secret","tls_fingerprint":"ab:cd"}'` + "\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHookState(t, test.script+"exit 0\n", "")

			res, err := newHookProvisioner(h, "endpoint only "+test.name).provisionOrReap()
			require.NoError(t, err, "an endpoint-only stdout must still launch")
			require.NotNil(t, res.Endpoint)
			assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
			assert.Equal(t, "secret", res.Endpoint.Token)
			assert.False(t, h.deleteRan(t), "a working sandbox must not be reaped")
		})
	}
}

// Every stdout shape that survived a review round on #2718 or #2841 is pinned
// here, by value, as a REFUSAL. Each one used to need its own rule about where a
// record ends and a log begins; none of them is endpoint-only stdout, so the
// parse rejects the lot for one reason.
//
// The security direction is what these inputs are really about: the logged URL
// and token must never become the endpoint af dials.
func TestHookLaunchRefusesEveryHistoricalSharedStdout(t *testing.T) {
	const loggedEndpoint = `{"url":"http://wrong.invalid","token":"logged-secret"}`

	tests := []struct {
		name  string
		lines []string
	}{
		{"nested endpoint in a malformed log", []string{`{"level":INVALID,"endpoint":` + loggedEndpoint + `}`, contractEndpoint}},
		{"nested endpoint with no real record", []string{`{"level":INVALID,"endpoint":` + loggedEndpoint + `}`}},
		{"nested endpoint after a mismatched closer", []string{`{"level":],"endpoint":` + loggedEndpoint + `}`, contractEndpoint}},
		{"unterminated log: doubled brace", []string{`progress {{`, contractEndpoint}},
		{"unterminated log: partial record", []string{`partial {"level":`, contractEndpoint}},
		{"unterminated log: open bracket", []string{`noise [`, contractEndpoint}},
		{"indented continuation of a broken record", []string{`{"level":INVALID,"endpoint":`, `  ` + loggedEndpoint, contractEndpoint}},
		{"unindented continuation of a broken record", []string{`{"level":INVALID,"endpoint":`, loggedEndpoint, contractEndpoint}},
		{"second object on the same line", []string{`{"level":"info"} ` + loggedEndpoint, contractEndpoint}},
		{"tunnel prose around the endpoint", []string{`tunnel forwarding`, contractEndpoint, `tunnel still forwarding`}},
		{"bracketed log prefixes", []string{`[INFO] tunnel forwarding`, `[WARN] retrying`, contractEndpoint}},
		{"trailing continuation after a complete value", []string{`{"level":"info"},"endpoint":`, loggedEndpoint, contractEndpoint}},
		{"multi-line log wrapper", []string{`{`, `  "level": "info",`, `  "endpoint":`, `  ` + loggedEndpoint, `}`, contractEndpoint}},
		{"endpoint-shaped prefix with a continuation", []string{loggedEndpoint + `,"endpoint":`, contractEndpoint}},
		{"mismatched bracket line", []string{`{"level":]`, loggedEndpoint, contractEndpoint}},
		{"bracketed prose with a stray quote", []string{`[INFO] opening "config`, contractEndpoint}},
		{"numeric bracket prefix", []string{`[2026] tunnel forwarding`, contractEndpoint}},
		{"punctuated bracket prose", []string{`[2026], tunnel forwarding`, contractEndpoint}},
		{"unbalanced brace log line", []string{`{level:INVALID,"endpoint":`, loggedEndpoint, contractEndpoint}},
		{"prose then a broken record", []string{`[INFO] tunnel forwarding`, `{"level":INVALID,"endpoint":`, loggedEndpoint, contractEndpoint}},

		// The two #2845 pairs: each member is indistinguishable from the other on
		// every inspectable property, and neither is endpoint-only stdout.
		{"#2845 pair 1: open log array", []string{`[INVALID,`, loggedEndpoint}},
		{"#2845 pair 1: bracket prose with an open brace", []string{`[INFO] opening {config`}},
		{"#2845 pair 2: separator on its own line", []string{loggedEndpoint, `,"endpoint":`}},
		{"#2845 pair 2: closer on its own line", []string{loggedEndpoint, `] tunnel closed`}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHookState(t, hookPrintfScript(test.lines), "")

			res, err := newHookProvisioner(h, "historical "+test.name).provisionOrReap()
			require.Error(t, err, "stdout shared with anything else must be refused")
			assert.Nil(t, res.Endpoint, "a logged URL must never become the endpoint af dials")
			assert.Contains(t, err.Error(), "printed something other than its endpoint on stdout")
			assert.NotContains(t, err.Error(), "logged-secret", "the logged token must not reach the error")
		})
	}
}

// A separator on its own line after the record is refused for every JSON
// structural byte, not just a comma (#2841 P1 3717439336).
func TestHookLaunchRefusesEveryStructuralContinuation(t *testing.T) {
	for _, separator := range []string{`,"endpoint":`, `:`, `}`, `]`} {
		t.Run(separator, func(t *testing.T) {
			h := newHookState(t, hookPrintfScript([]string{contractEndpoint, separator}), "")
			_, err := newHookProvisioner(h, "continuation "+separator).provisionOrReap()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "printed something other than its endpoint on stdout")
		})
	}
}

// The quote is what makes the error usable at 2am: it must be the offending text
// verbatim, not a paraphrase.
func TestHookLaunchRefusalQuotesTheOffendingStdoutVerbatim(t *testing.T) {
	h := newHookState(t, `
echo '[INFO] forwarding 127.0.0.1:9000 -> pod/af-7'
echo '`+contractEndpoint+`'
exit 0
`, "")
	_, err := newHookProvisioner(h, "verbatim quote").provisionOrReap()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[INFO] forwarding 127.0.0.1:9000 -> pod/af-7")
	assert.Contains(t, err.Error(), "launch_cmd", "and name the hook that printed it")
}

// A flooding tunnel must not paste megabytes into the headline — the script's
// full output is already attached below it.
func TestHookLaunchRefusalBoundsTheQuotedLine(t *testing.T) {
	h := newHookState(t, `
echo '`+strings.Repeat("x", 4000)+`'
echo '`+contractEndpoint+`'
exit 0
`, "")
	_, err := newHookProvisioner(h, "bounded quote").provisionOrReap()
	require.Error(t, err)

	headline, _, _ := strings.Cut(err.Error(), "\n")
	assert.Less(t, len(headline), 500, "the quoted line must be bounded")
	assert.Contains(t, headline, "…", "and say it was truncated")
}

// The two non-violation failures keep their own diagnoses: printing NOTHING on
// stdout is a different bug from printing one object of the wrong shape, and
// neither should be reported as a contract violation the operator cannot find.
func TestHookLaunchSeparatesEmptyStdoutFromTheWrongShape(t *testing.T) {
	t.Run("nothing on stdout", func(t *testing.T) {
		h := newHookState(t, "echo 'provisioned, but I forgot to echo the banner' >&2\nexit 0\n", "")
		_, err := newHookProvisioner(h, "silent stdout").provisionOrReap()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "printed no {\"url\",\"token\"} JSON on stdout")
		assert.NotContains(t, err.Error(), "printed something other than its endpoint")
	})

	t.Run("one object of the wrong shape", func(t *testing.T) {
		// The pre-#1592 contract: a name and a status, alone on stdout.
		h := newHookState(t, `echo '{"name":"fix-auth-bug","status":"running"}'`+"\nexit 0\n", "")
		_, err := newHookProvisioner(h, "old contract shape").provisionOrReap()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "it is not the endpoint record")
		assert.Contains(t, err.Error(), "docs/remote-hooks.md")
	})
}

// hookPrintfScript emits each line on stdout, one per line, from a launch_cmd.
func hookPrintfScript(lines []string) string {
	script := "printf '%s\\n'"
	for _, line := range lines {
		script += " '" + line + "'"
	}
	return script + "\nexit 0\n"
}
