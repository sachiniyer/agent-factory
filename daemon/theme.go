package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// GetTheme returns the palette this daemon is running with. Unlike GetConfig,
// this is a runtime read: a renderer must match the daemon process it is
// connected to, not a hand-edit that will only take effect on a later launch.
func (s *controlServer) GetTheme(_ GetThemeRequest, resp *GetThemeResponse) error {
	if s.manager == nil {
		return fmt.Errorf("daemon config is unavailable")
	}
	cfg := s.manager.Config()
	if cfg == nil {
		return fmt.Errorf("daemon config is unavailable")
	}
	t := cfg.Theme
	resp.Theme = apiproto.Theme{
		Name:                  t.Preset(),
		Foreground:            t.Foreground,
		ForegroundStrong:      t.ForegroundStrong,
		ForegroundMuted:       t.ForegroundMuted,
		ForegroundDim:         t.ForegroundDim,
		Background:            t.Background,
		BackgroundSubtle:      t.BackgroundSubtle,
		BackgroundPanel:       t.BackgroundPanel,
		Accent:                t.Accent,
		Success:               t.Success,
		Warning:               t.Warning,
		Error:                 t.Error,
		Info:                  t.Info,
		Purple:                t.Purple,
		SelectionBackground:   t.SelectionBackground,
		SelectionForeground:   t.SelectionForeground,
		PaneBorderDefault:     t.PaneBorderDefault,
		PaneBorderSelected:    t.PaneBorderSelected,
		PaneBorderInteractive: t.PaneBorderInteractive,
		PaneBorderPreview:     t.PaneBorderPreview,
	}
	return nil
}
