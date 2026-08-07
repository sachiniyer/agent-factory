package ui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTabJump(t *testing.T) {
	tabs := []string{"claude", "server", "dns-server", "build-logs-2026", "12"}

	tests := []struct {
		name  string
		query string
		want  int
		why   string
	}{
		{
			name:  "an ordinal past nine is the whole point",
			query: "12",
			want:  0,
			why: "12 is out of range for a 5-tab bar; it must NOT fall through to the tab NAMED \"12\" — " +
				"a number in this prompt means the same thing the digit keys mean",
		},
		{name: "a bare number is an ordinal", query: "3", want: 3, why: "digits mean position, here as everywhere"},
		{name: "the first tab", query: "1", want: 1},
		{name: "the last tab", query: "5", want: 5},
		{name: "zero is not a tab", query: "0", want: 0, why: "the bar is one-based"},
		{name: "negative is not a tab", query: "-2", want: 0},

		{name: "exact name", query: "server", want: 2, why: "exact wins over the dns-server substring"},
		{name: "exact name ignores case", query: "CLAUDE", want: 1},

		{
			name:  "unique prefix",
			query: "cla",
			want:  1,
			why:   "a prefix nobody else starts with is unambiguous",
		},
		{
			name:  "prefix beats substring",
			query: "s",
			want:  2,
			why: "\"s\" prefixes only \"server\" but appears inside \"dns-server\" too; " +
				"refusing here would make short prefixes useless",
		},
		{
			name:  "unique substring reaches the memorable middle",
			query: "logs",
			want:  4,
			why:   "a generated name is reachable by the part a human remembers",
		},

		{
			name:  "a unique prefix wins even when the query also appears inside another name",
			query: "d",
			want:  3,
			why: "only dns-server STARTS with d, so the prefix rule settles it — it also appears " +
				"inside build-logs, but pooling the two rules would refuse a perfectly clear prefix",
		},
		{name: "no match at all", query: "nonexistent", want: 0},
		{name: "empty query", query: "", want: 0},
		{name: "whitespace only", query: "   ", want: 0},
		{name: "surrounding whitespace is trimmed", query: "  server  ", want: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, ResolveTabJump(test.query, tabs), test.why)
		})
	}
}

// Ambiguity must lose even when one candidate is an exact PREFIX of another, which is
// the case a "first match wins" implementation gets wrong most often.
func TestResolveTabJumpPrefersTheExactNameOverALongerOne(t *testing.T) {
	tabs := []string{"log", "logs", "logging"}
	require.Equal(t, 1, ResolveTabJump("log", tabs), "an exact name is never ambiguous")
	require.Equal(t, 2, ResolveTabJump("logs", tabs))
	require.Equal(t, 3, ResolveTabJump("logg", tabs), "unique prefix")
	require.Equal(t, 0, ResolveTabJump("lo", tabs), "prefixes all three — ambiguous")
}

// An empty bar answers nothing rather than panicking, which is reachable: the prompt
// can be opened on a session whose tabs have not loaded.
func TestResolveTabJumpOnAnEmptyBar(t *testing.T) {
	require.Equal(t, 0, ResolveTabJump("1", nil))
	require.Equal(t, 0, ResolveTabJump("anything", []string{}))
}

// Genuine ambiguity, which the fixture above cannot express because no two of its
// names share a prefix. This is the case a "first match wins" implementation gets
// wrong, and the one where saying no is the right answer.
func TestResolveTabJumpRefusesAnAmbiguousPrefix(t *testing.T) {
	tabs := []string{"deploy-staging", "deploy-prod", "claude"}
	require.Equal(t, 0, ResolveTabJump("deploy", tabs),
		"two tabs start with deploy; picking one silently teaches the user the prompt is unreliable")
	require.Equal(t, 0, ResolveTabJump("d", tabs), "same, one character in")
	require.Equal(t, 1, ResolveTabJump("deploy-s", tabs), "narrowing it resolves")
	require.Equal(t, 2, ResolveTabJump("prod", tabs), "and a unique substring reaches the other")
}
