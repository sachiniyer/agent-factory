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

// The daemon-level pin for #7860's route on an off-box row (#7871 item 3): an
// automatic account swap attempted against a session that still owes a handoff
// mission is REFUSED — ErrOffBoxAccountSwapOwesMission — and the row must keep
// the mission, its verdict, and its wall exactly as they were. The scheduler
// must not even reach the attempt: the owed mission holds the row out of
// resumeLimitedSession entirely, so nothing is tried (no fence raise, no
// agent-server probe, no config lock) until the mission's state changes
// (#7871 item 1). Only after the mission transfers does the swap proceed.

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
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"content": "", "has_terminal_modes": false}})
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

func (s *offBoxAgentServer) totalHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.hits {
		total += n
	}
	return total
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

// Item 1 + item 3's property: a session that still owes a handoff mission
// yields NO swap opportunity — the scheduler does not even raise the fence or
// probe the agent-server. On master the same pass walks the whole attempt —
// the liveness probe hits the agent-server and a retry attempt is recorded —
// before the swap admission refuses it.
func TestResumeLimitedSessionsNeverAttemptsAnOwedMissionRow(t *testing.T) {
	manager, repoID, _, inst, srv := newOffBoxLimitedFixture(t)
	oweAMission(t, inst)

	manager.ResumeLimitedSessions()

	assert.Zero(t, srv.totalHits(),
		"an owed mission yields no swap opportunity: no probe, no swap call, no send")
	_, hasState := manager.limitResumeStates[stableSessionKey(repoID, inst)]
	assert.False(t, hasState, "no resume attempt may be recorded against the owed row")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the limit-resume fence was never raised")
	assert.Equal(t, owedMission, inst.PendingHandoffMission())
	assert.Equal(t, session.PromptNotDelivered, inst.PendingHandoffDeliveryStatus())
	assert.True(t, inst.LimitReached(), "the wall is the owed row's state, unchanged")
}

// The route itself (#7860): driving the attempt on the owed row — the shape
// the scheduler produced on master before item 1 — is refused by the swap
// admission, the fence drops, the mission and its verdict persist untouched,
// and no ordinary fallback send fires past the refusal.
func TestOffBoxSwapRefusalPersistsTheOwedMissionAndItsVerdict(t *testing.T) {
	manager, repoID, _, inst, srv := newOffBoxLimitedFixture(t)
	oweAMission(t, inst)
	key := daemonInstanceKey(repoID, "offbox")

	swap, err := manager.accountSwapOpportunityFromFactsWithEvidence(inst, manager.Config(), loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotNil(t, swap, "the owed mission is not a swap hint — the refusal owns the row")
	swap.fallbackDue = true

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
	assert.False(t, inst.LimitReached(), "the outgoing account's wall cleared with the record")
}
