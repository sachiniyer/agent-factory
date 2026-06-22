package ui

import "github.com/charmbracelet/lipgloss"

// AccentColor is the single source of truth for the UI accent (teal #7cb8bb,
// introduced in #932). Every accent surface — the sidebar title banner, active
// tab/window borders, overlay borders, menu action groups, task edit-mode
// chrome, and the help screen titles — routes through this constant so the
// accent can never drift again the way the old purple Color("62") banner did
// (#950). It is intentionally a plain lipgloss.Color: #932 used the same hex
// for both light and dark, so no AdaptiveColor split is needed, and a single
// Color drops in cleanly at every Foreground/Background/BorderForeground site.
//
// This is the accent and nothing else. Semantic colors — success green
// (#51bd73), error red, menu pink (Color("205")), and the recede/muted greys —
// are deliberately NOT part of the accent and must not be folded in here.
const AccentColor = lipgloss.Color("#7cb8bb")
