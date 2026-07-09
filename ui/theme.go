package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// Theme is the active TUI palette after resolving config.ThemeConfig into
// lipgloss colors. Defaults are Zenburn-derived (#1389).
type Theme struct {
	Foreground            lipgloss.Color
	ForegroundStrong      lipgloss.Color
	ForegroundMuted       lipgloss.Color
	ForegroundDim         lipgloss.Color
	Background            lipgloss.Color
	BackgroundSubtle      lipgloss.Color
	BackgroundPanel       lipgloss.Color
	Accent                lipgloss.Color
	Success               lipgloss.Color
	Warning               lipgloss.Color
	Error                 lipgloss.Color
	Info                  lipgloss.Color
	Purple                lipgloss.Color
	SelectionBackground   lipgloss.Color
	SelectionForeground   lipgloss.Color
	PaneBorderDefault     lipgloss.Color
	PaneBorderSelected    lipgloss.Color
	PaneBorderInteractive lipgloss.Color
	PaneBorderPreview     lipgloss.Color
}

var activeTheme = themeFromConfig(config.DefaultThemeConfig())

// AccentColor is the compatibility name for the main TUI accent. It now comes
// from the active [theme] palette (Zenburn blue #8CD0D3 by default) instead of
// the old fixed teal.
var AccentColor = activeTheme.Accent

func init() {
	ApplyTheme(config.DefaultThemeConfig())
}

func themeFromConfig(cfg config.ThemeConfig) Theme {
	return Theme{
		Foreground:            lipgloss.Color(cfg.Foreground),
		ForegroundStrong:      lipgloss.Color(cfg.ForegroundStrong),
		ForegroundMuted:       lipgloss.Color(cfg.ForegroundMuted),
		ForegroundDim:         lipgloss.Color(cfg.ForegroundDim),
		Background:            lipgloss.Color(cfg.Background),
		BackgroundSubtle:      lipgloss.Color(cfg.BackgroundSubtle),
		BackgroundPanel:       lipgloss.Color(cfg.BackgroundPanel),
		Accent:                lipgloss.Color(cfg.Accent),
		Success:               lipgloss.Color(cfg.Success),
		Warning:               lipgloss.Color(cfg.Warning),
		Error:                 lipgloss.Color(cfg.Error),
		Info:                  lipgloss.Color(cfg.Info),
		Purple:                lipgloss.Color(cfg.Purple),
		SelectionBackground:   lipgloss.Color(cfg.SelectionBackground),
		SelectionForeground:   lipgloss.Color(cfg.SelectionForeground),
		PaneBorderDefault:     lipgloss.Color(cfg.PaneBorderDefault),
		PaneBorderSelected:    lipgloss.Color(cfg.PaneBorderSelected),
		PaneBorderInteractive: lipgloss.Color(cfg.PaneBorderInteractive),
		PaneBorderPreview:     lipgloss.Color(cfg.PaneBorderPreview),
	}
}

// ApplyTheme installs the configured TUI palette and rebuilds package-level
// lipgloss styles that captured the previous colors at init time.
func ApplyTheme(cfg config.ThemeConfig) {
	activeTheme = themeFromConfig(cfg)
	AccentColor = activeTheme.Accent
	tree.ApplyTheme(tree.Theme{
		Foreground:          activeTheme.Foreground,
		ForegroundStrong:    activeTheme.ForegroundStrong,
		ForegroundMuted:     activeTheme.ForegroundMuted,
		ForegroundDim:       activeTheme.ForegroundDim,
		SelectionBackground: activeTheme.SelectionBackground,
		SelectionForeground: activeTheme.SelectionForeground,
		Success:             activeTheme.Success,
		Warning:             activeTheme.Warning,
		Error:               activeTheme.Error,
	})
	applyThemeStyles()
}

// CurrentTheme returns the active TUI palette for render-time styles.
func CurrentTheme() Theme {
	return activeTheme
}

func applyThemeStyles() {
	windowStyle = lipgloss.NewStyle().
		BorderForeground(activeTheme.Accent).
		Border(lipgloss.RoundedBorder())
	blurredWindowStyle = windowStyle.
		BorderForeground(activeTheme.PaneBorderDefault)
	interactiveWindowStyle = windowStyle.
		Border(lipgloss.DoubleBorder()).
		BorderForeground(activeTheme.PaneBorderInteractive)

	paneHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Foreground)
	paneHeaderFocusedStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.SelectionBackground).
		Foreground(activeTheme.SelectionForeground)
	paneHeaderDimStyle = lipgloss.NewStyle().
		Foreground(activeTheme.ForegroundMuted)
	paneHeaderInteractiveStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.Success).
		Foreground(activeTheme.Background)

	sectionHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Foreground)
	sectionHeaderSelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Background(activeTheme.SelectionBackground).
		Foreground(activeTheme.SelectionForeground)
	windowIndicatorStyle = lipgloss.NewStyle().
		Foreground(activeTheme.ForegroundMuted)
	mainTitle = lipgloss.NewStyle().
		Background(activeTheme.Accent).
		Foreground(activeTheme.Background)
	blurredTitle = lipgloss.NewStyle().
		Background(activeTheme.ForegroundDim).
		Foreground(activeTheme.ForegroundStrong)
	autoYesStyle = lipgloss.NewStyle().
		Background(activeTheme.SelectionBackground).
		Foreground(activeTheme.SelectionForeground)

	automationsTitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.Accent)
	automationsTitleDimStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.ForegroundMuted)
	automationsEnabledStyle = lipgloss.NewStyle().
		Foreground(activeTheme.Info)
	automationsDisabledStyle = lipgloss.NewStyle().
		Foreground(activeTheme.ForegroundMuted)
	automationItemTitleStyle = lipgloss.NewStyle().
		Foreground(tree.InstanceTitleColor)
	automationDetailStyle = lipgloss.NewStyle().
		Foreground(activeTheme.ForegroundMuted)
	automationsHintStyle = lipgloss.NewStyle().
		Foreground(activeTheme.ForegroundDim)

	keyStyle = lipgloss.NewStyle().Foreground(activeTheme.ForegroundDim)
	descStyle = lipgloss.NewStyle().Foreground(activeTheme.ForegroundMuted)
	sepStyle = lipgloss.NewStyle().Foreground(activeTheme.BackgroundSubtle)
	actionGroupStyle = lipgloss.NewStyle().Foreground(activeTheme.Accent)
	menuStyle = lipgloss.NewStyle().Foreground(activeTheme.Purple)

	tabPaneStyle = lipgloss.NewStyle().Foreground(activeTheme.Foreground)
	tabPaneFooterStyle = lipgloss.NewStyle().Foreground(activeTheme.ForegroundMuted)

	taskPlaceholderStyle = lipgloss.NewStyle().
		Faint(true).
		Foreground(activeTheme.ForegroundDim)
	taskFormMoreStyle = lipgloss.NewStyle().Foreground(activeTheme.ForegroundDim)

	alarmStyle = lipgloss.NewStyle().
		Background(activeTheme.Error).
		Foreground(activeTheme.SelectionForeground).
		Bold(true)
	errStyle = lipgloss.NewStyle().Foreground(activeTheme.Error)
}
