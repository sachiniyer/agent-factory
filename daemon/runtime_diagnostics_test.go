package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestServingDaemonSupervised_CorrelatesResponderAndManager(t *testing.T) {
	responding := HealthStatus{ServingPID: 42}
	cases := []struct {
		name string
		h    HealthStatus
		sup  SupervisionInfo
		want string
	}{
		{
			name: "same live pid",
			h:    responding,
			sup: SupervisionInfo{Supported: true, UnitPresent: true, Active: AnswerYes(),
				MainPID: 42, MainPIDPresent: AnswerYes()},
			want: "yes",
		},
		{
			name: "different live pid",
			h:    responding,
			sup: SupervisionInfo{Supported: true, UnitPresent: true, Active: AnswerYes(),
				MainPID: 99, MainPIDPresent: AnswerYes()},
			want: "no",
		},
		{
			name: "installed unit inactive",
			h:    responding,
			sup:  SupervisionInfo{Supported: true, UnitPresent: true, Active: AnswerNo()},
			want: "no",
		},
		{
			name: "no unit",
			h:    responding,
			sup:  SupervisionInfo{Supported: true},
			want: "no",
		},
		{
			name: "older responder has no pid",
			h:    HealthStatus{},
			sup: SupervisionInfo{Supported: true, UnitPresent: true, Active: AnswerYes(),
				MainPID: 42, MainPIDPresent: AnswerYes()},
			want: "unknown",
		},
		{
			name: "manager query failed",
			h:    responding,
			sup: SupervisionInfo{Supported: true, UnitPresent: true,
				Active: Undetermined(errors.New("user bus unavailable"))},
			want: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ServingDaemonSupervised(tc.h, tc.sup).String())
		})
	}
}

func TestRunningConfigMatches_ThreeValued(t *testing.T) {
	current := &config.Config{ListenAddr: "127.0.0.1:8443", RequireToken: true}
	cases := []struct {
		name string
		h    HealthStatus
		want string
	}{
		{
			name: "same posture",
			h: HealthStatus{BootConfig: &DaemonBootConfig{
				ListenAddr: "127.0.0.1:8443", RequireToken: true,
			}},
			want: "yes",
		},
		{
			name: "listener changed",
			h: HealthStatus{BootConfig: &DaemonBootConfig{
				ListenAddr: "0.0.0.0:8443", RequireToken: true,
			}},
			want: "no",
		},
		{
			name: "auth changed",
			h: HealthStatus{BootConfig: &DaemonBootConfig{
				ListenAddr: "127.0.0.1:8443", RequireToken: false,
			}},
			want: "no",
		},
		{
			name: "loopback auth changed",
			h: HealthStatus{BootConfig: &DaemonBootConfig{
				ListenAddr: "127.0.0.1:8443", RequireToken: true, RequireLoopbackToken: true,
			}},
			want: "no",
		},
		{name: "older responder", h: HealthStatus{}, want: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RunningConfigMatches(tc.h, current).String())
		})
	}
}

func TestRunningConfigDifference_NamesOnlyChangedPosture(t *testing.T) {
	boot := &DaemonBootConfig{ListenAddr: "0.0.0.0:8443", RequireToken: false}
	current := &config.Config{
		ListenAddr: "127.0.0.1:8443", RequireToken: true, RequireLoopbackToken: true,
	}

	diff := RunningConfigDifference(boot, current)
	require.Contains(t, diff, `listen_addr: running "0.0.0.0:8443", file "127.0.0.1:8443"`)
	require.Contains(t, diff, "require_token: running false, file true")
	require.Contains(t, diff, "require_loopback_token: running false, file true")
}
