package hooklog

import (
	"strings"
	"testing"
)

func TestExcerpt(t *testing.T) {
	long := strings.Repeat("é", ExcerptLineBytes) + "reason"
	// "…" is 3 bytes and "é" 2, so the byte budget left for "é"s is odd: the cut
	// lands mid-rune and must advance to the next rune start.
	kept := strings.Repeat("é", (ExcerptLineBytes-len("…")-len("reason"))/2) + "reason"
	for _, tc := range []struct {
		name, output, want string
	}{
		{name: "empty", output: "", want: ""},
		{name: "only blank lines", output: "\n \n\t\r\n", want: ""},
		{name: "one line", output: "boom\n", want: "; last output line:\n  | boom"},
		{
			name:   "keeps the last non-empty lines",
			output: "1\n2\n\n3\n4\n5\n\n6\n7\n\n",
			want:   "; last 5 output lines:\n  | 3\n  | 4\n  | 5\n  | 6\n  | 7",
		},
		{
			name:   "drops the truncation marker",
			output: truncatedMarker() + "tail\n",
			want:   "; last output line:\n  | tail",
		},
		{
			name:   "strips terminal escapes and controls",
			output: "\x1b[31m ERROR \x1b[0m\x1b]8;;http://x\x07build\x1b]8;;\x07\a failed\n",
			want:   "; last output line:\n  | ERROR build failed",
		},
		{
			name:   "keeps what a carriage return drew last",
			output: "progress 10%\rprogress 90%\rdone\r\n",
			want:   "; last output line:\n  | done",
		},
		{
			name:   "keeps the end of a long line on a rune boundary",
			output: long + "\n",
			want:   "; last output line:\n  | …" + kept,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Excerpt(tc.output)
			if got != tc.want {
				t.Fatalf("Excerpt(%q) =\n%q\nwant\n%q", tc.output, got, tc.want)
			}
			for _, line := range strings.Split(got, "\n")[1:] {
				if n := len(strings.TrimPrefix(line, ExcerptPrefix)); n > ExcerptLineBytes {
					t.Fatalf("quoted line is %d bytes, over the %d-byte bound: %q", n, ExcerptLineBytes, line)
				}
			}
		})
	}
}
