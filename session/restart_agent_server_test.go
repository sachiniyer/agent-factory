package session

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crashedHost stands in for a sandbox whose agent-server crashed: the loopback
// server answers alive only once the (fake) host has run the restart script, and
// the host's probe answers what the test dictates.
type crashedHost struct {
	mu        sync.Mutex
	restarted bool
	scripts   []string
	lifecycle []string
	probe     string
	// failDial makes the transport refuse to open once the server was restarted.
	failDial bool
	// failRoute names an adoption RPC ("provision"/"launch") the server refuses.
	failRoute string
	// badBanner makes the restarted server report another session's credential.
	badBanner bool
	// hostPID is what the host's launch record (agent-server.pid) holds, and
	// strayProbe what the identity probe answers for that PID.
	hostPID, strayProbe string
}

func newCrashedHost(t *testing.T, probe string) (*Instance, *sandboxProvisioner, *crashedHost) {
	t.Helper()
	inst, p := reattachFixture(t, "")
	key, err := runtimetoken.LoadKey()
	require.NoError(t, err)
	token, err := runtimetoken.DeriveAgentServerToken(key, p.tokenRef)
	require.NoError(t, err)

	h := &crashedHost{probe: probe}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		up := h.restarted
		h.mu.Unlock()
		if !up {
			http.Error(w, "not listening", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": agentAliveResp{Alive: true}})
	})
	for _, route := range []string{"provision", "launch"} {
		route := route
		mux.HandleFunc("/v1/agent/"+route, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.lifecycle = append(h.lifecycle, route+" "+strings.TrimSpace(string(body)))
			refuse := h.failRoute == route
			h.mu.Unlock()
			if refuse {
				http.Error(w, "refused", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]bool{"ok": true}})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var dialed []string
	p.startTunnelFn = func(addr string) (string, error) {
		dialed = append(dialed, addr)
		h.mu.Lock()
		refuse := h.failDial && h.restarted
		h.mu.Unlock()
		if refuse {
			return "", errors.New("transient dial failure")
		}
		return srv.Listener.Addr().String(), nil
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("dialed %v", dialed)
		}
	})
	p.remotePID = "4242"
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.scripts = append(h.scripts, script)
		switch {
		case strings.Contains(script, "agent-server.pid") && strings.HasPrefix(script, "if [ -r"):
			return []byte(h.hostPID + "\n"), nil
		case strings.Contains(script, "matches_session") && h.hostPID != "" && strings.Contains(script, "pid='"+h.hostPID+"'"):
			return []byte(h.strayProbe + "\n"), nil
		case strings.Contains(script, "matches_session"):
			return []byte(h.probe + "\n"), nil
		case strings.Contains(script, "echo relaunched"):
			h.restarted = true
			return []byte("4343\n"), nil
		case strings.HasPrefix(script, "cat ") && h.badBanner:
			return []byte(`{"addr":"127.0.0.1:9999","token":"someone-else","version":"9.9.9"}` + "\n" + remoteProcAlive + "\n"), nil
		case strings.HasPrefix(script, "cat "):
			return []byte(`{"addr":"127.0.0.1:9999","token":"` + token + `","version":"9.9.9"}` + "\n" + remoteProcAlive + "\n"), nil
		}
		return nil, nil
	}
	return inst, p, h
}

func (h *crashedHost) ran(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.scripts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestReattachRestartsACrashedAgentServerAndAdoptsItsSession(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"

	require.NoError(t, inst.ReattachSandbox())

	assert.True(t, h.ran("echo relaunched"), "the persisted launch command is what restarts it")
	assert.Equal(t, "4343", p.remotePID, "the reap must see the live PID")
	assert.Equal(t, "127.0.0.1:9999", p.remoteAddr, "the new port is what the transport re-dials")
	assert.Equal(t, "9.9.9", p.agentServerVersion)
	assert.NotNil(t, inst.remoteClient)

	// Adoption is the restore path, never a first-time launch: the surviving
	// tmux session keeps its agent.
	require.Len(t, h.lifecycle, 2)
	assert.Contains(t, h.lifecycle[0], "provision")
	assert.Contains(t, h.lifecycle[1], "launch")
	for _, call := range h.lifecycle {
		assert.Contains(t, call, `"first_time_setup":false`)
	}

	// The persisted handle follows, or the next daemon start would dial a dead port.
	cleanup := inst.backend.(*sandboxBackend).cleanup
	assert.Equal(t, "4343", cleanup.RemotePID)
	assert.Equal(t, "127.0.0.1:9999", cleanup.RemoteAddr)
}

func TestReattachDoesNotRestartWithoutProofTheServerIsDead(t *testing.T) {
	for _, tc := range []struct{ name, probe, want string }{
		{"still running but unresponsive", agentServerAliveMark, "still running"},
		{"the host cannot tell", agentServerUnknown, "could not tell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, p, h := newCrashedHost(t, tc.probe)
			p.launchScript = "echo relaunched & echo $!"

			err := inst.ReattachSandbox()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, h.ran("echo relaunched"), "a second agent-server must never start beside a possibly live one")
			assert.Nil(t, inst.remoteClient)
			assert.Equal(t, "4242", p.remotePID)
		})
	}
}

func TestReattachDoesNotRestartARowThatNeverRecordedItsLaunch(t *testing.T) {
	inst, _, h := newCrashedHost(t, agentServerProvedDead)

	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates restartable launches")
	assert.False(t, h.ran("matches_session"), "no probe is spent on a row that cannot be restarted")
}

func TestAgentServerProbeNeverKillsAndAgreesWithTheReapOnIdentity(t *testing.T) {
	probe := remotePIDIdentityProbeScript("4242", "/s/af", "ref")
	kill := remotePIDIdentityKillScript("4242", "/s/af", "ref")
	assert.NotContains(t, probe, "kill \"$pid\"")
	assert.NotContains(t, probe, "kill -9")
	prelude := remotePIDIdentityFuncs("4242", "/s/af", "ref")
	assert.True(t, strings.HasPrefix(probe, prelude) && strings.HasPrefix(kill, prelude),
		"the probe and the reap share one definition of 'this row's agent-server'")
}

// A restarted server this attempt cannot reach is stopped and its identity
// persisted: left running it would answer the next reattach's probe, which would
// then bind it without ever adopting the surviving session.
func TestReattachStopsAnUnadoptedRestartWhenTheSecondDialFails(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	h.failDial = true

	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.True(t, h.ran("kill \"$pid\""), "the unadopted server is stopped")
	assert.Empty(t, h.lifecycle, "no adoption was attempted")
	assert.Nil(t, inst.remoteClient)
	cleanup := inst.backend.(*sandboxBackend).cleanup
	assert.Equal(t, "4343", cleanup.RemotePID, "the persisted identity follows the restart so a retry probes the right process")
	assert.Equal(t, "127.0.0.1:9999", cleanup.RemoteAddr)
}

// Every failure exit of a restart keeps the workspace, records every process af
// started, and leaves no stale writer (#5995 review).

// Provision/Launch failing means the new server may hold the surviving
// worktree, and an ordinary agent-server deletes its worktree on SIGTERM. It is
// left running and recorded as owing adoption; the next reattach finishes it.
func TestReattachLeavesAHalfAdoptedRestartRunningAndFinishesItNextTime(t *testing.T) {
	for _, route := range []string{"provision", "launch"} {
		t.Run(route+" refused", func(t *testing.T) {
			inst, p, h := newCrashedHost(t, agentServerProvedDead)
			p.launchScript = "echo relaunched & echo $!"
			h.failRoute = route

			err := inst.ReattachSandbox()
			require.Error(t, err)
			assert.False(t, h.ran("kill \"$pid\""), "a server that may hold the workspace is never signalled")
			assert.Nil(t, inst.remoteClient)
			cleanup := inst.backend.(*sandboxBackend).cleanup
			assert.Equal(t, "4343", cleanup.RemotePID, "the running server is durably recorded")
			assert.True(t, cleanup.AdoptPending, "and so is the adoption it still owes")

			// Next attempt: the server answers, so it is adopted rather than just bound.
			h.mu.Lock()
			h.failRoute, h.lifecycle = "", nil
			h.mu.Unlock()
			require.NoError(t, inst.ReattachSandbox())
			require.Len(t, h.lifecycle, 2, "the pending adoption is completed")
			assert.NotNil(t, inst.remoteClient)
			assert.False(t, inst.backend.(*sandboxBackend).cleanup.AdoptPending)
		})
	}
}

// A banner that never arrives (or carries another credential) leaves a process
// af started: it is stopped (nothing has touched the workspace yet) and its PID
// persisted, not just held in memory.
func TestReattachStopsAndRecordsARestartWhoseBannerIsWrong(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	h.badBanner = true

	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.True(t, h.ran("kill \"$pid\""), "the stranger-credentialed process is stopped")
	assert.Empty(t, h.lifecycle, "nothing was adopted")
	cleanup := inst.backend.(*sandboxBackend).cleanup
	assert.Equal(t, "4343", cleanup.RemotePID, "the persisted PID follows the spawn")
}

// The proof that no agent-server runs covers the PID the HOST recorded at the
// last launch as well as the one the row holds: a daemon that died between a
// spawn and its own persist left a server only the host names.
func TestReattachDoesNotRestartBesideAServerOnlyTheHostRecorded(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	h.hostPID, h.strayProbe = "5555", agentServerAliveMark

	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "5555")
	assert.False(t, h.ran("echo relaunched"), "a second agent-server must never start beside one the host recorded")
}

func TestRestartRecordsTheDebtDurablyBeforeSpawningAndTheIdentityBeforeAdopting(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	type snap struct {
		spawned   bool
		pending   bool
		pid       string
		lifecycle int
	}
	var snaps []snap
	inst.SetRuntimeIdentityPersist(func(i *Instance) error {
		c := i.backend.(*sandboxBackend).cleanup
		h.mu.Lock()
		defer h.mu.Unlock()
		snaps = append(snaps, snap{h.restarted, c.AdoptPending, c.RemotePID, len(h.lifecycle)})
		return nil
	})

	require.NoError(t, inst.ReattachSandbox())
	require.GreaterOrEqual(t, len(snaps), 3)
	assert.Equal(t, snap{spawned: false, pending: true, pid: "4242"}, snaps[0], "the debt is on disk before any process exists")
	assert.Equal(t, snap{spawned: true, pending: true, pid: "4343"}, snaps[1], "the identity is on disk before any adoption RPC")
	last := snaps[len(snaps)-1]
	assert.False(t, last.pending, "the debt is cleared only after the adoption succeeded")
	assert.Equal(t, 2, last.lifecycle)
}

func TestRestartDoesNotSpawnWhenTheDebtCannotBeRecorded(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.SetRuntimeIdentityPersist(func(*Instance) error { return errors.New("disk full") })

	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.False(t, h.ran("echo relaunched"), "no process is spawned that the row does not yet name")
	assert.False(t, p.adoptPending)
}

func TestRestartStopsAServerItCannotRecordBeforeAdopting(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	calls := 0
	inst.SetRuntimeIdentityPersist(func(*Instance) error {
		calls++
		if calls == 2 { // the post-spawn identity write
			return errors.New("disk full")
		}
		return nil
	})

	require.Error(t, inst.ReattachSandbox())
	assert.True(t, h.ran("kill \"$pid\""), "a workspace-free server the row cannot name is stopped")
	assert.Empty(t, h.lifecycle, "and nothing was adopted")
}

// A restart-adoption cannot prove the agent survived, so a task run in flight
// ends interrupted BEFORE the endpoint is bound and observations resume.
func TestRestartAdoptionInterruptsAnInFlightTaskRunBeforeBinding(t *testing.T) {
	inst, p, _ := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.mu.Lock()
	inst.taskRunActive = true
	inst.mu.Unlock()

	require.NoError(t, inst.ReattachSandbox())
	inst.mu.RLock()
	active := inst.taskRunActive
	inst.mu.RUnlock()
	assert.False(t, active, "the old run must not be completable by the replacement's first idle tick")
	assert.True(t, inst.TaskRunInterrupted())
	assert.True(t, inst.ConsumeTaskRunInterrupted(), "the daemon stamps the owning task from this")
}

// After an agent swap the stored launch script names the OLD agent; the row is
// reported not restartable instead of being restarted as the wrong program.
func TestAgentSwapRetiresTheStoredLaunchScript(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.backend.(*sandboxBackend).cleanup.LaunchScript = p.launchScript

	inst.retireLaunchScript()

	assert.Empty(t, inst.backend.(*sandboxBackend).cleanup.LaunchScript)
	err := inst.ReattachSandbox()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates restartable launches")
	assert.False(t, h.ran("echo relaunched"))
}
