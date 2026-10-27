package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for the SGR parser in reverseVideoURLSubject. The prior
// implementation scanned for the first 'm' anywhere in the line and split the
// whole parameter chunk on ';', so any SGR token equal to "7" — a 256-color
// palette index (38;5;7 / 48;5;7) or an RGB component of a truecolor sequence —
// was misread as the reverse-video attribute. An ordinarily-colored URL then
// composed with an unrelated confirmation below into a doc-trust false
// positive that injected D+Enter into a live agent (#2638's guard exists to
// prevent exactly that composition). The guard's invariant — a recently
// printed URL must not turn the next generic confirmation into a URL-offer
// prompt — was already pinned for the unstyled case; these tests pin it for
// the colored case too, and pin that genuine reverse-video URLs stay detected.

const unrelatedDocConfirmLine = "Add src/main.go to the chat? (Y)es/(N)o/(D)on't ask again [Yes]:"

func docTrustPaneFromURLLine(urlLine string) string {
	return urlLine + "\n" + unrelatedDocConfirmLine
}

// coloredURLSubjects are URL subject lines whose SGR contains a bare "7" token
// only as an extended-color sub-parameter (palette index or RGB component),
// never as the reverse-video attribute. None may satisfy the guard.
var coloredURLSubjects = []struct {
	name    string
	urlLine string
}{
	{"256color-fg-index-7", "\x1b[38;5;7mhttps://example.com/docs\x1b[0m"},
	{"256color-bg-index-7", "\x1b[48;5;7mhttps://example.com/docs\x1b[0m"},
	{"truecolor-fg-R-7", "\x1b[38;2;7;170;170mhttps://example.com/docs\x1b[0m"},
	{"truecolor-bg-G-7", "\x1b[48;2;170;7;170mhttps://example.com/docs\x1b[0m"},
	{"truecolor-fg-B-7", "\x1b[38;2;170;170;7mhttps://example.com/docs\x1b[0m"},
	{"underline-color-256-index-7", "\x1b[58;5;7mhttps://example.com/docs\x1b[0m"},
	{"256color-index-7-plus-underline", "\x1b[38;5;7;4mhttps://example.com/docs\x1b[0m"},
}

func TestDocTrustPromptPresent_ColoredURLNotReverseVideo(t *testing.T) {
	for _, c := range coloredURLSubjects {
		t.Run(c.name, func(t *testing.T) {
			require.False(t, DocTrustPromptPresent(docTrustPaneFromURLLine(c.urlLine)),
				"a URL styled with an extended-color SGR whose sub-parameter equals 7 is not reverse video")
		})
	}
}

// The same colored URL above an unrelated confirmation must never inject
// keystrokes into a live agent — the harm the guard exists to prevent. This
// drives the real CheckAndHandleTrustPrompt so the DetectAgentFromCommand gate
// and the injection path are exercised end-to-end.
func TestCheckAndHandleTrustPrompt_ColoredURLAboveUnrelatedConfirmInjectsNothing(t *testing.T) {
	for _, program := range []string{ProgramAider, ProgramGemini} {
		for _, c := range coloredURLSubjects {
			t.Run(program+"/"+c.name, func(t *testing.T) {
				handled, cmds := runTrustPromptCheck(t, program, docTrustPaneFromURLLine(c.urlLine))
				require.Empty(t, sentKeystrokes(cmds),
					"a colored (non-reverse-video) URL must not compose with an unrelated "+
						"confirmation into a doc-trust prompt; got %v", cmds)
				require.False(t, handled)
			})
		}
	}
}

// A URL whose SGR carries no reverse attribute (and, for truecolor, no bare
// "7" at all) was already correctly rejected; pin it against an over-correction
// that drops genuine reverse-video detection.
var nonReverseURLSubjects = []struct {
	name    string
	urlLine string
}{
	{"truecolor-no-seven", "\x1b[38;2;170;170;170mhttps://example.com/docs\x1b[0m"},
	{"truecolor-other", "\x1b[48;2;200;100;50mhttps://example.com/docs\x1b[0m"},
	{"256color-index-21", "\x1b[38;5;21mhttps://example.com/docs\x1b[0m"},
	{"reset-then-256fg", "\x1b[0;38;5;245mhttps://example.com/docs\x1b[0m"},
	{"plain-unstyled", "https://example.com/docs"},
}

func TestDocTrustPromptPresent_NonReverseSGRNotReverseVideo(t *testing.T) {
	for _, c := range nonReverseURLSubjects {
		t.Run(c.name, func(t *testing.T) {
			require.False(t, DocTrustPromptPresent(docTrustPaneFromURLLine(c.urlLine)),
				"a non-reverse SGR must never satisfy the reverse-video guard")
		})
	}
}

// Genuine reverse-video URL subjects must still be detected and dismissed —
// including combinations with extended-color SGRs that previously matched.
var genuineReverseURLSubjects = []struct {
	name    string
	urlLine string
}{
	{"bare-reverse", "\x1b[7mhttps://aider.chat/docs/usage\x1b[0m"},
	{"reverse-plus-256fg", "\x1b[7;38;5;245mhttps://aider.chat/docs/usage\x1b[0m"},
	{"reverse-plus-truecolor", "\x1b[7;38;2;170;170;170mhttps://aider.chat/docs/usage\x1b[0m"},
	{"reverse-then-recolor", "\x1b[7m\x1b[38;5;245mhttps://aider.chat/docs/usage\x1b[0m"},
}

func TestCheckAndHandleTrustPrompt_GenuineReverseVideoURLIsDismissed(t *testing.T) {
	for _, program := range []string{ProgramAider, ProgramGemini} {
		for _, c := range genuineReverseURLSubjects {
			t.Run(program+"/"+c.name, func(t *testing.T) {
				handled, cmds := runTrustPromptCheck(t, program, docTrustPaneFromURLLine(c.urlLine))
				require.True(t, handled, "a genuine reverse-video URL subject must be dismissed")
				require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: D Enter",
					"the doc-trust dialog is dismissed with 'D'+Enter; got %v", cmds)
			})
		}
	}
}

// A reverse-video attribute cleared by a later SGR 0/27/empty must not be
// carried forward, and a following extended-color sequence must not re-introduce
// reverse from its "7" sub-parameter.
func TestDocTrustPromptPresent_ReverseClearedByResetStaysClear(t *testing.T) {
	cases := []struct {
		name    string
		urlLine string
	}{
		{"reset-then-extended-color-skip7", "\x1b[7m\x1b[0m\x1b[38;5;7mhttps://example.com/docs\x1b[0m"},
		{"twentyseven-clears", "\x1b[7m\x1b[27mhttps://example.com/docs\x1b[0m"},
		{"empty-param-clears", "\x1b[7m\x1b[mhttps://example.com/docs\x1b[0m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.False(t, DocTrustPromptPresent(docTrustPaneFromURLLine(c.urlLine)),
				"reverse cleared by reset must not be re-introduced by a later extended-color SGR")
		})
	}
}

// The reverse-video state is the running aggregate across leading SGR
// sequences: an extended-color SGR never flips it, and "7" sets it whether it
// comes before or after an extended-color SGR.
func TestDocTrustPromptPresent_ReverseAggregatesAcrossLeadingSGR(t *testing.T) {
	cases := []struct {
		name    string
		urlLine string
		want    bool
	}{
		{"set-then-extended-color-stays-reverse",
			"\x1b[7m\x1b[38;5;7mhttps://example.com/docs\x1b[0m", true},
		{"extended-color-then-set-becomes-reverse",
			"\x1b[38;5;7m\x1b[7mhttps://example.com/docs\x1b[0m", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, DocTrustPromptPresent(docTrustPaneFromURLLine(c.urlLine)))
		})
	}
}

// A leading non-SGR CSI (e.g. cursor-hide \x1b[?25l) before a genuine
// reverse-video URL is not re-emitted by `capture-pane -p -e -J`, which only
// re-emits SGR. Pin that such a prefix — if it ever reached the predicate —
// produces NO false positive: the safe direction is a miss, never a D+Enter
// injection.
func TestDocTrustPromptPresent_LeadingNonSGRCSIDoesNotProduceFalsePositive(t *testing.T) {
	urlLine := "\x1b[?25l\x1b[7mhttps://aider.chat/docs/usage\x1b[0m"
	require.False(t, DocTrustPromptPresent(docTrustPaneFromURLLine(urlLine)),
		"a leading non-SGR CSI must not be misparsed into a reverse-video false positive")
}
