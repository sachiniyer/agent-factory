package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}
