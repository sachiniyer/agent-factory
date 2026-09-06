package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// Theme contains fixed generated roles used by existing component boundaries.
type Theme = theme.Palette

var activeTheme = themeFromConfig(config.DefaultThemeConfig())

// AccentColor is the shared selection and focus accent.
var AccentColor = activeTheme.Accent

func init() {
	ApplyTheme(config.DefaultThemeConfig())
}

func themeFromConfig(_ config.ThemeConfig) Theme { return theme.Roles() }

// ApplyTheme rebuilds styles from fixed generated roles. Legacy palette input
// is intentionally ignored; appearance selection is added in P5 slice C.
func ApplyTheme(cfg config.ThemeConfig) {
	activeTheme = themeFromConfig(cfg)
	AccentColor = activeTheme.Accent
	tree.ApplyTheme(tree.Theme{
		Foreground:          activeTheme.Ink,
		ForegroundStrong:    activeTheme.Ink,
		ForegroundMuted:     activeTheme.InkMuted,
		ForegroundDim:       activeTheme.InkMuted,
		SelectionBackground: activeTheme.SurfaceRaised,
		SelectionForeground: activeTheme.Ink,
		Success:             activeTheme.Ready,
		Warning:             activeTheme.Lost,
		Error:               activeTheme.Dead,
	})
	applyThemeStyles()
}

// CurrentTheme returns the active TUI palette for render-time styles.
func CurrentTheme() Theme {
	return activeTheme
}

func applyThemeStyles() {
	windowStyle = lipgloss.NewStyle().
		BorderForeground(activeTheme.Border).
		Border(lipgloss.RoundedBorder())
	blurredWindowStyle = windowStyle.
		BorderForeground(activeTheme.Border)
	selectedWindowStyle = windowStyle.
		BorderForeground(activeTheme.Accent)
	interactiveWindowStyle = windowStyle.
		Border(lipgloss.DoubleBorder()).
		BorderForeground(activeTheme.Accent)
	previewWindowStyle = windowStyle.
		BorderForeground(activeTheme.Border)
	dropTargetWindowStyle = windowStyle.
		BorderForeground(activeTheme.Accent)

	paneHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Ink)
	paneHeaderFocusedStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.SurfaceRaised).
		Foreground(activeTheme.Ink)
	paneHeaderDimStyle = lipgloss.NewStyle().
		Foreground(activeTheme.InkMuted)
	paneHeaderInteractiveStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.SurfaceRaised).
		Foreground(activeTheme.Ink)

	sectionHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Ink)
	sectionHeaderSelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.SurfaceRaised).
		Foreground(activeTheme.Ink)
	windowIndicatorStyle = lipgloss.NewStyle().
		Foreground(activeTheme.InkMuted)
	mainTitle = lipgloss.NewStyle().
		Background(activeTheme.Accent).
		Foreground(activeTheme.Surface)
	blurredTitle = lipgloss.NewStyle().
		Background(activeTheme.InkMuted).
		Foreground(activeTheme.Ink)
	projectRowStyle = lipgloss.NewStyle().
		Foreground(activeTheme.Ink)
	projectRowActiveStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Accent)
	projectRowSelectedStyle = lipgloss.NewStyle().
		Background(activeTheme.SurfaceRaised).
		Foreground(activeTheme.Ink)

	automationsTitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Accent)
	automationsTitleDimStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.InkMuted)
	automationsEnabledStyle = lipgloss.NewStyle().
		Foreground(activeTheme.Ink)
	automationsDisabledStyle = lipgloss.NewStyle().
		Foreground(activeTheme.InkMuted)
	automationItemTitleStyle = lipgloss.NewStyle().
		Foreground(tree.InstanceTitleColor)
	automationDetailStyle = lipgloss.NewStyle().
		Foreground(activeTheme.InkMuted)
	automationsHintStyle = lipgloss.NewStyle().
		Foreground(activeTheme.InkMuted)

	keyStyle = lipgloss.NewStyle().Foreground(activeTheme.InkMuted)
	descStyle = lipgloss.NewStyle().Foreground(activeTheme.InkMuted)
	sepStyle = lipgloss.NewStyle().Foreground(activeTheme.Border)
	actionGroupStyle = lipgloss.NewStyle().Foreground(activeTheme.Accent)
	menuStyle = lipgloss.NewStyle().Foreground(activeTheme.Ink)

	tabPaneStyle = lipgloss.NewStyle().Foreground(activeTheme.Ink)

	taskPlaceholderStyle = lipgloss.NewStyle().
		Faint(true).
		Foreground(activeTheme.InkMuted)
	taskFormMoreStyle = lipgloss.NewStyle().Foreground(activeTheme.InkMuted)

	alarmStyle = lipgloss.NewStyle().
		Background(activeTheme.Surface).
		Foreground(activeTheme.Dead).
		Bold(true)
	errStyle = lipgloss.NewStyle().Foreground(activeTheme.Dead)
}
