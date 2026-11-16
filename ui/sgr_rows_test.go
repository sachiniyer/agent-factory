package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCarrySGRAcrossRows pins the row contract behind #7419: every row starts
// in the SGR state in effect at its first cell and ends with nothing open, and
// a row that already opens and closes its own state is returned byte-for-byte.
func TestCarrySGRAcrossRows(t *testing.T) {
	x80 := strings.Repeat("x", 80)
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "plain text is untouched",
			in:   "one\ntwo\n",
			want: "one\ntwo\n",
		},
		{
			name: "hard-newline rows that close themselves are byte-identical",
			in:   "\x1b[34mblue\x1b[39m\nplain\n\x1b[32mgreen\x1b[0m\nreset",
			want: "\x1b[34mblue\x1b[39m\nplain\n\x1b[32mgreen\x1b[0m\nreset",
		},
		{
			name: "a soft-wrapped colour is re-opened on each continuation row and closed on each row",
			in:   "\x1b[31m" + x80 + "\n" + x80 + "\nxx\x1b[39m\nafter",
			want: "\x1b[31m" + x80 + "\x1b[m\n\x1b[31m" + x80 + "\x1b[m\n\x1b[31mxx\x1b[39m\nafter",
		},
		{
			name: "attributes and colours carry together",
			in:   "\x1b[1m\x1b[33m" + x80 + "\nyy-tail\x1b[0m",
			want: "\x1b[1m\x1b[33m" + x80 + "\x1b[m\n\x1b[1;33myy-tail\x1b[0m",
		},
		{
			name: "a partial reset closes only what it names",
			in:   "\x1b[1;31m" + x80 + "\x1b[39m\nbold",
			want: "\x1b[1;31m" + x80 + "\x1b[39m\x1b[m\n\x1b[1mbold\x1b[m",
		},
		{
			name: "extended colours carry exactly",
			in:   "\x1b[38;5;196m" + x80 + "\nab\x1b[48;2;1;2;3m" + x80 + "\ncd\x1b[0m",
			want: "\x1b[38;5;196m" + x80 + "\x1b[m\n\x1b[38;5;196mab\x1b[48;2;1;2;3m" + x80 + "\x1b[m\n\x1b[38;5;196;48;2;1;2;3mcd\x1b[0m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, carrySGRAcrossRows(tc.in))
		})
	}
}
