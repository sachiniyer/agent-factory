package overlay

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// Explicit forms so the source encoding can't silently change what is tested:
// cafeNFC uses composed U+00E9, cafeNFD uses "e" + combining acute U+0301.
const (
	cafeNFC = "caf\u00e9"
	cafeNFD = "cafe\u0301"
)

// TestSearchTierOf classifies the match tiers the overlay ranks by (#5299):
// exact > prefix > contiguous substring (word boundary or mid-word) >
// scattered subsequence > no match.
func TestSearchTierOf(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		str     string
		want    searchTier
	}{
		{"exact", "beta", "beta", tierExact},
		{"exact folds case", "BETA", "beta", tierExact},
		{"exact across NFC/NFD", cafeNFD, cafeNFC, tierExact},

		{"prefix", "bet", "beta", tierPrefix},
		{"prefix folds case", "BET", "beta", tierPrefix},
		// The empty pattern must give EVERY candidate the same tier, or an
		// empty query would float empty-titled/empty-branched sessions to the
		// top of the list (searchTierOf("", "") used to be tierExact).
		{"empty pattern is a prefix of all", "", "alpha", tierPrefix},
		{"empty pattern is uniform on empty", "", "", tierPrefix},

		{"word-boundary substring", "bet", "alpha-bet-soup", tierSubstring},
		{"mid-word substring", "bet", "alphabetic", tierSubstring},
		{"substring folds case", "BET", "alphabetic", tierSubstring},

		{"scattered subsequence", "bet", "bright-newt", tierScattered},
		{"scattered folds case", "BET", "bright-newt", tierScattered},

		{"no match", "bet", "alpha", tierNone},
		{"out-of-order is no match", "teb", "beta", tierNone},
		{"pattern longer than str", "betanetwork", "beta", tierNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, searchTierOf(tt.pattern, tt.str))
		})
	}
}

// TestSearchOverlayRanksByMatchTier pins #5299 on the overlay itself: ranking
// reorders matches by tier — Enter lands on the top row — while the match set
// and the intra-tier (sidebar) order are untouched.
func TestSearchOverlayRanksByMatchTier(t *testing.T) {
	tests := []struct {
		name      string
		instances []*session.Instance
		query     string
		want      []string // expected titles, top to bottom
	}{
		{
			name: "issue #5299: prefix match beats a scattered one",
			instances: []*session.Instance{
				{Title: "bright-newt", Branch: "dev/bright-newt"},
				{Title: "alpha"},
				{Title: "beta", Branch: "dev/beta"},
			},
			query: "bet",
			want:  []string{"beta", "bright-newt"},
		},
		{
			name: "exact beats prefix",
			instances: []*session.Instance{
				{Title: "betamax"},
				{Title: "beta"},
			},
			query: "beta",
			want:  []string{"beta", "betamax"},
		},
		{
			name: "prefix beats substring",
			instances: []*session.Instance{
				{Title: "alphabetic"},
				{Title: "beta"},
			},
			query: "bet",
			want:  []string{"beta", "alphabetic"},
		},
		{
			name: "substring beats scattered",
			instances: []*session.Instance{
				{Title: "bright-newt"},
				{Title: "alphabet"},
			},
			query: "bet",
			want:  []string{"alphabet", "bright-newt"},
		},
		{
			name: "branch match participates in ranking",
			instances: []*session.Instance{
				{Title: "bright-newt"},
				{Title: "gamma", Branch: "dev/beta-deploy"},
				{Title: "beta"},
			},
			query: "bet",
			want:  []string{"beta", "gamma", "bright-newt"},
		},
		{
			name: "ties keep sidebar order",
			instances: []*session.Instance{
				{Title: "bet-two"},
				{Title: "bet-one"},
			},
			query: "bet",
			want:  []string{"bet-two", "bet-one"},
		},
		{
			// Regression: an empty query must leave the sidebar order
			// untouched — the branched-with-branch sessions and the
			// empty-branch "pending" one all earn the same tier.
			name: "empty query keeps sidebar order",
			instances: []*session.Instance{
				{Title: "with-branch", Branch: "dev/with-branch"},
				{Title: "no-branch"},
				{Title: "another", Branch: "dev/another"},
			},
			query: "",
			want:  []string{"with-branch", "no-branch", "another"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSearchOverlay(tt.instances)
			for _, r := range tt.query {
				s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			}

			got := s.ResultInstances()
			require.Len(t, got, len(tt.want))
			for i, inst := range got {
				assert.Equal(t, tt.want[i], inst.Title, "result %d", i)
			}

			// The user habit from the issue: /, type, Enter — lands on the
			// top row.
			require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
			assert.Equal(t, tt.want[0], s.GetSelectedInstance().Title)
		})
	}
}

// TestSearchOverlayEmptyStatesUseSentenceCase is #3632: the "no matches found"
// line was the one empty state in this file with no test pin at all, so nothing
// noticed it drifting out of sentence case. Its sibling — the "type to search…"
// prompt — is a mid-sentence instruction, not an empty state, and stays as it is.
func TestSearchOverlayEmptyStatesUseSentenceCase(t *testing.T) {
	s := NewSearchOverlay(nil)
	s.SetWidth(60)
	// Type a query the (empty) instance list cannot match, so the overlay is in
	// its no-results state rather than its "type to search…" prompt.
	for _, r := range "zzzz" {
		s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	out := renderedText(s.Render())
	require.Contains(t, out, "No matches found",
		"the empty result state renders in sentence case:\n%s", out)
	require.NotContains(t, out, "no matches found",
		"the lowercase form must be gone:\n%s", out)
}
