package daemon

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
