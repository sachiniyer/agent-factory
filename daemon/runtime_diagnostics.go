package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// ServingDaemonSupervised correlates the one process which answered Ping with
// the installed unit's live MainPID. It never infers a negative from a failed
// service-manager probe: missing evidence stays Undetermined.
func ServingDaemonSupervised(h HealthStatus, supervision SupervisionInfo) ProbeAnswer {
	if h.PingErr != nil {
		return Undetermined(fmt.Errorf("no daemon answered Ping: %w", h.PingErr))
	}
	if !supervision.Supported {
		return Undetermined(errors.New("this platform has no supported autostart supervision probe"))
	}
	if !supervision.UnitPresent {
		return AnswerNo()
	}

	var result ProbeAnswer
	supervision.Active.Match(
		func() {
			if h.ServingPID <= 0 {
				result = Undetermined(errors.New("the responding daemon predates Ping PID reporting"))
				return
			}
			supervision.MainPIDPresent.Match(
				func() {
					if supervision.MainPID == h.ServingPID {
						result = AnswerYes()
					} else {
						result = AnswerNo()
					}
				},
				func() { result = AnswerNo() },
				func() { result = AnswerNo() },
				func(cause error) { result = Undetermined(cause) },
			)
		},
		func() { result = AnswerNo() },
		func() { result = AnswerNo() },
		func(cause error) { result = Undetermined(cause) },
	)
	return result
}

// RunningConfigMatches reports whether the listener/auth posture on disk is
// the posture the responding daemon actually booted with. An older responder
// or a failed config load is an evidence gap, not a mismatch.
func RunningConfigMatches(h HealthStatus, current *config.Config) ProbeAnswer {
	if h.PingErr != nil {
		return Undetermined(fmt.Errorf("no daemon answered Ping: %w", h.PingErr))
	}
	if h.BootConfig == nil {
		return Undetermined(errors.New("the responding daemon predates boot-config reporting"))
	}
	if current == nil {
		return Undetermined(errors.New("the on-disk config could not be loaded"))
	}
	if strings.TrimSpace(h.BootConfig.ListenAddr) == strings.TrimSpace(current.ListenAddr) &&
		h.BootConfig.RequireToken == current.RequireToken &&
		h.BootConfig.RequireLoopbackToken == current.RequireLoopbackToken {
		return AnswerYes()
	}
	return AnswerNo()
}

// RunningConfigDifference renders only the non-secret fields which differ.
// Empty means no known difference (including unavailable evidence).
func RunningConfigDifference(boot *DaemonBootConfig, current *config.Config) string {
	if boot == nil || current == nil {
		return ""
	}
	var diffs []string
	if strings.TrimSpace(boot.ListenAddr) != strings.TrimSpace(current.ListenAddr) {
		diffs = append(diffs, fmt.Sprintf("listen_addr: running %q, file %q", boot.ListenAddr, current.ListenAddr))
	}
	if boot.RequireToken != current.RequireToken {
		diffs = append(diffs, fmt.Sprintf("require_token: running %t, file %t", boot.RequireToken, current.RequireToken))
	}
	if boot.RequireLoopbackToken != current.RequireLoopbackToken {
		diffs = append(diffs, fmt.Sprintf("require_loopback_token: running %t, file %t",
			boot.RequireLoopbackToken, current.RequireLoopbackToken))
	}
	return strings.Join(diffs, "; ")
}
