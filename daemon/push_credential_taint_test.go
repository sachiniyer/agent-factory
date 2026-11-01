package daemon

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The machine taint (#5571 W7.8, design steer on #6433): one test per property.

func (pf *pushFixture) taint(t *testing.T, sessionID, title, why string, m session.RuntimeMachine) {
	t.Helper()
	require.NoError(t, pf.mgr.pushCreds.taints.record(session.Placement{SessionID: sessionID, Title: title, Why: why, Machine: m}, time.Now()))
}

var box1Host = session.RuntimeMachine{Host: "box1.example"}

func TestGitCredential_RefusedOnATaintedMachine(t *testing.T) {
	for name, tc := range map[string]struct {
		taint   session.RuntimeMachine
		refused bool
	}{
		"the machine, by name":                {box1Host, true},
		"the machine, by another spelling":    {session.RuntimeMachine{Host: "BOX1.example."}, true},
		"the machine, by address":             {session.RuntimeMachine{Host: "198.51.100.7"}, true},
		"another machine":                     {session.RuntimeMachine{Host: "box9.example"}, false},
		"the machine, another login user":     {session.RuntimeMachine{Host: "box1.example", User: "af-other"}, false},
		"a machine af couldn't establish":     {session.RuntimeMachine{Host: "nowhere.example"}, true},
		"an unconfirmable sandbox.ssh target": {session.RuntimeMachine{SandboxSSH: "/usr/local/bin/to-box"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			p := pf.placements["me"]
			p.Machine = session.RuntimeMachine{Host: "box1.example", User: "dev"}
			pf.placements["me"] = p
			if tc.taint.User == "" && tc.taint.SandboxSSH == "" {
				tc.taint.User = "dev"
			}
			pf.taint(t, "child-1", "child", "created by another session's af", tc.taint)
			code, got, msg := pf.askOwn(t)
			if tc.refused {
				requireRefused(t, code, got, msg, refusePushHostTainted)
				assert.Zero(t, pf.tokenReads.Load())
				return
			}
			requireAnswered(t, code, got, msg)
		})
	}
}

// Codex P1 on #6433 at b144695: a create onto a repository's own target whose
// startup fails after its hooks ran leaves no row, and its cleanup reaps the
// runtime but not a detached process. The taint was written before the backend
// ran, and it stays.
func TestTaint_AFailedCreateKeepsItsMachineTainted(t *testing.T) {
	pf := newPushFixture(t)
	t.Cleanup(session.SetPlannedRuntimeMachineForTest(func(session.InstanceOptions, string) (session.RuntimeMachine, bool, error) {
		return session.RuntimeMachine{Host: "box1.example", RepoChosen: true}, true, nil
	}))
	var taintedBeforeBackend int
	t.Cleanup(session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		taintedBeforeBackend = len(pf.mgr.pushCreds.taintsFor(box1Host.Identity()))
		return nil, errors.New("startup failed after the hooks ran")
	}))
	_, err := pf.mgr.CreateSession(context.Background(), CreateSessionRequest{Title: "repo-target", RepoPath: pf.me.Path, Program: "claude"})
	require.Error(t, err)
	assert.Equal(t, 1, taintedBeforeBackend, "the taint was written before the backend ran")

	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
	taints := pf.mgr.pushCreds.taintsFor(box1Host.Identity())
	require.Len(t, taints, 1)
	assert.Equal(t, "repo-target", taints[0].Title)
	assert.Contains(t, taints[0].Why, "repository's own [ssh] target")
}

// Codex P1 on #6433 at b144695: a session re-provisioned onto another machine
// leaves residue on the one it left. Every machine it occupied stays tainted,
// after the row is gone and across a restart.
func TestTaint_EveryMachineAReplacedRuntimeOccupiedStaysTainted(t *testing.T) {
	pf := newPushFixture(t)
	creds := newSandboxCredentials(pf.mgr, "child-1")
	require.NoError(t, creds.RecordPlacement(session.Placement{Title: "child", Why: "created by another session's af", Machine: box1Host}))
	require.NoError(t, creds.RecordPlacement(session.Placement{Title: "child", Why: "created by another session's af", Machine: session.RuntimeMachine{Host: "box9.example"}}))

	restarted := &pushTaintState{path: pf.mgr.pushCreds.taints.path}
	for _, m := range []session.RuntimeMachine{box1Host, {Host: "box9.example"}} {
		got, err := restarted.matching(m.Identity())
		require.NoError(t, err)
		assert.Lenf(t, got, 1, "%s is still tainted after a restart", m.Host)
	}
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
}

// Only an explicit reset clears a taint, and a reset of one host clears only
// what is known to be on it: a taint af couldn't place on any machine is cleared
// only by --all.
func TestResetTrust_ClearsOnlyWhatIsOnThatMachine(t *testing.T) {
	pf := newPushFixture(t)
	pf.taint(t, "child-1", "child", "created by another session's af", box1Host)
	pf.taint(t, "child-2", "child2", "created by another session's af", session.RuntimeMachine{Host: "box9.example"})
	cs := &controlServer{manager: pf.mgr}

	var resp ResetPushTrustResponse
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "box1"}, &resp))
	require.Len(t, resp.Cleared, 1)
	assert.Equal(t, "child-1", resp.Cleared[0].SessionID)
	assert.Contains(t, resp.Identity.Names, "box1.example", "the identities reset are shown")
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	left, err := pf.mgr.pushCreds.taints.matching(session.RuntimeMachine{Host: "box9.example"}.Identity())
	require.NoError(t, err)
	assert.Len(t, left, 1, "another machine's taint stays")

	pf.taint(t, "legacy-1", "legacy", "of unknown provenance", session.RuntimeMachine{SandboxSSH: "/usr/local/bin/to-box"})
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "box1"}, &resp))
	assert.Empty(t, resp.Cleared, "a taint on no known machine is not this host's to clear")
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)

	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{All: true}, &resp))
	assert.Len(t, resp.Cleared, 2)
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	assert.Error(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "not-a-host"}, &resp), "an unlisted name")
}

// A record that can't be read taints every machine until it can.
func TestTaint_AnUnreadableRecordWithholdsEveryHost(t *testing.T) {
	pf := newPushFixture(t)
	require.NoError(t, os.WriteFile(pf.mgr.pushCreds.taints.path, []byte("{not json"), 0o600))
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
	resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
	pf.mgr.fillPushCredentialBlockers(pf.mgr.Config(), resp)
	require.NotEmpty(t, resp.Hosts[0].PushCredentialBlockers)
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "can't be read")
}

// Licensing a tainted machine is refused up front, before anything is written,
// with the reason and the reset command. Turning a licence off, and an entry
// already licensed, are never refused.
func TestLicensingATaintedMachineIsRefusedUpFront(t *testing.T) {
	pf := newPushFixture(t)
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": c.Hosts["box1"], "box9": {Host: "box9.example"}}
		c.SandboxSSH = "ssh freebox"
	})
	pf.taint(t, "child-1", "child", "created by another session's af", session.RuntimeMachine{Host: "box9.example"})
	pf.taint(t, "child-2", "child2", "created by another session's af", box1Host)
	pf.taint(t, "legacy-1", "legacy", "of unknown provenance", session.RuntimeMachine{SandboxSSH: "ssh freebox"})

	licensed := config.HostConfig{Host: "box9.example", PushCredentials: true}
	err := pf.mgr.refuseLicensingTainted(SetConfigValueRequest{HostsEdit: &config.HostsEdit{Op: config.HostsEditAdd, Name: "box9", Host: &licensed}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to opt box9 into push credentials")
	assert.Contains(t, err.Error(), "af hosts reset-trust box9")

	plain := config.HostConfig{Host: "box9.example"}
	assert.NoError(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{HostsEdit: &config.HostsEdit{Op: config.HostsEditAdd, Name: "box9", Host: &plain}}))
	assert.Error(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{Key: "hosts",
		Value: `{"box1":{"host":"box1.example","push_credentials":true},"box9":{"host":"box9.example","push_credentials":true}}`}), "the hosts table newly licensing box9")
	assert.NoError(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{Key: "hosts",
		Value: `{"box1":{"host":"box1.example","push_credentials":true}}`}), "box1 was already licensed: an unchanged entry is not refused")
	assert.Error(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{Key: "sandbox_push_credentials", Value: "true"}), "the legacy spelling too")
	assert.NoError(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{Key: "sandbox.push_credentials", Value: "false"}))

	// And SetConfigValue refuses before it writes.
	cs := &controlServer{manager: pf.mgr}
	var resp SetConfigValueResponse
	err = cs.SetConfigValue(SetConfigValueRequest{HostsEdit: &config.HostsEdit{Op: config.HostsEditAdd, Name: "box9b", Host: &licensed}}, &resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to opt box9b into push credentials")
	cfg, lerr := config.LoadConfig()
	require.NoError(t, lerr)
	_, written := cfg.Hosts["box9b"]
	assert.False(t, written, "nothing was written")
}

// `af hosts check` shows each taint, with the session that made it as context,
// on a licensed host and on one that isn't licensed yet.
func TestCheckHostShowsEachTaintAsContext(t *testing.T) {
	pf := newPushFixture(t)
	pf.taint(t, "child-1", "child", "created by another session's af", box1Host)
	pf.taint(t, "repo-1", "repo", "on a repository's own [ssh] target, which the repository chose", session.RuntimeMachine{Host: "box9.example"})
	cfg := &config.Config{Hosts: map[string]config.HostConfig{
		"box1": {Host: "box1.example", PushCredentials: true},
		"box9": {Host: "box9.example"},
		"box2": {Host: "box2.example"},
	}}
	resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}, {Name: "box9"}, {Name: "box2"}}}
	pf.mgr.fillPushCredentialBlockers(cfg, resp)

	require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1)
	line := resp.Hosts[0].PushCredentialBlockers[0]
	assert.Contains(t, line, "tainted since")
	assert.Contains(t, line, `session "child" (child-1)`)
	assert.Contains(t, line, "af hosts reset-trust box1")
	require.Len(t, resp.Hosts[1].PushCredentialBlockers, 1, "an unlicensed host shows its taint too: it can't be licensed until reset")
	assert.Contains(t, resp.Hosts[1].PushCredentialBlockers[0], "af hosts reset-trust box9")
	assert.Empty(t, resp.Hosts[2].PushCredentialBlockers)
}

// A restored row the operator didn't place taints its machine before any push
// credential is answered; an operator's own does not.
func TestRestoredRowsTheOperatorDidNotPlaceAreTaintedFirst(t *testing.T) {
	pf := newPushFixture(t)
	pf.mgr.pushCreds.taints.restored.Store(false)
	pf.track(t, session.InstanceData{ID: "child-1", Title: "child", CreatedBySandbox: "x"})
	pf.track(t, session.InstanceData{ID: "op-1", Title: "op", CreatedByOperator: true})
	pf.placeOn(t, "child", session.RuntimeMachine{Host: "box9.example"})
	pf.placeOn(t, "op", box1Host)

	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushTaintsLoading)

	pf.mgr.recordRestoredPlacements()
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	taints, err := pf.mgr.pushCreds.taints.matching(session.RuntimeMachine{Host: "box9.example"}.Identity())
	require.NoError(t, err)
	require.Len(t, taints, 1)
	assert.Equal(t, "child-1", taints[0].SessionID)
	mine, err := pf.mgr.pushCreds.taints.matching(box1Host.Identity())
	require.NoError(t, err)
	assert.Empty(t, mine, "the operator's own session taints nothing")
}
