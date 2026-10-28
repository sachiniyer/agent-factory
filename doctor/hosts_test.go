package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

func hostsCtx(probe func(daemon.CheckHostRequest) (daemon.CheckHostResponse, error)) *scanContext {
	return &scanContext{opts: Options{checkHosts: probe}}
}

func hostsCfg() *config.Config {
	return &config.Config{Hosts: map[string]config.HostConfig{"box": {Host: "box.example"}}}
}

func render(r *Report) string {
	var b strings.Builder
	for _, c := range r.Checks {
		b.WriteString(c.Name + "|" + c.Detail + "\n")
	}
	return b.String()
}

func TestCheckListedHosts_SilentWithoutHosts(t *testing.T) {
	r := &Report{}
	checkListedHosts(hostsCtx(func(daemon.CheckHostRequest) (daemon.CheckHostResponse, error) {
		t.Fatal("must not probe when nothing is listed")
		return daemon.CheckHostResponse{}, nil
	}), r, &config.Config{}, daemon.HealthStatus{})
	assert.Empty(t, r.Checks)
	assert.Empty(t, r.Incomplete)
}

func TestCheckListedHosts_ReportsEachVerdict(t *testing.T) {
	r := &Report{}
	checkListedHosts(hostsCtx(func(daemon.CheckHostRequest) (daemon.CheckHostResponse, error) {
		return daemon.CheckHostResponse{Hosts: []daemon.HostCheckResult{
			{Name: "ready", Probed: true, Reachable: true, Readiness: "ready"},
			{Name: "blocked", Probed: true, Reachable: true, Problems: []string{"git is not on the host's PATH"}},
			{Name: "down", Probed: true, Error: "connection refused"},
			{Name: "busy", Readiness: "stale"},
		}}, nil
	}), r, hostsCfg(), daemon.HealthStatus{})
	out := render(r)
	assert.Contains(t, out, "host ready|ready")
	assert.Contains(t, out, "host blocked|cannot run a session: git is not on the host's PATH")
	assert.Contains(t, out, "host down|could not be probed: connection refused")
	assert.Contains(t, out, "host busy|a probe was already running")
	assert.Equal(t, []string{"hosts"}, r.Incomplete, "an unreachable host is unknown, not healthy")
}

func TestCheckListedHosts_UnanswerableDaemonIsNotAPass(t *testing.T) {
	r := &Report{}
	checkListedHosts(hostsCtx(func(daemon.CheckHostRequest) (daemon.CheckHostResponse, error) {
		t.Fatal("must not probe through a daemon that is not answering")
		return daemon.CheckHostResponse{}, nil
	}), r, hostsCfg(), daemon.HealthStatus{PingErr: errors.New("no socket")})
	require.Len(t, r.Checks, 1)
	assert.Contains(t, r.Checks[0].Detail, "not probed")
	assert.Equal(t, []string{"hosts"}, r.Incomplete)

	r = &Report{}
	checkListedHosts(hostsCtx(func(daemon.CheckHostRequest) (daemon.CheckHostResponse, error) {
		return daemon.CheckHostResponse{}, errors.New("unknown method CheckHost")
	}), r, hostsCfg(), daemon.HealthStatus{})
	require.Len(t, r.Checks, 1)
	assert.Contains(t, r.Checks[0].Remediation, "af daemon restart")
	assert.Equal(t, []string{"hosts"}, r.Incomplete)
}
