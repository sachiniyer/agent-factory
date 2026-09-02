package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// railHeaderFacts is what a rendered rail header actually tells the reader:
// how much of the section's noun survived, whether the count is there, and
// whether the affordance is there. It is the unit the monotonicity rule is
// stated in — "facts(w) ⊇ facts(w-1)".
type railHeaderFacts struct {
	noun  int // cells of the noun that survived; 0 when it is gone entirely
	count bool
	hint  bool
}

func (f railHeaderFacts) String() string {
	return fmt.Sprintf("{noun:%d count:%v hint:%v}", f.noun, f.count, f.hint)
}

// supersetOf reports whether f shows at least everything g showed.
func (f railHeaderFacts) supersetOf(g railHeaderFacts) bool {
	return f.noun >= g.noun && (f.count || !g.count) && (f.hint || !g.hint)
}

func readRailHeaderFacts(line, noun, count, hint string) railHeaderFacts {
	f := railHeaderFacts{
		count: strings.Contains(line, count),
		hint:  strings.Contains(line, hint),
	}
	// The noun, if present at all, is the longest of its prefixes the line
	// renders — " Projects", " Proj…", or nothing. Sliced by RUNE, so a noun
	// that ever carries a multibyte character measures its prefixes rather than
	// its bytes.
	runes := []rune(noun)
	for n := len(runes); n > 0; n-- {
		if strings.Contains(line, " "+string(runes[:n])) {
			f.noun = n
			break
		}
	}
	return f
}

// railHeaderCase is one rail section rendered at a width.
type railHeaderCase struct {
	name          string
	render        func(t *testing.T, w int) string
	noun          string
	count         string
	hint          string
	minInfoWidth  int // at and above this width the count must be intact
	minHintWidth  int // at and above this width the affordance must be intact
	minNounWidth  int // at and above this width the noun must be present at all
	fullNounWidth int // at and above this width the noun must be complete
}

func railHeaderCases() []railHeaderCase {
	return []railHeaderCase{
		{
			name: "automations",
			render: func(t *testing.T, w int) string {
				t.Helper()
				a := newTestAutomations(nSimpleTasks(2))
				a.SetRect(layout.Rect{W: w, H: 4})
				return strings.TrimRight(strings.Split(stripANSI(a.View()), "\n")[0], " ")
			},
			noun: "Automations", count: "(2)",
			hint:          railHelpKey(keys.KeyTaskList) + " manage",
			minInfoWidth:  15,
			minHintWidth:  15,
			minNounWidth:  20,
			fullNounWidth: 27,
		},
		{
			name: "projects",
			render: func(t *testing.T, w int) string {
				t.Helper()
				p := newTestProjects([]SidebarProject{
					{Name: "agent-factory", Root: "/repos/agent-factory", SessionCount: 3, Active: true},
				})
				p.SetRect(layout.Rect{X: 0, Y: 0, W: w, H: 6})
				return strings.TrimRight(strings.Split(stripANSI(p.String()), "\n")[0], " ")
			},
			noun: "Projects", count: "(1)",
			hint:          railActionHint(keys.KeySwitchProjectRow, "switch"),
			minInfoWidth:  15,
			minHintWidth:  15,
			minNounWidth:  19,
			fullNounWidth: 24,
		},
	}
}

// TestRailHeaderLadderIsMonotonic is #3642. The Projects header shed its count
// at 25 columns and brought it back at 29 — its fallback kept the whole name and
// dropped the hint, while the rung above it did the reverse, so a WIDER rail said
// less than a narrower one:
//
//	22-24   Projects (1)                 noun, count
//	25-28   Projects · enter switch      noun, ......, hint
//	29+     Projects (1) · enter switch  noun, count, hint
//
// The rule that forbids that is stated over facts rather than over strings: for
// every width, what the header shows must be a superset of what it showed one
// column narrower. Both rail sections now ride one ladder, so the property is
// asserted for both — a divergence in shed order between them is exactly the
// drift #2580 caught the last time these two headers disagreed.
func TestRailHeaderLadderIsMonotonic(t *testing.T) {
	for _, tc := range railHeaderCases() {
		t.Run(tc.name, func(t *testing.T) {
			prev := readRailHeaderFacts(tc.render(t, 7), tc.noun, tc.count, tc.hint)
			for w := 8; w <= 60; w++ {
				line := tc.render(t, w)
				cur := readRailHeaderFacts(line, tc.noun, tc.count, tc.hint)
				require.Truef(t, cur.supersetOf(prev),
					"width %d shows less than width %d: %v vs %v\n  %d: %q\n  %d: %q",
					w, w-1, cur, prev, w-1, tc.render(t, w-1), w, line)
				prev = cur
			}
		})
	}
}

// The three rules the ladder holds at every rung, asserted across the whole
// supported range rather than sampled.
func TestRailHeaderKeepsItsSeparatorCountAndAffordance(t *testing.T) {
	for _, tc := range railHeaderCases() {
		t.Run(tc.name, func(t *testing.T) {
			for w := 8; w <= 60; w++ {
				line := tc.render(t, w)

				require.NotContainsf(t, line, "…"+railHintSeparator,
					"width %d: the ellipsis must never abut the separator: %q", w, line)
				if i := strings.Index(line, "·"); i > 0 {
					require.Equalf(t, " ", line[i-1:i],
						"width %d: the separator must keep its leading space: %q", w, line)
				}
				if w >= tc.minInfoWidth {
					require.Containsf(t, line, tc.count,
						"width %d: the count is the only information the header carries: %q", w, line)
				}
				if w >= tc.minHintWidth {
					require.Containsf(t, line, tc.hint,
						"width %d: the affordance is the last thing cut: %q", w, line)
				}
				if w >= tc.minNounWidth {
					require.Truef(t, strings.Contains(line, " "+string([]rune(tc.noun)[:1])),
						"width %d: the noun must be present: %q", w, line)
				}
				if w >= tc.fullNounWidth {
					require.Containsf(t, line, tc.noun,
						"width %d: the noun must be complete: %q", w, line)
				}
			}
		})
	}
}

// A header that cannot say how many things exist renders the same line for two
// of them as for none. Both sections used to, at their narrow widths.
func TestRailHeaderDistinguishesEmptyFromPopulated(t *testing.T) {
	for w := 19; w <= 40; w++ {
		empty := newTestAutomations(nil)
		empty.SetRect(layout.Rect{W: w, H: 4})
		busy := newTestAutomations(nSimpleTasks(2))
		busy.SetRect(layout.Rect{W: w, H: 4})
		require.NotEqualf(t, stripANSI(empty.View()), stripANSI(busy.View()),
			"automations, width %d: two tasks must not render the same header as none", w)

		noProjects := newTestProjects(nil)
		noProjects.SetRect(layout.Rect{X: 0, Y: 0, W: w, H: 6})
		oneProject := newTestProjects([]SidebarProject{{Name: "agent-factory", SessionCount: 3, Active: true}})
		oneProject.SetRect(layout.Rect{X: 0, Y: 0, W: w, H: 6})
		require.NotEqualf(t,
			strings.Split(stripANSI(noProjects.String()), "\n")[0],
			strings.Split(stripANSI(oneProject.String()), "\n")[0],
			"projects, width %d: one project must not render the same header as none", w)
	}
}

// The exact ladder for the Projects header across the range #3642 measures.
func TestProjectsHeaderDegradationOrder(t *testing.T) {
	render := railHeaderCases()[1].render
	for _, tc := range []struct {
		w    int
		want string
	}{
		{40, " Projects (1) · ↵ switch"}, // everything
		{24, " Projects (1) · ↵ switch"}, // exactly
		{23, " Projec… (1) · ↵ switch"},  // the noun shrinks, count intact
		{22, " Proje… (1) · ↵ switch"},   // the 22-col rail minimum (#1090), i.e. 80x24
		{19, " Pr… (1) · ↵ switch"},      // …and keeps shrinking
		{15, " (1) · ↵ switch"},          // the noun goes, count and hint stay
	} {
		require.Equalf(t, tc.want, render(t, tc.w), "rail width %d", tc.w)
	}
}
