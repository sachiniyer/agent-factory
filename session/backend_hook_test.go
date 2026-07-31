package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractJSON exercises the bracket-counting parser added for #572,
// which replaced the previous line-based heuristic. The parser must
// recover the first complete top-level JSON value from mixed
// stderr+stdout output, including pretty-printed (multi-line) payloads
// produced by tools like `jq .` and `python3 -m json.tool`.
func TestExtractJSON(t *testing.T) {
	prettyObject := `{
  "name": "remote-one",
  "status": "running",
  "host": "h1"
}`

	prettyArray := `[
  {
    "name": "remote-one",
    "status": "running"
  },
  {
    "name": "remote-two",
    "status": "stopped"
  }
]`

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "pretty-printed object",
			in:   prettyObject,
			want: prettyObject,
		},
		{
			name: "pretty-printed array",
			in:   prettyArray,
			want: prettyArray,
		},
		{
			name: "stderr progress before multi-line array",
			in:   "connecting to remote host...\nfetched 2 sessions\n" + prettyArray,
			want: prettyArray,
		},
		{
			name: "noise around (not inside) JSON",
			in:   "begin output\n" + prettyObject + "\nend of output\n",
			want: prettyObject,
		},
		{
			name: "escaped quotes inside string",
			in:   `{"msg": "she said \"hi\""}`,
			want: `{"msg": "she said \"hi\""}`,
		},
		{
			name: "nested arrays inside object",
			in:   `{"items": [1, 2, [3, 4]]}`,
			want: `{"items": [1, 2, [3, 4]]}`,
		},
		{
			name: "top-level string is not a match",
			in:   `"abc"`,
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "no JSON at all",
			in:   "just some log lines\nnothing structured here\n",
			want: "",
		},
		{
			name: "single-line object regression",
			in:   `{"name": "remote-one", "status": "running"}`,
			want: `{"name": "remote-one", "status": "running"}`,
		},
		{
			name: "single-line array regression",
			in:   `[{"name": "remote-one", "status": "running"}]`,
			want: `[{"name": "remote-one", "status": "running"}]`,
		},
		{
			name: "brace inside string does not unbalance",
			in:   `{"text": "this } is not a close"}`,
			want: `{"text": "this } is not a close"}`,
		},
		{
			name: "bracket inside string does not unbalance",
			in:   `{"text": "this ] is not a close"}`,
			want: `{"text": "this ] is not a close"}`,
		},
		{
			name: "skips invalid candidate then finds valid one",
			// First `{` opens an invalid candidate (unbalanced after `:`); the
			// parser must keep scanning until it finds a complete value.
			in:   "noise { not json } more\n" + `{"name": "remote-one"}`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "recovers valid value inside balanced malformed prose",
			in:   `noise { not json {"name": "remote-one"} still bad }`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "descends through nested malformed wrappers",
			in:   `noise { bad [ bad {"name": "remote-one"} bad ] bad }`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "resynchronizes after raw newline in unterminated string",
			in:   "progress { reading \"config\n" + `{"name": "remote-one"}`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "resynchronizes before same-line JSON after unterminated string",
			in:   "progress { reading \"config " + `{"name": "remote-one"}` + "\n",
			want: `{"name": "remote-one"}`,
		},
		{
			name: "resynchronizes before same-line JSON at EOF",
			in:   "progress { reading \"config " + `{"name": "remote-one"}`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "recovers value after diagnostic colon",
			in:   "progress { pending:" + `{"name": "remote-one"}`,
			want: `{"name": "remote-one"}`,
		},
		{
			name: "resynchronizes past multiple openers in malformed string",
			in:   "progress { reading \"config {{ " + `{"name": "remote-one"}` + "\n",
			want: `{"name": "remote-one"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHookProvisionRejectsEndpointInMalformedLogArray(t *testing.T) {
	h := newHookState(t, `
echo '[INVALID,{"url":"http://array.invalid","token":"array-secret"}]' >&2
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	p := newHookProvisioner(h, "malformed array logger")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "an endpoint-shaped log array element must not hide the launch record")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "valid endpoint output must not reap the working sandbox")
}

func TestHookOutputSuffixRedactsOverlappingSerializedEndpoints(t *testing.T) {
	const firstSecret = "first-overlap-must-not-leak"
	const secondSecret = "second-overlap-must-not-leak"
	output := `"{\"token\":\"first-overlap-must-not-leak\"}"{\"token\":\"second-overlap-must-not-leak\"}"`

	var suffix string
	require.NotPanics(t, func() {
		suffix = hookOutputSuffix([]byte(output))
	}, "overlapping serialized values must not abort hook error reporting")
	assert.NotContains(t, suffix, firstSecret)
	assert.NotContains(t, suffix, secondSecret)
	assert.Contains(t, suffix, "[REDACTED]")
}

func TestHookOutputSuffixRedactsUnterminatedSerializedEndpoint(t *testing.T) {
	const secret = "unterminated-serialized-token-must-not-leak"
	output := `INFO endpoint="{\"url\":\"\",\"token\":\"unterminated-serialized-token-must-not-leak\"`

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "an incomplete outer string must not expose its serialized token")
	assert.Contains(t, suffix, "[REDACTED]")
}

func TestHookOutputSuffixRedactsUnterminatedSerializedEndpointBeforeNewline(t *testing.T) {
	const secret = "newline-terminated-serialized-token-must-not-leak"
	output := "INFO endpoint=\"{\\\"token\\\":\\\"newline-terminated-serialized-token-must-not-leak\\\"}\n"

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a line terminator must not make an incomplete outer string safe")
	assert.Contains(t, suffix, "[REDACTED]")
}

func TestHookOutputSuffixRedactsSerializedEndpointAfterNewline(t *testing.T) {
	const secret = "post-newline-serialized-token-must-not-leak"
	output := "INFO endpoint=\"\n{\\\"token\\\":\\\"post-newline-serialized-token-must-not-leak\\\"}\""

	suffix := hookOutputSuffix([]byte(output))
	assert.NotContains(t, suffix, secret, "a raw newline must not hide a following escaped token")
	assert.Contains(t, suffix, "[REDACTED]")
}

func TestExtractJSONHandlesMalformedStringOpenerFlood(t *testing.T) {
	const value = `{"name":"remote-one"}`
	output := `progress { reading "config ` + strings.Repeat("{", 50_000) + value + "\n"

	started := time.Now()
	assert.Equal(t, value, extractJSON(output))
	assert.Less(t, time.Since(started), time.Second, "string resynchronization must not retry every opener suffix")
}
