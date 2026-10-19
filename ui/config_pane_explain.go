package ui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/config"
)

// The explain sub-view (#4803): 'e' on a key row resolves it through the pane's
// explain seam — in-process locally, the targeted daemon's ExplainConfig route
// under a remote target — and swaps the list for the SAME candidate trace
// `af config get --explain` prints. The trace is the point of the capability:
// the row already shows WHAT the effective value is; this is WHERE it came
// from, which layers were shadowed or disallowed, and which file+key path each
// candidate lives at. ResolvedValue carries all of it, so nothing here
// re-derives precedence — this file formats, it does not resolve.

// beginExplain swaps the list for the selected key's provenance. The resolve
// is synchronous for the same reason the remote save in commitEdit is: the
// pane's Update runs on the UI goroutine and a daemon round-trip there is
// bounded by the client's timeout — worse is a keyed "explaining…" state the
// user can out-type.
func (c *ConfigPane) beginExplain() {
	entry := c.selectedEntry()
	if entry == nil {
		// Tier headings and account rows have no config key to explain.
		return
	}
	value, err := c.explain(entry.Key)
	if err != nil {
		// Surface the refusal where a status belongs — the remote daemon does
		// not know the route (version skew), the key is unknown to it, or it
		// is unreachable. Never fabricate an empty trace.
		c.status = err.Error()
		c.statusIsError = true
		c.restartNotice = ""
		return
	}
	c.savedScrollTop = c.scrollTop
	c.scrollTop = 0
	c.explainKey = entry.Key
	c.explainLines = c.wrappedExplainLines(*value)
	c.explaining = true
	c.clearStatus()
}

// handleExplainKey drives the explain view: a reader, so it scrolls and exits
// and swallows the rest rather than letting a stray key mutate config behind
// the trace. 'e' toggles back out as naturally as it came in.
func (c *ConfigPane) handleExplainKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "e", "enter":
		c.explaining = false
		c.explainLines = nil
		c.scrollTop = c.savedScrollTop
	case "up", "k":
		c.scrollTop = max(0, c.scrollTop-1)
	case "down", "j":
		c.scrollTop = min(max(0, len(c.explainLines)-1), c.scrollTop+1)
	case "pgup":
		c.scrollTop = max(0, c.scrollTop-c.explainBudget())
	case "pgdown":
		c.scrollTop = min(max(0, len(c.explainLines)-1), c.scrollTop+c.explainBudget())
	}
	return true
}

// explainBudget approximates the explain window for the page keys so they step
// by a real page rather than a guessed constant. (The window itself computes
// the exact budget from the rendered header/footer; this only sizes a step.)
func (c *ConfigPane) explainBudget() int {
	budget := c.height - 4 - cueRows // title + subtitle, blank + hints footer
	if budget < 1 {
		return 1
	}
	return budget
}

// renderExplainView lays out the trace through the same header/window/footer
// assembly as the list view, so the scroll cues and the box behave identically.
func (c *ConfigPane) renderExplainView() string {
	header := c.fitPaneLine(configTitleStyle.Render("Explain")+
		configLocationStyle.Render("  "+c.explainKey)) + "\n" +
		c.wrap("on-disk sources · the running daemon's value was not checked", configHintStyle)
	footer := "\n" + configHintStyle.Render(c.fitHints([]configHint{
		{text: "↑/↓ scroll", drop: 1},
		{text: "esc back"},
	})) + "\n"
	return c.renderWindowed(header, footer, c.explainLines, -1, -1)
}

// wrappedExplainLines renders the ResolvedValue the way the CLI's explanation
// does — effective value, default, policy, then each candidate with its value,
// location, and result — wrapped to the pane so the window's line math stays
// exact.
func (c *ConfigPane) wrappedExplainLines(v config.ResolvedValue) []string {
	var logical []string
	logical = append(logical,
		fmt.Sprintf("%s = %s", v.Key, explainSafe(config.FormatExplainValue(v.Value))))
	if v.Default != "" {
		logical = append(logical, "default: "+explainSafe(v.Default))
	}
	logical = append(logical, "policy: "+v.Merge+" · "+strings.Join(v.Precedence, " < "), "")
	for _, cand := range v.Candidates {
		result := cand.Result
		if cand.Reason != "" {
			result += " · " + cand.Reason
		}
		logical = append(logical, cand.Layer+": "+result)
		value := "—"
		if cand.Present {
			value = explainSafe(config.FormatExplainValue(cand.Value))
		}
		location := "compiled default"
		if cand.Path != "" {
			location = explainSafe(cand.Path) + ":" + explainSafe(cand.KeyPath)
		}
		logical = append(logical, "    "+value+" · "+location)
	}
	if len(v.Origins) > 0 {
		// Composite keys (tables, merged lists) do not have one winner: each
		// leaf carries its own origin, so a single candidate row would lie.
		logical = append(logical, "", "origins:")
		leaves := make([]string, 0, len(v.Origins))
		for leaf := range v.Origins {
			leaves = append(leaves, leaf)
		}
		sort.Strings(leaves)
		for _, leaf := range leaves {
			origin := v.Origins[leaf]
			location := "compiled default"
			if origin.Path != "" {
				location = explainSafe(origin.Path) + ":" + explainSafe(origin.KeyPath)
			}
			logical = append(logical, "  "+explainSafe(leaf)+": "+origin.Layer+" · "+location)
		}
	}

	var lines []string
	for _, l := range logical {
		wrapped := configValueStyle.Width(c.width - 2).Render(l)
		lines = append(lines, strings.Split(strings.TrimSuffix(wrapped, "\n"), "\n")...)
	}
	return lines
}

// explainSafe renders file-sourced text (values, paths, key paths) safe for
// the compositor — the same strip+flatten pair the list's truncateConfigPreview
// applies, without its truncation: the explain view exists to show the whole
// value, wrapped. What it cannot afford to pass through is what a value can
// DO: a free-form key like on_archive_command holds whatever TOML expressed,
// including \u001B escapes that Lip Gloss would emit verbatim — screen clears,
// cursor moves, OSC writes from inside the trace.
func explainSafe(s string) string {
	return flattenToOneLine(xansi.Strip(s))
}
