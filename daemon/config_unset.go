package daemon

import "github.com/sachiniyer/agent-factory/config"

// UnsetConfigValue is the admission-gated global-unset counterpart of
// SetConfigValue. The config package removes both alias spellings atomically;
// the daemon then applies the resulting effective default live.
func (s *controlServer) UnsetConfigValue(req UnsetConfigValueRequest, resp *UnsetConfigValueResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	result, err := config.UnsetGlobalConfigValue(req.Key)
	if err != nil {
		return err
	}
	resp.Result = result
	daemonApplied := false
	if s.manager != nil {
		if applied, applyErr := s.manager.ApplyConfig(); applyErr == nil {
			resp.Applied = applied.Applied
			resp.Pending = applied.Pending
			resp.Warnings = applied.Warnings
			daemonApplied = true
		}
	}
	resp.RestartNotice = config.EffectNotice(result.Key, daemonApplied)
	return nil
}
