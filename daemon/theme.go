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
	// Theme is EffectNextAfLaunch, so ApplyConfig must not advance this runtime
	// projection as a side effect of applying an unrelated daemon key. cfg is the
	// immutable generation captured when this daemon started; a restarted daemon
	// receives the newly saved palette and browsers refetch it on reconnect.
	cfg := s.manager.cfg
	if cfg == nil {
		// Preserve the small direct-Manager test construction fallback that Config()
		// supports. Real managers always carry the frozen startup snapshot.
		cfg = s.manager.Config()
	}
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
