package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

const readyIcon = "● "

var readyStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#51bd73", Dark: "#51bd73"})

var titleStyle = lipgloss.NewStyle().
	Padding(1, 1, 0, 1).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var listDescStyle = lipgloss.NewStyle().
	Padding(0, 1, 1, 1).
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

var selectedTitleStyle = lipgloss.NewStyle().
	Padding(1, 1, 0, 1).
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#1a1a1a"})

var selectedDescStyle = lipgloss.NewStyle().
	Padding(0, 1, 1, 1).
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#1a1a1a"})

var mainTitle = lipgloss.NewStyle().
	Background(lipgloss.Color("62")).
	Foreground(lipgloss.Color("230"))

var autoYesStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.Color("#1a1a1a"))

// deletingTitleColor dims a mid-deletion row — title and branch/PR lines —
// to the description gray so it visually recedes while its teardown runs in
// the background (#844, #853).
var deletingTitleColor = lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"}

// InstanceRenderer handles rendering of session.Instance objects
type InstanceRenderer struct {
	spinner *spinner.Model
	width   int
	// indexWidth is the number of digits to left-pad the 1-based row index to,
	// so every row in a list shares one prefix width and the branch/PR lines
	// stay aligned across power-of-10 boundaries (9→10, 99→100, …). The caller
	// sets it to the digit count of the largest index in the list; a small list
	// keeps the original single-digit prefix and pays no extra width. When it is
	// 0 (or smaller than idx's own digit count) Render falls back to idx's width
	// so the index is never truncated (#871, #923, #939).
	indexWidth int
}

func (r *InstanceRenderer) setWidth(width int) {
	r.width = AdjustPreviewWidth(width)
}

// ɹ and ɻ are other options.
const branchIcon = "Ꮧ"

func (r *InstanceRenderer) Render(i *session.Instance, idx int, selected bool, hasMultipleRepos bool) string {
	// Each extra digit grows the prefix by one cell, which shifts the
	// len(prefix)-derived branch/PR indentation and misaligns adjacent visible
	// rows at every power-of-10 boundary (9→10, 99→100, 999→1000, …). Left-pad
	// the NUMBER (right-justified) to a width derived from the largest index in
	// the list so every row's prefix is the same width while the dot and full
	// index are always preserved. An earlier trim-loop (#923) held width by
	// deleting the rightmost char per tier, which corrupted content — dropping
	// the dot at idx≥100 and a digit at idx≥1000 (e.g. 1000 rendered as "100").
	// Padding keeps the same alignment without eating content, and because the
	// width tracks the list size a small list still renders the original
	// single-digit prefix (#871, #923, #939).
	digits := r.indexWidth
	if d := len(strconv.Itoa(idx)); d > digits {
		digits = d
	}
	prefix := fmt.Sprintf(" %*d. ", digits, idx)
	titleS := selectedTitleStyle
	descS := selectedDescStyle
	if !selected {
		titleS = titleStyle
		descS = listDescStyle
	}

	// add spinner next to title if it's running
	status := i.GetStatus()
	var join string
	switch status {
	case session.Running, session.Loading, session.Deleting:
		join = fmt.Sprintf("%s ", r.spinner.View())
	case session.Ready:
		join = readyStyle.Render(readyIcon)
	default:
	}

	// Cut the title if it's too long
	titleText := i.Title
	if i.IsRemote() {
		titleText = "[remote] " + titleText
	}
	// A deleting row keeps spinning but is explicitly marked and dimmed so it
	// reads as "going away", not "busy working" (#844).
	if status == session.Deleting {
		titleText = "[deleting] " + titleText
		titleS = titleS.Foreground(deletingTitleColor)
		// Dim the branch/PR lines too: on a selected row descS is the
		// high-contrast selectedDescStyle, and leaving it bright makes the
		// secondary lines stand out more than the dimmed title (#853).
		descS = descS.Foreground(deletingTitleColor)
	}
	widthAvail := r.width - 3 - runewidth.StringWidth(prefix) - 1
	if widthAvail <= 0 {
		// No room for any title text at this width; render just the prefix.
		// lipgloss.Place doesn't clip oversize content, so leaving titleText
		// intact here would spill past sidebarW (#646).
		titleText = ""
	} else if runewidth.StringWidth(titleText) > widthAvail {
		// Drop the "..." tail when the container is too narrow to fit it,
		// otherwise runewidth.Truncate returns content wider than widthAvail
		// and lipgloss.Place won't clip the overflow.
		tail := "..."
		if widthAvail < runewidth.StringWidth(tail) {
			tail = ""
		}
		titleText = runewidth.Truncate(titleText, widthAvail, tail)
	}
	// At very narrow widths (sidebarW ≤ 11, r.width ≤ 9) the row would still
	// overflow sidebarW even with the bot's titleText="" fix above:
	// titleStyle.Padding(1,1,0,1) and descStyle's matching horizontal padding
	// each add 2 cells beyond r.width, exceeding the 10% buffer that
	// AdjustPreviewWidth carves out below sidebarW. JoinVertical then pads the
	// shorter title row up to the wider branchLine row, so the row spills past
	// the sidebar container. Drop horizontal padding on both styles at narrow
	// widths so the rendered row stays inside sidebarW (#646). Keep the top
	// padding line so the existing test's line indexing still works.
	if r.width <= 9 {
		titleS = titleS.PaddingLeft(0).PaddingRight(0)
		descS = descS.PaddingLeft(0).PaddingRight(0)
	}
	title := titleS.Render(lipgloss.JoinHorizontal(
		lipgloss.Left,
		lipgloss.Place(r.width-3, 1, lipgloss.Left, lipgloss.Center, fmt.Sprintf("%s %s", prefix, titleText)),
		" ",
		join,
	))

	remainingWidth := r.width
	remainingWidth -= runewidth.StringWidth(prefix)
	remainingWidth -= runewidth.StringWidth(branchIcon)
	remainingWidth -= 2 // for the literal " " and "-" in the branchLine format string

	// Use the mutex-guarded accessor so this read (on the renderer
	// goroutine) doesn't race with LocalBackend.Start's write on the
	// instance-creation tea.Cmd goroutine.
	branch := i.GetBranch()
	if i.Started() && hasMultipleRepos {
		repoName, err := i.RepoName()
		if err != nil {
			log.ErrorLog.Printf("could not get repo name in instance renderer: %v", err)
		} else {
			branch += fmt.Sprintf(" (%s)", repoName)
		}
	}
	// Don't show branch if there's no space for it. Or show ellipsis if it's too long.
	branchWidth := runewidth.StringWidth(branch)
	if remainingWidth < 0 {
		branch = ""
	} else if remainingWidth < branchWidth {
		if remainingWidth < 3 {
			branch = ""
		} else {
			// We know the remainingWidth is at least 4 and branch is longer than that, so this is safe.
			branch = runewidth.Truncate(branch, remainingWidth-3, "...")
		}
	}
	remainingWidth -= runewidth.StringWidth(branch)

	// Add spaces to fill the remaining width.
	spaces := ""
	if remainingWidth > 0 {
		spaces = strings.Repeat(" ", remainingWidth)
	}

	branchLine := fmt.Sprintf("%s %s-%s%s", strings.Repeat(" ", len(prefix)), branchIcon, branch, spaces)

	// Build PR info line if available
	var prLine string
	if prInfo := i.GetPRInfo(); prInfo != nil {
		prText := fmt.Sprintf("PR #%d: %s", prInfo.Number, prInfo.Title)
		prMaxWidth := r.width - len(prefix) - 2
		if prMaxWidth > 0 && runewidth.StringWidth(prText) > prMaxWidth {
			tail := "..."
			if prMaxWidth < runewidth.StringWidth(tail) {
				tail = ""
			}
			prText = runewidth.Truncate(prText, prMaxWidth, tail)
		}
		prLine = fmt.Sprintf("%s %s", strings.Repeat(" ", len(prefix)), prText)
	}

	// join title and subtitle
	lines := []string{title, descS.Render(branchLine)}
	if prLine != "" {
		lines = append(lines, descS.Render(prLine))
	}
	text := lipgloss.JoinVertical(lipgloss.Left, lines...)

	return text
}
