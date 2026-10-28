package ui

import (
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

func dialogRecoveryStyles() theme.StyleSet {
	styles := theme.Styles()
	bg := theme.Roles().SurfaceRaised
	styles.Body = styles.Body.Background(bg)
	styles.Title = styles.Title.Background(bg)
	styles.Error = styles.Error.Background(bg)
	return styles
}

// DialogRecoveryContent retains the recovery hierarchy on the dialog surface.
func DialogRecoveryContent(condition, detail, action string, failed bool, width int) string {
	return recoveryContent(condition, detail, action, failed, width, dialogRecoveryStyles())
}

// DialogRecoveryScreen fills an empty or failed dialog without a nested surface.
func DialogRecoveryScreen(r layout.Rect, condition, detail, action string, failed bool) string {
	return recoveryScreen(r, condition, detail, action, failed, dialogRecoveryStyles())
}
