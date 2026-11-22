package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7368: an off-box account swap must carry af's managed guidance skill into
// the INCOMING scope, exactly as a local swap lands it under the incoming
// account's directory. The in-sandbox plan build resolves its skill target
// unscoped, so the agent-server's own environment answers with the OUTGOING
// live scope — the directory the rotation then renames to account.prev and
// deletes. Unless staging writes the skill into account.next, a codex or
// gemini session relaunches on every off-box swap (operator handoff and
// automatic failover alike) without the guidance global_agent_skills
// promised.

// scopeSkillServer is the in-sandbox agent-server for a scope-discovered
// agent: it runs the REAL scope rotation (newScopeRotation reads the agent's
// scope variable from its own environment) and records what the staged scope
// held at the moment it was asked — the point after which the daemon can no
// longer add anything to it.
type scopeSkillServer struct {
	AgentServer
	live       string
	agent      string
	scopeEnv   string // the agent's scope variable (CODEX_HOME, GEMINI_CLI_HOME)
	can        bool
	failAfter  error // returned after a successful rotation, to exercise restore
	lastScoped bool  // the request's scoped_stage flag — this build honors it
	rec        accountSwapRecorder
	calls      int
	atCall     map[string]string // scope-relative path -> staged content at swap time
}

func (s *scopeSkillServer) CanSwapAccount() (bool, error) { return s.can, nil }

func (s *scopeSkillServer) AccountSwapStatus() (string, string, string, error) {
	o, p, id := s.rec.status()
	return o, p, id, nil
}

func (s *scopeSkillServer) ScopedStageAccountSwap() (bool, error) { return true, nil }
func (s *scopeSkillServer) SwapAccountInPlace(swapID string, scopedStage bool) (string, error) {
	s.lastScoped = scopedStage
	return s.rec.run(swapID, func() (string, error) { return s.swap(swapID) })
}

// DiscardAccountNext is the serialized staged-scope discard (#7773 R2): under
// the swap's own lock it tombstones the id and removes the staged scope, so a
// request still queued behind it can never commit. It enforces the stage
// marker exactly as production does (#7788): only this swap's bound stage is
// removed, and an unreadable marker fails rather than passing silently. Both
// spellings are probed — the discard cannot see which staging protocol the
// pair negotiated (#7793).
func (s *scopeSkillServer) DiscardAccountNext(swapID string) error {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if swapID != "" {
		s.rec.cancelled = swapID
	}
	paths := []string{s.live + accountNextSuffix}
	if swapID != "" {
		paths = append(paths, stagedScopeDir(s.live, swapID))
	}
	for _, next := range paths {
		if _, statErr := os.Lstat(next); errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if dErr := discardAccountNextBound(next, swapID); dErr != nil {
			return dErr
		}
	}
	// The stage is provably gone, so the tombstone publishes as this swap's
	// own terminal verdict — replayable by a daemon whose record clear was
	// lost (#7793), exactly like the real server's noteStagedDiscarded.
	s.rec.refuseLocked(swapID)
	return nil
}

func (s *scopeSkillServer) swap(id string) (string, error) {
	s.calls++
	os.Setenv(s.scopeEnv, s.live) // the server env names the live scope; the rotation reads it
	staged := s.live + accountNextSuffix
	if s.lastScoped {
		staged = stagedScopeDir(s.live, id)
	}
	for rel := range s.atCall {
		s.atCall[rel] = readOrEmpty(filepath.Join(staged, filepath.FromSlash(rel)))
	}
	rot, err := newScopeRotation(s.agent, id, s.lastScoped)
	if err != nil {
		return "", err
	}
	if err := rot.rotate(); err != nil {
		return "", err
	}
	if s.failAfter != nil {
		if rbErr := rot.restore(); rbErr != nil {
			return "", errors.Join(s.failAfter, rbErr)
		}
		return "", s.failAfter
	}
	return s.agent, nil
}

// scopeSkillFixture is the daemon half of an off-box swap for an agent whose
// skill is discovered under the account's config root: the session dir stands
// in for the host, localShell runs the staging scripts for real, and the
// agent's scope variable points at the live scope the way the agent-server's
// launch environment does.
type scopeSkillFixture struct {
	inst     *Instance
	w        *sandboxWorkspace
	srv      *scopeSkillServer
	skillRel string // the skill's tar-relative name inside a scope
}

// swapPolicyBytes renders the policy snapshot an agent-server installs from
// af-policy.json — the create-or-restore-time global_agent_skills answer the
// in-sandbox launch acts on (#7644).
func swapPolicyBytes(t *testing.T, granted bool) []byte {
	t.Helper()
	data, err := config.AgentPolicy{Version: config.AgentPolicyVersion,
		Global: config.PolicyLayer{GlobalAgentSkills: granted}}.Marshal()
	require.NoError(t, err)
	return data
}

func newScopeSkillFixture(t *testing.T, agent string, skillsGranted bool) *scopeSkillFixture {
	t.Helper()
	skillRel, ok := scopeDiscoveredSkillRel(agent)
	require.True(t, ok, "fixture is for agents whose skill is scope-discovered")

	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.GlobalAgentSkills = skillsGranted
	require.NoError(t, config.SaveConfig(cfg))

	credRel := accountCredentialRel[agent][0]
	workDir, err := agentaccount.Register(home, agent, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, agent, "alt")
	require.NoError(t, err)
	for _, d := range []string{workDir, altDir} {
		p := filepath.Join(d, filepath.FromSlash(credRel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte("token"), 0o600))
	}

	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")

	dir := t.TempDir()
	p := &sandboxProvisioner{sessionDir: dir}
	p.runCommandFn = localShell{}.Run
	acct := sessionenv.Account{Agent: agent, Name: "work", Dir: workDir}
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{
		Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target, Program: agent,
		// The policy snapshot provision ships beside the session — the
		// create-time global_agent_skills answer the in-sandbox launch acts
		// on. The swap's staging asks that same snapshot, never the daemon's
		// live config, so the two sides cannot disagree (#7644).
		AgentPolicy: swapPolicyBytes(t, skillsGranted),
	}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	envVar, ok := sessionenv.SupportsAccounts(agent)
	require.True(t, ok)
	t.Setenv(envVar, w.AccountDir()) // the agent-server's env names the live scope

	srv := &scopeSkillServer{
		live: w.AccountDir(), agent: agent, scopeEnv: envVar,
		can: true, atCall: map[string]string{skillRel: ""},
	}
	stubAccountSwapID(t)
	inst := &Instance{
		Title: "scopeswap", Path: repo, Program: agent, Host: "box1",
		Account: "work", accountAgent: agent,
		backend: &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir, HostName: "box1"}},
		started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv
	inst.SetOffBoxSwapPersist(func(*Instance) error { return nil })
	return &scopeSkillFixture{inst: inst, w: w, srv: srv, skillRel: skillRel}
}

// seedLiveSkill is the provision-time write the first launch's unscoped
// resolution makes into the live scope — the file every off-box swap today
// carries into account.prev and deletes.
func (f *scopeSkillFixture) seedLiveSkill(t *testing.T) {
	t.Helper()
	p := filepath.Join(f.w.AccountDir(), filepath.FromSlash(f.skillRel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(afSkillDoc), 0o600))
}

func (f *scopeSkillFixture) liveSkill() string {
	return filepath.Join(f.w.AccountDir(), filepath.FromSlash(f.skillRel))
}

// scopeSkillAgents are the account-scopable agents whose guidance is a file
// discovered under the account's config root — the only agents an off-box
// scope rotation can lose guidance for.
var scopeSkillAgents = []struct {
	agent string
}{
	{agent: tmux.ProgramCodex},
	{agent: tmux.ProgramGemini},
}

// The core #7368 regression: with global_agent_skills granted, the staged
// scope must hold af's skill when the agent-server is asked to rotate, so the
// relaunched agent reads its guidance from the scope it lands in.
func TestSwapOffBoxAccountStagesTheAfSkill(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true)
			f.seedLiveSkill(t) // what provision's launch left in the outgoing scope

			_, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			assert.Equal(t, afSkillDoc, f.srv.atCall[f.skillRel],
				"the skill was already in the staged scope when the agent-server was asked — nothing can add it later")
			got, err := os.ReadFile(f.liveSkill())
			require.NoError(t, err, "the relaunched agent reads the skill from the scope it lands in")
			assert.Equal(t, afSkillDoc, string(got), "af's own managed skill, not a copy of whatever the outgoing scope held")
			assert.NoDirExists(t, f.w.PrevAccountDir(),
				"the outgoing scope — and the write the unscoped plan build made into it — is cleaned up, not rescued")
			assert.NoDirExists(t, f.w.nextAccountDirFor("test-swap-1", true))
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "alt", acct)
		})
	}
}

// The gate is unchanged: with the default global_agent_skills = false the
// staged scope carries no skill file at all, just as provision's scope never
// got one.
func TestSwapOffBoxAccountStagesNoSkillWhenDeclined(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, false)

			_, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			assert.Empty(t, f.srv.atCall[f.skillRel],
				"the staged scope held no skill file when the rotation ran")
			assert.NoFileExists(t, f.liveSkill(),
				"global_agent_skills = false writes nothing into any scope")
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "alt", acct)
		})
	}
}

// A swap that fails before the rotation must leave the outgoing live scope —
// including the guidance its agent is still reading — exactly as it was.
func TestSwapOffBoxAccountStageFailureKeepsTheOutgoingSkill(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true)
			f.seedLiveSkill(t)
			p := f.inst.currentBackend().(*sshBackend).provisioner
			p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
				return nil, errors.New("ssh: connection refused")
			}

			_, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.ErrorContains(t, err, "staging account")

			assert.Zero(t, f.srv.calls, "the agent-server is never asked to stop anything")
			got, readErr := os.ReadFile(f.liveSkill())
			require.NoError(t, readErr, "the live scope's guidance survives a swap that never reached the rotation")
			assert.Equal(t, afSkillDoc, string(got))
			assert.NoDirExists(t, f.w.nextAccountDirFor("test-swap-1", true))
			assert.NoDirExists(t, f.w.PrevAccountDir(), "no rotation happened")
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "work", acct)
		})
	}
}

// A failure AFTER the rotation puts the outgoing scope back: the live scope's
// skill is intact for the agent that is still running, and the staged copy —
// which is what the next swap's staging would replace anyway — is gone.
func TestSwapOffBoxAccountRolledBackSwapKeepsTheOutgoingSkill(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true)
			f.seedLiveSkill(t)
			f.srv.failAfter = errors.New("the new agent did not start")

			_, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.ErrorContains(t, err, "did not start")

			got, readErr := os.ReadFile(f.liveSkill())
			require.NoError(t, readErr, "the rollback returns the outgoing scope — and its guidance — to live")
			assert.Equal(t, afSkillDoc, string(got))
			assert.NoDirExists(t, f.w.nextAccountDirFor("test-swap-1", true))
			assert.NoDirExists(t, f.w.PrevAccountDir())
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "work", acct)
		})
	}
}

// runRelaunchSkillEnsure is the in-sandbox relaunch's own skill step: the same
// ensure the launch's plan build calls, run under the policy snapshot the
// agent-server installs — the one consent answer this swap's staging must
// match (#7644). It is installed only after the swap returns: until then this
// test process speaks for the daemon, which has no policy installed.
func (f *scopeSkillFixture) runRelaunchSkillEnsure(t *testing.T) {
	t.Helper()
	policy, err := config.ParseAgentPolicy(f.w.spec.AgentPolicy)
	require.NoError(t, err)
	config.InstallAgentPolicy(policy)
	t.Cleanup(config.ClearAgentPolicy)
	target := skillTarget{root: f.w.AccountDir()}
	switch f.srv.agent {
	case tmux.ProgramCodex:
		_, err = ensureCodexSkillDir(target)
	case tmux.ProgramGemini:
		_, err = ensureGeminiSkillDir(target)
	}
	require.NoError(t, err)
}

// #7644: the staged scope and the in-sandbox relaunch must act on ONE consent
// answer — the policy snapshot shipped at create or restore — never the
// daemon's live config, which a swap does not refresh. Here the operator
// opted in AFTER the create that shipped a declined snapshot: the staged
// scope carries no skill, and the relaunch's own write under that same
// snapshot leaves the live scope without one — both sides answered "no".
// Staging from the live config (what master does here) writes a skill the
// relaunch then deletes: the flap the issue names.
func TestSwapOffBoxAccountStagesWithTheSnapshotConsentFlippedToGranted(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, false) // create shipped a declined snapshot
			// The operator opted in after create: the live config now grants.
			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			cfg.GlobalAgentSkills = true
			require.NoError(t, config.SaveConfig(cfg))

			_, err = f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			assert.Empty(t, f.srv.atCall[f.skillRel],
				"the staged scope follows the snapshot's declined answer, not the live config's grant")
			assert.NoFileExists(t, f.liveSkill())
			f.runRelaunchSkillEnsure(t)
			assert.NoFileExists(t, f.liveSkill(),
				"the relaunch under the same snapshot writes nothing either — one answer on both sides")
		})
	}
}

// The mirror: consent revoked after the create that shipped a granted
// snapshot. Staging and the relaunch both act on the snapshot's "yes": the
// skill lands and stays. On the live config (master), the staged scope gets
// nothing while the relaunch writes it back — the same flap, reversed.
func TestSwapOffBoxAccountStagesWithTheSnapshotConsentFlippedToDeclined(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true) // create shipped a granted snapshot
			f.seedLiveSkill(t)                           // what provision's granted launch left
			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			cfg.GlobalAgentSkills = false // the operator revoked after create
			require.NoError(t, config.SaveConfig(cfg))

			_, err = f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			assert.Equal(t, afSkillDoc, f.srv.atCall[f.skillRel],
				"the staged scope follows the snapshot's granted answer, not the live revocation")
			f.runRelaunchSkillEnsure(t)
			got, err := os.ReadFile(f.liveSkill())
			require.NoError(t, err)
			assert.Equal(t, afSkillDoc, string(got),
				"the relaunch under the same snapshot keeps the same skill — one answer on both sides")
		})
	}
}

// A consent answer af cannot establish is UNKNOWN — an unreadable config is
// not a "no" (#1933) — and on this side of the write it must never count as a
// "yes" either: the staged scope gets no skill, and the swap says the guidance
// did not land through the same skip list a refused sandbox_agent_config file
// rides (#6337's channel, no new surface). This also pins the mutation #7644
// names: the gate written `!= globalSkillGranted` keeps unknown silent; a gate
// written `== globalSkillDeclined` would stage the skill on an unreadable
// answer and fail every variant below.
func TestSwapOffBoxAccountUnknownConsentStagesNoSkillAndSaysSo(t *testing.T) {
	variants := []struct {
		name       string
		prep       func(t *testing.T, f *scopeSkillFixture)
		wantReason string // daemon-local text the skip entry's reason must carry
	}{
		{"the shipped policy does not parse", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = []byte("{not a policy")
		}, "the snapshot is not valid JSON"},
		{"the restored workspace's policy file is gone", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = nil // a spec rebuilt from the row holds no copy
			f.w.restored = true
		}, "could not be read on the host"},
		{"the restored workspace's policy file does not parse", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = nil
			f.w.restored = true
			require.NoError(t, os.WriteFile(f.w.PolicyPath(), []byte("{not a policy"), 0o600))
		}, "the snapshot is not valid JSON"},
		// A daemon restart onto a newer binary leaves the host's policy file at
		// the old build's schema — the nightly upgrade case (#7644 R2). A
		// version or a field this af does not know is named as another af's
		// snapshot rather than generic damage.
		{"the snapshot is a version this af does not know", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = []byte(`{"version":2,"global":{"global_agent_skills":true}}`)
		}, "the snapshot is from another af version"},
		{"the snapshot carries a field this af does not know", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = []byte(`{"version":1,"surprise":true}`)
		}, "the snapshot is from another af version"},
		// The read-back's size bound: an over-limit reply is unknown, never a
		// truncated policy (#7644 R3). Once through the provisioner's RunLimited
		// and once through a shell with no bounded-read capability.
		{"the policy file is over the read bound", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = nil
			require.NoError(t, os.WriteFile(f.w.PolicyPath(), make([]byte, accountSyncMaxFileBytes+1), 0o600))
		}, "is larger than the bound af reads it under"},
		{"the policy file is over the read bound on a shell without RunLimited", func(t *testing.T, f *scopeSkillFixture) {
			f.w.spec.AgentPolicy = nil
			f.w.shell = localShell{} // a sandboxShell with no bounded-read capability
			require.NoError(t, os.WriteFile(f.w.PolicyPath(), make([]byte, accountSyncMaxFileBytes+1), 0o600))
		}, "is larger than the bound af reads it under"},
	}
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			for _, v := range variants {
				t.Run(v.name, func(t *testing.T) {
					f := newScopeSkillFixture(t, tc.agent, true) // the live config GRANTS
					v.prep(t, f)

					res, err := f.inst.SwapOffBoxAccount("alt", false, "")
					require.NoError(t, err)

					assert.Empty(t, f.srv.atCall[f.skillRel],
						"an unknown consent answer stages no skill, whatever the live config says")
					assert.NoFileExists(t, f.liveSkill())
					require.NotEmpty(t, res.AgentConfigSkipped,
						"the swap's result says the guidance did not land")
					var entry string
					for _, e := range res.AgentConfigSkipped {
						if strings.Contains(e, f.skillRel) {
							entry = e
						}
					}
					require.NotEmpty(t, entry, "one skip entry names the skill file that was not staged")
					assert.Contains(t, entry, tc.agent+":")
					assert.Contains(t, entry, v.wantReason,
						"the reason says which kind of unreadable the snapshot was")
					assert.Equal(t, res.AgentConfigSkipped, f.inst.AgentConfigSkipped(),
						"the row carries the same skip list the swap result reported")
				})
			}
		})
	}
}

// A failed read's reply is host-authored bytes — a FIFO parked where the
// policy file should be can emit a megabyte of escape codes and newlines, and
// the skip list's "; "-join would let them forge extra agent:file entries.
// None of it may reach a skip entry, a row, or a warning (#7644 R1): the
// entry's reason is fixed daemon-local text, asserted here in its exact form.
func TestSwapOffBoxAccountUnknownConsentEntryCarriesNoHostBytes(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true)
			f.w.spec.AgentPolicy = nil // force the host read
			reply := strings.Repeat("\x1b]8;;forged\x07claude:evil.txt: a forged entry\r\n", 20000) +
				strings.Repeat("x", accountSyncMaxFileBytes)
			p := f.inst.currentBackend().(*sshBackend).provisioner
			p.runCommandFn = func(d time.Duration, script string, in io.Reader, interactive bool) ([]byte, error) {
				if strings.Contains(script, "af-policy.json") {
					return []byte(reply), errors.New("exit status 1")
				}
				return localShell{}.Run(d, script, in, interactive)
			}

			res, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			want := fmt.Sprintf("%s:%s: af could not confirm whether the sandbox's operator policy grants global_agent_skills: %s could not be read on the host",
				tc.agent, f.skillRel, f.w.PolicyPath())
			require.Equal(t, []string{want}, res.AgentConfigSkipped,
				"one fixed daemon-local entry — none of the reply's bytes, newlines, or forged entries landed")
			assert.Empty(t, f.srv.atCall[f.skillRel])
			assert.NoFileExists(t, f.liveSkill())
		})
	}
}

// A workspace rebuilt from the session row after a daemon restart keeps no
// copy of the shipped policy; the swap reads the consent back from the file
// the agent-server installed it from — the same file a later agent-server
// restart would re-install — so a reattached session's swap stages with the
// same answer as anywhere else.
func TestSwapOffBoxAccountOnARestoredWorkspaceStagesWithTheShippedPolicy(t *testing.T) {
	for _, tc := range scopeSkillAgents {
		t.Run(tc.agent, func(t *testing.T) {
			f := newScopeSkillFixture(t, tc.agent, true)
			f.w.spec.AgentPolicy = nil // a spec rebuilt from the row holds no copy
			f.w.restored = true
			require.NoError(t, os.WriteFile(f.w.PolicyPath(), swapPolicyBytes(t, true), 0o600))

			res, err := f.inst.SwapOffBoxAccount("alt", false, "")
			require.NoError(t, err)

			assert.Equal(t, afSkillDoc, f.srv.atCall[f.skillRel],
				"the policy file provision left beside the session still answers the staging question")
			got, err := os.ReadFile(f.liveSkill())
			require.NoError(t, err)
			assert.Equal(t, afSkillDoc, string(got))
			assert.Empty(t, res.AgentConfigSkipped)
		})
	}
}
