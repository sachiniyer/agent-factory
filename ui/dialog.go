package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

// DialogStyle is the shared frame for pickers, forms, confirmations, help and
// assistant/login hosts. Keep its geometry shared with their sizing paths:
// one vertical row and two horizontal cells of inset, with a rounded border.
func DialogStyle() lipgloss.Style {
	return theme.Styles().Dialog.Padding(1, 2)
}

// DialogTitleStyle and DialogHintStyle keep actionable copy in ink.
func DialogTitleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(theme.Roles().Ink)
}

func DialogHintStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.Roles().Ink)
}

// RenderDialog keeps the AF-owned content background continuous across nested
// Lip Gloss text styles, whose resets otherwise punch surface-coloured stripes
// into the raised dialog. This is applied before framing, never to a workspace
// frame or an agent terminal, and does not rewrite RGB values or foregrounds.
func RenderDialog(style lipgloss.Style, content string) string {
	seq := lipgloss.ColorProfile().FromColor(theme.Roles().SurfaceRaised).Sequence(true)
	if seq != "" {
		bg := termenv.CSI + seq + "m"
		reset := termenv.CSI + termenv.ResetSeq + "m"
		content = strings.ReplaceAll(content, reset, reset+bg)
	}
	return style.Render(content)
}
