package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// GetTheme returns the palette this daemon is running with. Unlike GetConfig,
// this is a runtime read: every renderer connected to the daemon must see the
// same generation that ApplyConfig made live.
func (s *controlServer) GetTheme(_ GetThemeRequest, resp *GetThemeResponse) error {
	if s.manager == nil {
		return fmt.Errorf("daemon config is unavailable")
	}
	// Snapshot once so all nineteen slots belong to one ApplyConfig generation.
	// An ApplyConfig request makes an edit live immediately; a direct edit is pulled
	// across the daemon boundary by the next TUI launch before that renderer mounts.
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

// ApplyTheme is the narrow launch-boundary counterpart to ApplyConfig. A TUI
// launch may advance the shared palette, but must never silently apply unrelated
// listener/auth edits whose failures require an operator-facing save response.
func (s *controlServer) ApplyTheme(_ ApplyThemeRequest, resp *ApplyThemeResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if s.manager == nil {
		return nil
	}
	changed, err := s.manager.ApplyTheme()
	if err != nil {
		return err
	}
	resp.Changed = changed
	return nil
}
