package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The daemon-level pin for #7860's route on an off-box row (#7871 items 1+3):
// an automatic account swap is never attempted against a session that still
// owes a handoff mission — the scheduler withholds the swap hint before any
// swap work, and the admission underneath still refuses with
// ErrOffBoxAccountSwapOwesMission for a mission that lands mid-flight. The
// hold is deliberately narrow: it withholds ONLY the swap opportunity, never
// the row's ordinary same-account resume — that resume never depended on a
// swap candidate existing, so it runs exactly as on master while the mission
// is owed. Only after the mission's own state changes does the swap proceed.

// offBoxAgentServer is the stand-in agent-server the swap's REST calls reach:
// it counts every request by route so a test can prove what was — and was
// not — attempted against the row.
type offBoxAgentServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    map[string]int
	swapIDs []string
	// sessionDir is the fixture's "host" session dir: the swap-account route
	// rotates the account scopes there the way the real agent-server would.
	sessionDir string
}

func newOffBoxAgentServer(t *testing.T, sessionDir string) *offBoxAgentServer {
	t.Helper()
	s := &offBoxAgentServer{hits: map[string]int{}, sessionDir: sessionDir}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"version": "test",
			"features": []string{
				"account-swap", "account-swap-discard",
				"account-swap-scoped-stage", "agent-tab-id",
			},
		}})
	})
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
	})
	mux.HandleFunc("/v1/agent/swap-account", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SwapID      string `json:"swap_id"`
			ScopedStage bool   `json:"scoped_stage"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.swapIDs = append(s.swapIDs, req.SwapID)
		s.mu.Unlock()
		// The rotation the real agent-server runs between the confirmed stop
		// of the old agent and the start of the new one: live -> .prev,
		// this swap's staged scope -> live.
		live := filepath.Join(s.sessionDir, "account")
		prev := filepath.Join(s.sessionDir, "account.prev")
		staged := filepath.Join(s.sessionDir, "account.next."+req.SwapID)
		if !req.ScopedStage {
			staged = filepath.Join(s.sessionDir, "account.next")
		}
		_ = os.RemoveAll(prev)
		_ = os.Rename(live, prev)
		_ = os.Rename(staged, live)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"ok": true, "program": "claude",
		}})
	})
	mux.HandleFunc("/v1/agent/swap-account-status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var id string
		if n := len(s.swapIDs); n > 0 {
			id = s.swapIDs[n-1]
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"ok": true, "outcome": "committed", "program": "claude", "swap_id": id,
		}})
	})
	mux.HandleFunc("/v1/agent/swap-account-discard", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/send-prompt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/relaunch", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/launch", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/preview", func(w http.ResponseWriter, r *http.Request) {
		// Claude's composer glyph: the send path's readiness wait sees a ready
		// pane on the first poll instead of burning its whole budget.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"content": "❯", "has_terminal_modes": false}})
	})
	mux.HandleFunc("/v1/agent/snapshot", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"updated": false}})
	})
	mux.HandleFunc("/v1/agent/stop", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/kill", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	})
	mux.HandleFunc("/v1/agent/workspace-facts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/v1/agent/bound-draft-still-staged", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"staged": false}})
	})
	mux.HandleFunc("/v1/agent/tabs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"tabs": []any{}}})
	})
	mux.HandleFunc("/v1/agent/restore-tabs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/v1/agent/agent-tab-id", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": ""}})
	})
	// Anything else the swap or resume path asks for answers with an empty
	// envelope rather than a 404 the client would read as the host refusing.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
	s.srv = httptest.NewServer(counted)
	t.Cleanup(s.srv.Close)
	return s
}

// resetHits drops the calls fixture setup already made — the installer's
// feature-cache warm-up is a health probe of its own, so the "nothing was
// attempted" assertion counts only what runs after the fixture returns.
func (s *offBoxAgentServer) resetHits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = map[string]int{}
}

func (s *offBoxAgentServer) routeHits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// newOffBoxLimitedFixture builds the row the route runs against: a started
// ssh session parked at a usage-limit wall on the auto-selected account
// "work", carrying the synced account on a listed host, with "alt" registered
// and configured as the failover candidate. The stand-in agent-server answers
// every swap-route the row's client can call.
func newOffBoxLimitedFixture(t *testing.T) (*Manager, string, string, *session.Instance, *offBoxAgentServer) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	hostEntry := config.HostConfig{Host: "box1.example", User: "dev"}
	cfg := config.DefaultConfig()
	cfg.Hosts = map[string]config.HostConfig{"box1": hostEntry}
	cfg.LimitAutoResume = true
	cfg.LimitAccountCandidates = []string{"work", "alt"}
	// The send licence reads the global config FILE — it must be saved before
	// the fixture's sync-in runs and before every swap admission re-asks it.
	require.NoError(t, config.SaveConfig(cfg))

	home := os.Getenv("AGENT_FACTORY_HOME")
	workDir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramClaude, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".credentials.json"), []byte("token-for-work"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, ".credentials.json"), []byte("token-for-alt"), 0o600))

	sessionDir := t.TempDir()
	srv := newOffBoxAgentServer(t, sessionDir)

	manager, err := NewManager(cfg)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{Title: "offbox", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	require.NoError(t, inst.InstallOffBoxSwapBackendForTest(session.OffBoxSwapFixture{
		Endpoint:     session.AgentServerEndpoint{URL: srv.srv.URL, Token: "test-token"},
		SessionDir:   sessionDir,
		Account:      sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: workDir},
		SyncHost:     "box1",
		SyncTarget:   hostEntry,
		AutoSelected: true,
		// The "host" these scripts would run on is this machine: the test's
		// runner executes them locally, the same way the session package's own
		// fixtures drive the provisioner.
		Run: func(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
			cmd := exec.Command("sh", "-c", script)
			cmd.Stdin = stdin
			return cmd.CombinedOutput()
		},
	}))
	// The swap's in-flight record refuses to cross without a durable write;
	// the fixture row's checkpoints are in-memory only.
	inst.SetOffBoxSwapPersist(func(*session.Instance) error { return nil })
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.Prompt = "the work already on the row"
	inst.SetLimitReached(nowFunc().Add(-time.Hour))
	seedDiskInstance(t, repo.ID, "offbox", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, "offbox")] = inst
	manager.mu.Unlock()
	srv.resetHits()
	return manager, repo.ID, repoPath, inst, srv
}

const owedMission = "[Agent Factory] This session was handed off from an earlier runtime. Continue the prior task.\n\nprior task"

func oweAMission(t *testing.T, inst *session.Instance) {
	t.Helper()
	inst.SetPendingHandoffMission(owedMission)
	require.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus())
}

// Item 1's property, narrowed by review: a session that still owes a handoff
// mission yields NO swap attempt — the host's swap routes are never called —
// but the hold is ONLY on the swap. The row's ordinary same-account resume
// still runs (it never depended on a swap candidate existing), the wall
// un-stalls on the account the row already has, and the mission and its
// verdict stay owed for their own recovery. An earlier version held the whole
// row out of the scheduler and starved that resume behind the mission.
func TestResumeLimitedSessionsWithholdsOnlyTheSwapFromAnOwedRow(t *testing.T) {
	manager, _, _, inst, srv := newOffBoxLimitedFixture(t)
	oweAMission(t, inst)

	manager.ResumeLimitedSessions()

	assert.Zero(t, srv.routeHits("/v1/agent/swap-account"),
		"the owed mission withholds the swap: the host's swap route is never called")
	assert.Zero(t, srv.routeHits("/v1/agent/swap-account-status"))
	assert.GreaterOrEqual(t, srv.routeHits("/v1/agent/send-prompt"), 1,
		"the same-account resume still sends the stored prompt")
	acct, _ := inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row resumes on the account it already had")
	assert.Equal(t, owedMission, inst.PendingHandoffMission(),
		"the durable mission survives — the resume is not a discharge")
	assert.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the fence lowered after the resume")
	assert.False(t, inst.LimitReached(), "the ordinary resume un-stalled the wall")
}

// The route itself (#7860): the scheduler's timing hint is computed before
// the mission lands — the mid-swap race — and the swap admission then refuses
// with ErrOffBoxAccountSwapOwesMission. The fence drops, the mission and its
// verdict persist untouched, and no ordinary fallback send fires past the
// refusal even with the fallback armed.
func TestOffBoxSwapRefusalPersistsTheOwedMissionAndItsVerdict(t *testing.T) {
	manager, repoID, _, inst, srv := newOffBoxLimitedFixture(t)
	key := daemonInstanceKey(repoID, "offbox")

	swap, err := manager.accountSwapOpportunityFromFactsWithEvidence(inst, manager.Config(), loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotNil(t, swap, "with no mission owed yet, the failover candidate is a live hint")
	swap.fallbackDue = true
	oweAMission(t, inst)

	outcome, _, resumeErr := manager.resumeFromLimitLockedWithAccount(repoID, key, inst, inst.Title, swap)
	require.Error(t, resumeErr)
	assert.ErrorIs(t, resumeErr, session.ErrOffBoxAccountSwapOwesMission,
		"the refusal must carry the sentinel the failover routes on")
	assert.Equal(t, resumeNotPerformed, outcome)
	assert.False(t, swap.fellBack,
		"an owed mission must not fall back to the ordinary resume either")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the fence lowered after the refusal")
	assert.Equal(t, owedMission, inst.PendingHandoffMission(),
		"the durable mission survives the refused swap")
	assert.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus(),
		"and so does its delivery verdict")
	assert.Zero(t, srv.routeHits("/v1/agent/send-prompt"),
		"no fallback send fires past the refusal — the mission stays owed")
	assert.Zero(t, srv.routeHits("/v1/agent/swap-account"),
		"the swap request itself is never sent")
	assert.True(t, inst.LimitReached(), "the wall belongs to the owed row still")
}

// Re-admission: once the owed mission's state changes — here the not-delivered
// mission transfers into the stored prompt at the wall — the scheduler admits
// the row again and the failover swap commits on the host.
func TestOffBoxSwapIsReadmittedOnceTheMissionTransfers(t *testing.T) {
	manager, _, _, inst, srv := newOffBoxLimitedFixture(t)
	oweAMission(t, inst)

	// Handoff recovery transfers a not-delivered mission on a walled row into
	// the stored prompt — the mission's own state change, after which the row
	// is no longer owed anything.
	manager.ResumePendingHandoffs()
	require.Empty(t, inst.PendingHandoffMission(),
		"the transfer clears the obligation")
	require.Equal(t, owedMission, inst.Prompt,
		"the transferred mission becomes the stored prompt the limit retry owns")

	manager.ResumeLimitedSessions()

	acct, _ := inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the failover swap committed on re-admission")
	assert.Equal(t, 1, srv.routeHits("/v1/agent/swap-account"),
		"the swap request crossed exactly once")
	assert.Nil(t, inst.InFlightOffBoxSwap(), "the record settled with the commit")
	assert.Empty(t, inst.PendingHandoffMission(),
		"the committed swap's notice was delivered and retired — its final verdict settled")
	assert.Empty(t, inst.PendingHandoffDeliveryStatus(),
		"and the verdict record cleared with it")
	assert.False(t, inst.LimitReached(), "the outgoing account's wall cleared with the record")
}

// The shape the hold can actually starve: an AMBIGUOUS verdict at the wall has
// no automatic exit — handoff recovery will not replay it (that belongs to
// not-delivered), the at-the-wall transfer does not apply, and the operator's
// confirm-or-retry is the only discharge. On master the scheduler still ran
// the row's scheduled same-account resume — the "un-stall" the wall remedy
// describes. Off-box arm: the swap stays withheld while the resume runs.
func TestAmbiguousOwedMissionStillGetsItsSameAccountResumeOffBox(t *testing.T) {
	manager, _, _, inst, srv := newOffBoxLimitedFixture(t)
	oweAMission(t, inst)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(owedMission, session.PromptCouldNotConfirm))

	manager.ResumePendingHandoffs()
	require.Equal(t, owedMission, inst.PendingHandoffMission(),
		"an ambiguous verdict is the operator's call — recovery neither replays nor transfers it")
	require.Equal(t, session.PromptCouldNotConfirm, inst.PendingHandoffDeliveryStatus())

	manager.ResumeLimitedSessions()

	assert.Zero(t, srv.routeHits("/v1/agent/swap-account"),
		"the owed mission still withholds the account swap")
	assert.GreaterOrEqual(t, srv.routeHits("/v1/agent/send-prompt"), 1,
		"the scheduled same-account resume ran anyway")
	acct, _ := inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row resumes on the account it already had")
	assert.Equal(t, owedMission, inst.PendingHandoffMission(),
		"the ambiguous mission is still owed — the resume does not discharge it")
	assert.Equal(t, session.PromptCouldNotConfirm, inst.PendingHandoffDeliveryStatus())
	assert.False(t, inst.LimitReached(), "the resume un-stalled the wall")
}

// Same property on a LOCAL row — the owed-mission hold is an off-box swap
// rule, so a local ambiguous row at the wall must resume exactly as it did
// before the gate existed.
func TestAmbiguousOwedMissionStillGetsItsSameAccountResumeLocal(t *testing.T) {
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "stored prompt", nowFunc().Add(-time.Hour))
	inst.SetPendingHandoffMission(owedMission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(owedMission, session.PromptCouldNotConfirm))

	manager.ResumePendingHandoffs()
	require.Equal(t, owedMission, inst.PendingHandoffMission())

	manager.ResumeLimitedSessions()

	_, _, prompts := backend.snapshot()
	require.Equal(t, []string{"stored prompt"}, prompts,
		"the owed mission withholds only the swap — the scheduled same-account resume still sends the stored prompt")
	assert.Equal(t, owedMission, inst.PendingHandoffMission(),
		"the ambiguous mission is still owed")
	assert.False(t, inst.LimitReached(), "the resume un-stalled the wall")
}
