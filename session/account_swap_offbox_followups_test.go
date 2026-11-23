package session

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7799: follow-ups to the durable in-flight record (#7795). The checkpoint
// that names the incoming account never carries the outgoing account's limit
// wall; a stage no live record names does not outlive a reattach on a server
// that can discard it in order; and every committed-swap arm owes the
// replacement the same takeover brief.

// limitWallReset is the outgoing account's parked reset — far enough out that
// a reload carrying it would park the brief behind it for hours.
var limitWallReset = time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)

const limitWallEvidence = "resets 5pm"

// parkFixtureAtWall parks the fixture's row on the outgoing account's usage
// limit through the real park path, so the wall carries the attribution and
// the standing shadow mirror a production park writes.
func parkFixtureAtWall(t *testing.T, f *swapFixture) {
	t.Helper()
	f.inst.SetLimitReachedAtEvidence(limitWallReset, limitWallEvidence, LimitBanner{}, false)
	require.True(t, f.inst.LimitReached(), "the row is parked before the swap")
	f.inst.mu.RLock()
	defer f.inst.mu.RUnlock()
	require.Equal(t, "work", f.inst.limitAccount, "the wall is the outgoing account's")
}

// captureCheckpoints replaces the fixture's durable write with one that keeps
// every row the swap would have written to disk, in order.
func captureCheckpoints(f *swapFixture) *[]InstanceData {
	var snaps []InstanceData
	f.inst.SetOffBoxSwapPersist(func(ii *Instance) error {
		snaps = append(snaps, ii.ToInstanceData().ForStorage())
		return nil
	})
	return &snaps
}

// assertNoOutgoingWallOnIncomingCheckpoints is the #7799 property: every
// durable row that names the incoming account is free of the outgoing
// account's live wall. It returns those checkpoints for further asserts.
func assertNoOutgoingWallOnIncomingCheckpoints(t *testing.T, snaps []InstanceData, incoming string) []InstanceData {
	t.Helper()
	var named []InstanceData
	for _, s := range snaps {
		if s.Account != incoming {
			continue
		}
		named = append(named, s)
		assert.NotEqual(t, LiveLimitReached, s.Liveness,
			"a checkpoint naming %q must not be parked on the outgoing account's wall", incoming)
		assert.True(t, s.LimitResetAt.IsZero(), "nor carry its reset time")
		assert.Empty(t, s.LimitResetEvidence, "nor its reset evidence")
		assert.Empty(t, s.LimitAccount, "nor its attribution")
		assert.Empty(t, s.LimitAgent)
	}
	require.NotEmpty(t, named, "some checkpoint names the incoming account")
	return named
}

// The manual-handoff shape: the swap's commit writes the record-clear
// checkpoint naming the new account. On master it still carried the old
// account's wall — the daemon cleared it only afterwards, so a death between
// the two reloaded the new account parked at the old one's reset.
func TestOffBoxSwapCommitCheckpointCarriesNoOutgoingWall(t *testing.T) {
	f := newSwapFixture(t)
	parkFixtureAtWall(t, f)
	snaps := captureCheckpoints(f)

	_, err := f.inst.SwapOffBoxAccount("alt", false, "")
	require.NoError(t, err)

	named := assertNoOutgoingWallOnIncomingCheckpoints(t, *snaps, "alt")
	assert.Nil(t, named[len(named)-1].LastLimitWall,
		"outside a respawn fence the replaced runtime's wall shadow is dropped, as before")
	assert.False(t, f.inst.LimitReached(), "and the live row is past the wall")
	_, known := f.inst.LimitResetAt()
	assert.False(t, known)
}

// The scheduled-failover shape: the swap runs under the limit-resume fence
// (OpRespawning), which keeps the wall shadow a failed resume would re-park
// from. The commit checkpoint keeps it — as a RETIRED claim attributed to the
// outgoing account, never a standing wall the incoming one could inherit.
func TestOffBoxSwapCommitUnderTheResumeFenceRetiresTheOutgoingWall(t *testing.T) {
	f := newSwapFixture(t)
	parkFixtureAtWall(t, f)
	f.inst.mu.Lock()
	f.inst.inFlightOp = OpRespawning
	f.inst.mu.Unlock()
	snaps := captureCheckpoints(f)

	_, err := f.inst.SwapOffBoxAccount("alt", true, "")
	require.NoError(t, err)

	named := assertNoOutgoingWallOnIncomingCheckpoints(t, *snaps, "alt")
	wall := named[len(named)-1].LastLimitWall
	require.NotNil(t, wall, "the respawn fence keeps the shadow")
	require.NotNil(t, wall.Retired, "with an explicit retirement verdict")
	assert.True(t, *wall.Retired, "a wall the row moved past, never a standing claim")
	assert.Equal(t, "work", wall.Account, "still attributed to the account that hit it")
	assert.True(t, limitWallReset.Equal(wall.ResetAt), "with the reset it advertised")
}

// A daemon death between the commit checkpoint and the failover's tail
// reloads exactly what that checkpoint wrote. The reload must carry none of
// the outgoing account's wall: on master the parked liveness and the claim
// fields restoreLimitClaims writes back (reset, evidence, attribution) all
// came back on the incoming account, so the mission retry could park the
// brief at the outgoing account's reset.
func TestOffBoxSwapReloadBetweenCheckpointsCarriesNoOutgoingWall(t *testing.T) {
	f := newSwapFixture(t)
	parkFixtureAtWall(t, f)
	snaps := captureCheckpoints(f)
	const notice = "[Agent Factory] This session switched accounts.\n\ncontinue"

	_, err := f.inst.SwapOffBoxAccount("alt", true, notice)
	require.NoError(t, err)
	named := assertNoOutgoingWallOnIncomingCheckpoints(t, *snaps, "alt")
	last := named[len(named)-1]
	require.Nil(t, last.OffBoxSwapInFlight, "the death lands after the record's clear")
	require.Equal(t, "ssh", last.BackendType)

	// The reload sees what that checkpoint carries about the account, the
	// wall, and the owed brief, on the row's own backend type — which loads
	// inert and provisionally Lost, the reattach's starting point.
	reload := InstanceData{
		ID: "swap", Title: "swap", Program: tmux.ProgramClaude,
		Path: t.TempDir(), Branch: "af/swap", BackendType: last.BackendType,
		Liveness: last.Liveness, Account: last.Account, AccountAgent: last.AccountAgent,
		LimitResetAt: last.LimitResetAt, LimitResetEvidence: last.LimitResetEvidence,
		LimitAgent: last.LimitAgent, LimitAccount: last.LimitAccount,
		LastLimitWall:         last.LastLimitWall,
		PendingHandoffMission: last.PendingHandoffMission, HandoffDeliveryStatus: last.HandoffDeliveryStatus,
	}
	reloaded, err := FromInstanceData(reload)
	require.NoError(t, err)
	assert.False(t, reloaded.LimitReached(), "the reloaded incoming account is not parked")
	reloaded.mu.RLock()
	assert.True(t, reloaded.limitResetAt.IsZero(), "and carries no reset to park the brief behind")
	assert.Empty(t, reloaded.limitResetEvidence)
	assert.Empty(t, reloaded.limitAccount, "nor the outgoing account's attribution")
	reloaded.mu.RUnlock()
	assert.Equal(t, notice, reloaded.PendingHandoffMission(), "while the brief stays owed")
}

// stageSweepHost is a reattach fixture whose stand-in agent-server serves the
// serialized discard against a real directory, marker-bound the way the
// agent-server's own discardAccountNextInPlace is. discard=false is a server
// that predates the route.
type stageSweepHost struct {
	inst     *Instance
	live     string
	discards *[]string
	persists *[]InstanceData
}

func newStageSweepHost(t *testing.T, discard bool, status map[string]any) *stageSweepHost {
	t.Helper()
	newListedFixture(t, oneListedHost)
	home := os.Getenv("AGENT_FACTORY_HOME")
	workDir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramClaude, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, credFile), []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, credFile), []byte(secretAlt), 0o600))

	dir := t.TempDir()
	live := filepath.Join(dir, "account")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, credFile), []byte(secretWork), 0o600))
	t.Setenv("CLAUDE_CONFIG_DIR", live)

	_, err = runtimetoken.EnsureKey()
	require.NoError(t, err)
	ref, err := runtimetoken.NewTokenRef()
	require.NoError(t, err)

	var discards []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": agentAliveResp{Alive: true}})
	})
	if status != nil {
		mux.HandleFunc("/v1/agent/swap-account-status", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": status})
		})
	}
	if discard {
		mux.HandleFunc("/v1/agent/swap-account-discard", func(w http.ResponseWriter, r *http.Request) {
			var req accountSwapReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			discards = append(discards, req.SwapID)
			// The server's own rule: both spellings probed, each removed
			// only where its marker names this swap or it carries none.
			paths := []string{live + accountNextSuffix}
			if req.SwapID != "" {
				paths = append(paths, stagedScopeDir(live, req.SwapID))
			}
			for _, next := range paths {
				if _, statErr := os.Lstat(next); errors.Is(statErr, os.ErrNotExist) {
					continue
				}
				if dErr := discardAccountNextBound(next, req.SwapID); dErr != nil {
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": dErr.Error()}})
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]bool{"ok": true}})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	backend, teardown, err := restoreRuntimeCleanup("load", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: dir, RemotePID: "4242",
		RemoteAddr: "127.0.0.1:" + port, AgentServerVersion: "1.0.0", TokenRef: ref,
	}})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	p.sessionDir = dir
	p.runCommandFn = localShell{}.Run
	p.startTunnelFn = func(string) (*AgentServerEndpoint, error) {
		return &AgentServerEndpoint{URL: "http://" + srv.Listener.Addr().String()}, nil
	}

	var persists []InstanceData
	inst := &Instance{
		Title: "load", liveness: LiveLost, backend: backend, runtimeTeardown: teardown,
		Account: "work", accountAgent: tmux.ProgramClaude,
	}
	inst.offBoxSwapPersist = func(ii *Instance) error {
		persists = append(persists, ii.ToInstanceData().ForStorage())
		return nil
	}
	return &stageSweepHost{inst: inst, live: live, discards: &discards, persists: &persists}
}

// stage writes a staged scope at dir holding credentials, bound to marker
// when it is non-empty.
func (h *stageSweepHost) stage(t *testing.T, dir, marker, cred string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, credFile), []byte(cred), 0o600))
	if marker != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, accountSwapMarkerRel), []byte(marker), 0o600))
	}
}

// A daemon that died between staging and its record landing leaves a stage no
// record names; an older build can leave the anonymous shared spelling. A
// reattach of the record-less row sweeps both through the serialized discard
// and never touches the live scope. On master both stayed until teardown.
func TestReattachSweepsOrphanedStagesThroughTheSerializedDiscard(t *testing.T) {
	h := newStageSweepHost(t, true, nil)
	orphan := stagedScopeDir(h.live, "swap-orphan")
	h.stage(t, orphan, "swap-orphan", secretAlt)
	h.stage(t, h.live+accountNextSuffix, "", secretAlt)

	require.NoError(t, h.inst.ReattachSandbox())

	assert.NoDirExists(t, orphan, "the stage no record names is gone")
	assert.NoDirExists(t, h.live+accountNextSuffix, "and so is the anonymous shared one")
	assert.Equal(t, secretWork, readOrEmpty(filepath.Join(h.live, credFile)), "the live scope is untouched")
	assert.ElementsMatch(t, []string{"", "swap-orphan"}, *h.discards,
		"each removal went through the serialized discard, once per id")
	assert.NotNil(t, h.inst.remoteClient, "and the row still binds")
}

// A row whose agent-server was dead is restarted and re-dialed by the
// reattach; the restarted server is the same serialized route — and holds no
// queued request at all — so its orphans are swept there too, never left for
// teardown. On the previous call site (gated on the first dial answering) this
// arm never swept.
func TestReattachSweepsOrphanedStagesOnARestartedAgentServer(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.Account, inst.accountAgent = "work", tmux.ProgramClaude
	// The fixture's host is scripted, not a directory: it answers the sweep's
	// one listing with an orphan bound to a swap no record names.
	shared := (&sandboxWorkspace{SessionDir: p.sessionDir}).AccountDir() + accountNextSuffix
	host := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, in io.Reader, combined bool) ([]byte, error) {
		if strings.HasPrefix(script, "for d in ") && strings.Contains(script, accountNextSuffix) {
			return []byte(shared + ".swap-orphan\tswap-orphan\n"), nil
		}
		return host(d, script, in, combined)
	}

	require.NoError(t, inst.ReattachSandbox())
	require.True(t, h.ran("echo relaunched"), "the dead server was restarted")
	assert.NotNil(t, inst.remoteClient, "and the row bound to it")

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, []string{"swap-orphan"}, h.discards,
		"the orphan went through the restarted server's serialized discard")
	assert.False(t, h.discardBeforeRestart, "never at the dead server it replaced")
	assert.Len(t, h.lifecycle, 2, "adoption still ran before the sweep")
}

// The discard is marker-bound on the server: a swap-unique stage whose marker
// names a different swap is not the path's swap to remove, so it stays.
func TestReattachSweepLeavesAStageBoundToAnotherSwap(t *testing.T) {
	h := newStageSweepHost(t, true, nil)
	mismatched := stagedScopeDir(h.live, "swap-a")
	h.stage(t, mismatched, "swap-b", secretAlt)

	require.NoError(t, h.inst.ReattachSandbox())

	assert.DirExists(t, mismatched, "a stage bound to another swap is never removed under this one")
	assert.Equal(t, secretAlt, readOrEmpty(filepath.Join(mismatched, credFile)))
	assert.Equal(t, secretWork, readOrEmpty(filepath.Join(h.live, credFile)))
}

// A server too old for the discard route offers no ordering: the orphans stay
// for teardown — never an unordered rm — and the reattach still succeeds.
func TestReattachSweepLeavesOrphansOnAServerWithoutTheDiscardRoute(t *testing.T) {
	h := newStageSweepHost(t, false, nil)
	h.stage(t, h.live+accountNextSuffix, "swap-old", secretAlt)

	require.NoError(t, h.inst.ReattachSandbox(), "a sweep failure never fails the reattach")
	assert.DirExists(t, h.live+accountNextSuffix, "no unordered removal on a server with no serialization")
	assert.NotNil(t, h.inst.remoteClient)
}

// A row with no account has no scope anything was ever staged beside: the
// sweep reads nothing on the host and removes nothing.
func TestReattachSweepSkipsARowWithNoAccountScope(t *testing.T) {
	h := newStageSweepHost(t, true, nil)
	h.inst.Account, h.inst.accountAgent = "", ""
	h.stage(t, h.live+accountNextSuffix, "", secretAlt)
	p := h.inst.backend.(*sandboxBackend).provisioner
	var scripts []string
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		scripts = append(scripts, script)
		return localShell{}.Run(d, script, stdin, combined)
	}

	require.NoError(t, h.inst.ReattachSandbox())
	for _, s := range scripts {
		assert.NotContains(t, s, accountNextSuffix, "no host listing ran for an account-less row")
	}
	assert.Empty(t, *h.discards)
	assert.DirExists(t, h.live+accountNextSuffix)
}

// A host-supplied marker or name that could reach past the account.next.<id>
// spelling never becomes a discard id.
func TestParseAccountStagesAdmitsOnlyThisScopesStages(t *testing.T) {
	shared := "/s/account.next"
	out := []byte(shared + "\tswap-1\n" +
		shared + ".swap-2\t\n" +
		shared + "../../etc\tx\n" +
		"/elsewhere/account.next\ty\n" +
		"no tab here\n")
	got := parseAccountStages(shared, out)
	assert.Equal(t, []accountStage{
		{dir: shared, marker: "swap-1"},
		{dir: shared + ".swap-2", suffix: "swap-2"},
	}, got)
	assert.False(t, validStageID("../x"))
	assert.False(t, validStageID(""))
	assert.True(t, validStageID("0123abcdef"))
}

// The reconciled commit at reattach — the record's verdict reached by a later
// daemon — writes its checkpoint with no outgoing wall and owing the brief the
// record carried, the same two facts the live commit writes (#7799).
func TestReattachReconciledCommitClearsTheWallAndOwesTheBrief(t *testing.T) {
	h := newStageSweepHost(t, true, map[string]any{
		"ok": true, "outcome": swapOutcomeCommitted, "program": "claude", "swap_id": "swap-1",
	})
	// The host after the lost swap committed: the new account live, the old
	// one waiting at .prev.
	require.NoError(t, os.WriteFile(filepath.Join(h.live, credFile), []byte(secretAlt), 0o600))
	prev := h.live + accountPrevSuffix
	require.NoError(t, os.MkdirAll(prev, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(prev, credFile), []byte(secretWork), 0o600))

	const notice = "[Agent Factory] This session switched accounts.\n\ncontinue"
	h.inst.liveness = LiveLimitReached
	h.inst.limitResetAt = limitWallReset
	h.inst.limitResetEvidence = limitWallEvidence
	h.inst.limitAgent = tmux.ProgramClaude
	h.inst.limitAccount = "work"
	h.inst.startupStateUnknown = true
	h.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt", Notice: notice}

	require.NoError(t, h.inst.ReattachSandbox())
	acct, _ := h.inst.AccountSelection()
	require.Equal(t, "alt", acct)

	named := assertNoOutgoingWallOnIncomingCheckpoints(t, *h.persists, "alt")
	settle := named[len(named)-1]
	assert.Nil(t, settle.OffBoxSwapInFlight, "the checkpoint is the record's clear")
	assert.Equal(t, notice, settle.PendingHandoffMission, "and it owes the replacement the brief")
	assert.Equal(t, PromptNotDelivered, settle.HandoffDeliveryStatus, "replayable by recovery")

	h.inst.mu.RLock()
	defer h.inst.mu.RUnlock()
	assert.True(t, h.inst.limitResetAt.IsZero(), "no outgoing reset survives in memory either")
	assert.Empty(t, h.inst.limitAccount)
}

// A commit only a reconcile reaches owes the replacement the same brief the
// live commit stamps (#7799): the record carries the notice, and the
// checkpoint that clears it carries the pending mission with a not-delivered
// verdict. On master the reconciled arm stamped nothing, so recovery had no
// brief to replay.
func TestReconciledCommitOwesTheTakeoverBrief(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")
	const notice = "[Agent Factory] This session switched accounts. Continue the same task.\n\ncontinue"

	var checkpoints []InstanceData
	f.inst.SetOffBoxSwapPersist(func(ii *Instance) error {
		checkpoints = append(checkpoints, ii.ToInstanceData().ForStorage())
		return nil
	})

	_, err := f.inst.SwapOffBoxAccount("alt", false, notice)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.NotEmpty(t, checkpoints)
	require.NotNil(t, checkpoints[0].OffBoxSwapInFlight)
	assert.Equal(t, notice, checkpoints[0].OffBoxSwapInFlight.Notice,
		"the record durably carries the brief a later daemon may owe")
	assert.Empty(t, f.inst.PendingHandoffMission(), "nothing is owed while the verdict is unknown")

	f.srv.statusErr = nil
	settled, err := f.reconcileInFlight()
	require.NoError(t, err)
	require.True(t, settled)

	assert.Equal(t, notice, f.inst.PendingHandoffMission(), "the reconciled commit owes the brief")
	assert.Equal(t, PromptNotDelivered, f.inst.PendingHandoffDeliveryStatus(),
		"with the verdict recovery replays")
	last := checkpoints[len(checkpoints)-1]
	assert.Nil(t, last.OffBoxSwapInFlight, "the settle's checkpoint retires the record")
	assert.Equal(t, notice, last.PendingHandoffMission, "in the same write that carries the obligation")
	assert.Equal(t, PromptNotDelivered, last.HandoffDeliveryStatus)
}
