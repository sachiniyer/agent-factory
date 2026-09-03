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
	// Carry the WHOLE apply outcome to the notice, not just "the apply call returned
	// nil" (#3397). ApplyConfig succeeding does not mean a network.listen_addr /
	// network.preview_listen_addr rebind succeeded — a failed one keeps the OLD
	// listener serving and lands in FailedListenerKeys — and this handler used to
	// drop that half, so an unset whose rebind failed claimed "Applied" on stdout
	// while resp.Warnings said the opposite. config.EffectNotice owns that decision
	// now, so the two set surfaces and the two unset surfaces cannot diverge again.
	var outcome config.ApplyOutcome
	if s.manager != nil {
		if applied, applyErr := s.manager.ApplyConfig(); applyErr == nil {
			resp.Applied = applied.Applied
			resp.Pending = applied.Pending
			resp.Warnings = applied.Warnings
			outcome = config.ApplyOutcome{DaemonApplied: true, FailedListenerKeys: applied.FailedListenerKeys}
		}
	}
	resp.RestartNotice = config.EffectNotice(result.Key, outcome)
	// Where the daemon is accepting now, for a listener key (#3722). Same read as
	// SetConfigValue's, after the apply for the same reason: clearing
	// network.listen_addr moves the listener back to its default address, and the
	// caller may have been talking over the one it moved off.
	if s.manager != nil {
		resp.ListenerAddr = s.manager.ListenerAddress(result.Key)
	}
	return nil
}
