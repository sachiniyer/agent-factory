package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The machine taint (#5571 W7.8, design steer and reviews on #6433): one test per
// property.

func (pf *pushFixture) taint(t *testing.T, sessionID, title, why string, m session.RuntimeMachine) {
	t.Helper()
	require.NoError(t, pf.mgr.pushCreds.taints.record(session.Placement{SessionID: sessionID, Title: title, Why: why, Machine: m}, m.Identity(), time.Now()))
}

func (pf *pushFixture) taintIdentity(t *testing.T, sessionID string, id session.MachineIdentity) {
	t.Helper()
	require.NoError(t, pf.mgr.pushCreds.taints.record(session.Placement{SessionID: sessionID, Title: sessionID, Why: "created by another session's af"}, id, time.Now()))
}

var box1Host = session.RuntimeMachine{Host: "box1.example"}

func TestGitCredential_RefusedOnATaintedMachine(t *testing.T) {
	for name, tc := range map[string]struct {
		taint   session.RuntimeMachine
		refused bool
	}{
		"the machine, by name":             {box1Host, true},
		"the machine, by another spelling": {session.RuntimeMachine{Host: "BOX1.example."}, true},
		"the machine, by address":          {session.RuntimeMachine{Host: "198.51.100.7"}, true},
		// Fourth taint review, 1: the whole machine, whatever login placed it.
		"the machine, through another login's entry": {session.HostMachine(config.HostConfig{Host: "box1.example", User: "ubuntu"}), true},
		"another machine": {session.RuntimeMachine{Host: "box9.example"}, false},
		// Fourth taint review, 2: nothing unproven enters the ledger, so a name
		// that doesn't resolve is recorded by name, and matches only that name.
		"a name that doesn't resolve": {session.RuntimeMachine{Host: "nowhere.example"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
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

// Fourth taint review, 2: while any host is licensed, a placement onto a machine
// af can't identify is refused at placement; with none licensed, what af knows
// of it is recorded, never a match-everything entry.
func TestAnUnidentifiablePlacementIsRefusedWhileAHostIsLicensed(t *testing.T) {
	pf := newPushFixture(t)
	creds := newSandboxCredentials(pf.mgr, "child-1")
	err := creds.RecordPlacement(session.Placement{Title: "child", Why: "created by another session's af", Machine: session.RuntimeMachine{Host: "nowhere.example"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nowhere.example doesn't resolve")
	assert.Contains(t, err.Error(), "won't place code you didn't place there")
	err = creds.RecordPlacement(session.Placement{Title: "child", Why: "x", Machine: session.RuntimeMachine{SandboxSSH: "/usr/local/bin/to-box"}})
	require.Error(t, err, "a sandbox.ssh ssh can't confirm, too")

	pf.setConfig(func(c *config.Config) { c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}} })
	require.NoError(t, creds.RecordPlacement(session.Placement{Title: "child", Why: "x", Machine: session.RuntimeMachine{Host: "nowhere.example"}}))
	got, err := pf.mgr.pushCreds.taints.matching(session.MachineIdentity{Names: []string{"nowhere.example"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Identity.Unproven, "recorded by name, never as every machine")
	none, err := pf.mgr.pushCreds.taints.matching(box1Host.Identity())
	require.NoError(t, err)
	assert.Empty(t, none, "it taints no other machine")
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

// Codex P1 on #6433 at daf4ade: each placement is its own entry, and a reset of
// one machine clears only entries known to be ONLY on it. A session that went
// through one name (a VIP) onto two machines keeps the other machine tainted.
// One entry can also be cleared by its id.
func TestResetTrust_ClearsOnlyWhatIsOnlyOnThatMachine(t *testing.T) {
	pf := newPushFixture(t)
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": c.Hosts["box1"], "vip": {Host: "vip.example"}, "old": {Host: "198.51.100.29"}}
	})
	cs := &controlServer{manager: pf.mgr}
	// vip.example resolves (now) to 198.51.100.30; the session ran on .29 first.
	t.Cleanup(session.SetMachineLookupForTest(map[string][]string{"box1.example": {"198.51.100.7"}, "vip.example": {"198.51.100.30"}}))
	pf.taintIdentity(t, "repo-1", session.MachineIdentity{Names: []string{"vip.example"}, Addrs: []string{"198.51.100.29"}})
	pf.taintIdentity(t, "repo-1", session.MachineIdentity{Names: []string{"vip.example"}, Addrs: []string{"198.51.100.30"}})

	var resp ResetPushTrustResponse
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "old"}, &resp))
	require.Len(t, resp.Cleared, 1, "the old machine's entry only")
	assert.Equal(t, []string{"198.51.100.29"}, resp.Cleared[0].Identity.Addrs)
	left, err := pf.mgr.pushCreds.taints.matching(session.MachineIdentity{Addrs: []string{"198.51.100.30"}})
	require.NoError(t, err)
	require.Len(t, left, 1, "the replacement machine stays tainted")

	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Entry: left[0].ID}, &resp))
	require.Len(t, resp.Cleared, 1, "one entry by its id")

	pf.taint(t, "child-1", "child", "created by another session's af", box1Host)
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "box1"}, &resp))
	require.Len(t, resp.Cleared, 1)
	assert.Contains(t, resp.Identity.Names, "box1.example", "the identities reset are shown")
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	pf.taint(t, "child-2", "child2", "x", session.RuntimeMachine{Host: "box9.example"})
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{All: true}, &resp))
	assert.Len(t, resp.Cleared, 1)

	assert.Error(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "not-a-host"}, &resp), "an unlisted name")
	assert.Error(t, cs.ResetPushTrust(ResetPushTrustRequest{Host: "box1", All: true}, &resp), "exactly one scope")
}

// A ledger that can't be read taints every machine until it can.
func TestTaint_AnUnreadableLedgerWithholdsEveryHost(t *testing.T) {
	pf := newPushFixture(t)
	require.NoError(t, os.WriteFile(pf.mgr.pushCreds.taints.path, []byte("{not json"), 0o600))
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
	resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
	pf.mgr.fillPushCredentialBlockers(pf.mgr.Config(), resp)
	require.NotEmpty(t, resp.Hosts[0].PushCredentialBlockers)
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "can't be read")
}

// Fourth taint review, 3: two daemons on one AF home (an upgrade overlap) never
// drop each other's taints: every update reads the ledger fresh under its lock.
func TestTaint_TwoDaemonsOnOneLedgerLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushTaintFileName)
	t.Cleanup(session.SetMachineLookupForTest(map[string][]string{}))
	a, b := &pushTaintState{path: path}, &pushTaintState{path: path}
	rec := func(s *pushTaintState, i int) error {
		id := session.MachineIdentity{Addrs: []string{fmt.Sprintf("198.51.100.%d", i)}}
		return s.record(session.Placement{SessionID: fmt.Sprintf("s%d", i), Why: "x"}, id, time.Now())
	}
	require.NoError(t, rec(a, 1))
	_, err := b.matching(session.MachineIdentity{Addrs: []string{"203.0.113.1"}}) // b has read the ledger
	require.NoError(t, err)
	require.NoError(t, rec(a, 2))
	require.NoError(t, rec(b, 3)) // b must not write back a stale copy without a's second taint

	var wg sync.WaitGroup
	for i := 10; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 0 {
				s = b
			}
			assert.NoError(t, rec(s, i))
		}(i)
	}
	wg.Wait()
	fresh := &pushTaintState{path: path}
	for _, i := range append([]int{1, 2, 3}, func() []int {
		var out []int
		for i := 10; i < 40; i++ {
			out = append(out, i)
		}
		return out
	}()...) {
		got, err := fresh.matching(session.MachineIdentity{Addrs: []string{fmt.Sprintf("198.51.100.%d", i)}})
		require.NoError(t, err)
		assert.Lenf(t, got, 1, "the taint of placement %d survived both daemons' writes", i)
	}
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

// `af hosts check` shows each taint, with the session that made it and its entry
// id as context, on a licensed host and on one that isn't licensed yet.
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
	assert.Contains(t, line, "[entry ")
	assert.Contains(t, line, "af hosts reset-trust box1")
	require.Len(t, resp.Hosts[1].PushCredentialBlockers, 1, "an unlicensed host shows its taint too: it can't be licensed until reset")
	assert.Contains(t, resp.Hosts[1].PushCredentialBlockers[0], "af hosts reset-trust box9")
	assert.Empty(t, resp.Hosts[2].PushCredentialBlockers)
}

// A restored row the operator didn't place taints its machine before any push
// credential is answered; an operator's own does not. Codex P1 on #6433 at
// daf4ade and the fourth taint review, 3: a restore record that can't be written
// leaves push credentials withheld, and the next request retries it.
func TestRestoredRowsTheOperatorDidNotPlaceAreTaintedFirst(t *testing.T) {
	pf := newPushFixture(t)
	pf.mgr.pushCreds.taints.restored.Store(false)
	pf.track(t, session.InstanceData{ID: "child-1", Title: "child", CreatedBySandbox: "x"})
	pf.track(t, session.InstanceData{ID: "op-1", Title: "op", CreatedByOperator: true})
	pf.placeOn(t, "child", session.RuntimeMachine{Host: "box9.example"})
	pf.placeOn(t, "op", box1Host)

	// A readable ledger the writer refuses to write through (a symlink).
	real := filepath.Join(t.TempDir(), "real.json")
	require.NoError(t, os.WriteFile(real, []byte("[]"), 0o600))
	good := pf.mgr.pushCreds.taints.path
	require.NoError(t, os.Symlink(real, good))
	pf.mgr.recordRestoredPlacements()
	assert.False(t, pf.mgr.pushCreds.taints.restored.Load(), "a rolled-back restore record is not done")
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushTaintsLoading)

	require.NoError(t, os.Remove(good))
	code, got, msg = pf.askOwn(t) // retries the restore pass, which now succeeds
	requireAnswered(t, code, got, msg)
	assert.True(t, pf.mgr.pushCreds.taints.restored.Load())
	taints, err := pf.mgr.pushCreds.taints.matching(session.RuntimeMachine{Host: "box9.example"}.Identity())
	require.NoError(t, err)
	require.Len(t, taints, 1)
	assert.Equal(t, "child-1", taints[0].SessionID)
	mine, err := pf.mgr.pushCreds.taints.matching(box1Host.Identity())
	require.NoError(t, err)
	assert.Empty(t, mine, "the operator's own session taints nothing")
}

// Codex P1 on #6433 at daf4ade: a taint recorded while the second pass's
// blocking steps run (the carrier lookup) is seen by the final check.
func TestGitCredential_ATaintRecordedMidRequestIsSeenAtTheFinalCheck(t *testing.T) {
	pf := newPushFixture(t)
	prev := testHookPushBeforeFinalCheck
	testHookPushBeforeFinalCheck = func() { pf.taint(t, "child-1", "child", "created by another session's af", box1Host) }
	t.Cleanup(func() { testHookPushBeforeFinalCheck = prev })
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostTainted)
}

// Delta check of taint round two, a: an entry af couldn't place on an identified
// machine (a name that doesn't resolve, a sandbox.ssh ssh can't confirm) can't
// be shown to be another machine, so no host is licensed while one exists, and
// the refusal names its --entry id. It blocks no grant on a host already
// licensed.
func TestLicensingIsRefusedWhileAnUnconfirmedEntryExists(t *testing.T) {
	pf := newPushFixture(t)
	licensedBox1 := pf.mgr.Config().Hosts["box1"]
	pf.setConfig(func(c *config.Config) { c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}} })
	creds := newSandboxCredentials(pf.mgr, "child-1")
	require.NoError(t, creds.RecordPlacement(session.Placement{Title: "child", Why: "x", Machine: session.RuntimeMachine{SandboxSSH: "/usr/local/bin/to-box"}}))
	thin, err := pf.mgr.pushCreds.taints.unconfirmed()
	require.NoError(t, err)
	require.Len(t, thin, 1, "an unconfirmed sandbox.ssh placement leaves a record, never nothing")

	licensed := config.HostConfig{Host: "box9.example", PushCredentials: true}
	err = pf.mgr.refuseLicensingTainted(SetConfigValueRequest{HostsEdit: &config.HostsEdit{Op: config.HostsEditAdd, Name: "box9", Host: &licensed}})
	require.Error(t, err, "a machine nothing names is still refused while the entry exists")
	assert.Contains(t, err.Error(), "af hosts reset-trust --entry "+thin[0].ID)

	resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
	pf.mgr.fillPushCredentialBlockers(pf.mgr.Config(), resp)
	require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1, "every host names it")
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "--entry "+thin[0].ID)

	pf.setConfig(func(c *config.Config) { c.Hosts = map[string]config.HostConfig{"box1": licensedBox1} })
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg) // a host already licensed keeps answering

	cs := &controlServer{manager: pf.mgr}
	var rr ResetPushTrustResponse
	require.NoError(t, cs.ResetPushTrust(ResetPushTrustRequest{Entry: thin[0].ID}, &rr))
	assert.NoError(t, pf.mgr.refuseLicensingTainted(SetConfigValueRequest{HostsEdit: &config.HostsEdit{Op: config.HostsEditAdd, Name: "box9", Host: &licensed}}))
}

// Codex P1 on #6433 at 2fd8cc9: the licence and the automatic-push fence are
// re-checked after the final ledger read, which can wait on its lock.
func TestGitCredential_LicenceAndFenceAreRecheckedAfterTheFinalLedgerRead(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(pf *pushFixture)
		refusal string
	}{
		"the licence is turned off": {func(pf *pushFixture) {
			pf.setConfig(func(c *config.Config) {
				c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}}
			})
		}, refusePushNotLicensed},
		"an automatic push fences the runtime": {func(pf *pushFixture) {
			_, err := pf.mgr.pushCreds.beginAutoPush(pf.me.ID, "ref-me", nil)
			require.NoError(t, err)
		}, refusePushAutoPreserve},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			pf.mgr.pushCreds.fencePath = filepath.Join(t.TempDir(), pushFenceFileName)
			prev := testHookPushBeforeFinalCheck
			testHookPushBeforeFinalCheck = func() { tc.change(pf) }
			t.Cleanup(func() { testHookPushBeforeFinalCheck = prev })
			code, got, msg := pf.askOwn(t)
			requireRefused(t, code, got, msg, tc.refusal)
		})
	}
}

// Codex P2 on #6433 at 2fd8cc9: a reset is durable. The restore pass on the next
// start doesn't re-import a placement the operator cleared; a NEW placement of
// that session taints again.
func TestRestoreDoesNotReimportAClearedPlacement(t *testing.T) {
	pf := newPushFixture(t)
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": c.Hosts["box1"], "box9": {Host: "box9.example"}}
	})
	pf.track(t, session.InstanceData{ID: "child-1", Title: "child", CreatedBySandbox: "x", Status: session.Archived})
	box9 := session.RuntimeMachine{Host: "box9.example"}
	pf.placeOn(t, "child", box9)
	pf.mgr.pushCreds.taints.restored.Store(false)
	pf.mgr.recordRestoredPlacements()
	require.Len(t, pf.mgr.pushCreds.taintsFor(box9.Identity()), 1)

	var rr ResetPushTrustResponse
	require.NoError(t, (&controlServer{manager: pf.mgr}).ResetPushTrust(ResetPushTrustRequest{Host: "box9"}, &rr))
	require.Len(t, rr.Cleared, 1)

	pf.mgr.pushCreds.taints.restored.Store(false) // the next daemon start
	pf.mgr.recordRestoredPlacements()
	assert.Empty(t, pf.mgr.pushCreds.taintsFor(box9.Identity()), "the restore pass undid the reset")

	creds := newSandboxCredentials(pf.mgr, "child-1")
	require.NoError(t, creds.RecordPlacement(session.Placement{Title: "child", Why: "x", Machine: box9}))
	assert.Len(t, pf.mgr.pushCreds.taintsFor(box9.Identity()), 1, "a new placement taints again")
}

// Codex P1 on #6433 at 2fd8cc9: an eligible caller on a tainted machine can't
// drive an eligible peer: whatever the taint stands for could otherwise reach
// the peer's credential through the caller's own callback.
func TestATaintedCallerCannotDriveAnEligiblePeer(t *testing.T) {
	pf := newPushFixture(t)
	pf.eligiblePeer()
	require.True(t, pf.mgr.pushEligible(pf.me), "precondition: the caller is eligible")
	require.True(t, pf.mgr.pushEligible(pf.peer), "precondition: the peer is eligible")
	_, out := pf.call(t, pf.secret, "/v1/SendPrompt", `{"title":"peer","prompt":"hi"}`)
	assert.NotContains(t, out, refusePushEscalationText, "anti-vacuous: an untainted eligible caller may prompt it")

	pf.taint(t, "child-1", "child", "created by another session's af", box1Host) // the caller's machine
	_, out = pf.call(t, pf.secret, "/v1/SendPrompt", `{"title":"peer","prompt":"fetch the token"}`)
	assert.Contains(t, out, refusePushEscalationText)
}
