package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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
	live, next string
	agent      string
	scopeEnv   string // the agent's scope variable (CODEX_HOME, GEMINI_CLI_HOME)
	can        bool
	failAfter  error // returned after a successful rotation, to exercise restore
	rec        accountSwapRecorder
	calls      int
	atCall     map[string]string // scope-relative path -> staged content at swap time
}

func (s *scopeSkillServer) CanSwapAccount() (bool, error) { return s.can, nil }

func (s *scopeSkillServer) AccountSwapStatus() (string, string, error) {
	o, p := s.rec.status()
	return o, p, nil
}

func (s *scopeSkillServer) SwapAccountInPlace() (string, error) {
	return s.rec.run(s.swap)
}

func (s *scopeSkillServer) swap() (string, error) {
	s.calls++
	os.Setenv(s.scopeEnv, s.live) // the server env names the live scope; the rotation reads it
	for rel := range s.atCall {
		s.atCall[rel] = readOrEmpty(filepath.Join(s.next, filepath.FromSlash(rel)))
	}
	rot, err := newScopeRotation(s.agent)
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

// scopeDiscoveredSkillRel is the path of af's managed skill inside an account
// scope, mirroring the skills-base function the launch's own write resolves
// through (codexSkillsBase, geminiSkillsBase).
func scopeDiscoveredSkillRel(agent string) (string, bool) {
	switch agent {
	case tmux.ProgramCodex:
		return "skills/" + afSkillDirName + "/SKILL.md", true
	case tmux.ProgramGemini:
		return ".gemini/skills/" + afSkillDirName + "/SKILL.md", true
	default:
		return "", false
	}
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
	}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	envVar, ok := sessionenv.SupportsAccounts(agent)
	require.True(t, ok)
	t.Setenv(envVar, w.AccountDir()) // the agent-server's env names the live scope

	srv := &scopeSkillServer{
		live: w.AccountDir(), next: w.NextAccountDir(), agent: agent, scopeEnv: envVar,
		can: true, atCall: map[string]string{skillRel: ""},
	}
	inst := &Instance{
		Title: "scopeswap", Path: repo, Program: agent, Host: "box1",
		Account: "work", accountAgent: agent,
		backend: &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir, HostName: "box1"}},
		started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv
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

			_, err := f.inst.SwapOffBoxAccount("alt", false)
			require.NoError(t, err)

			assert.Equal(t, afSkillDoc, f.srv.atCall[f.skillRel],
				"the skill was already in the staged scope when the agent-server was asked — nothing can add it later")
			got, err := os.ReadFile(f.liveSkill())
			require.NoError(t, err, "the relaunched agent reads the skill from the scope it lands in")
			assert.Equal(t, afSkillDoc, string(got), "af's own managed skill, not a copy of whatever the outgoing scope held")
			assert.NoDirExists(t, f.w.PrevAccountDir(),
				"the outgoing scope — and the write the unscoped plan build made into it — is cleaned up, not rescued")
			assert.NoDirExists(t, f.w.NextAccountDir())
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

			_, err := f.inst.SwapOffBoxAccount("alt", false)
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

			_, err := f.inst.SwapOffBoxAccount("alt", false)
			require.ErrorContains(t, err, "staging account")

			assert.Zero(t, f.srv.calls, "the agent-server is never asked to stop anything")
			got, readErr := os.ReadFile(f.liveSkill())
			require.NoError(t, readErr, "the live scope's guidance survives a swap that never reached the rotation")
			assert.Equal(t, afSkillDoc, string(got))
			assert.NoDirExists(t, f.w.NextAccountDir())
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

			_, err := f.inst.SwapOffBoxAccount("alt", false)
			require.ErrorContains(t, err, "did not start")

			got, readErr := os.ReadFile(f.liveSkill())
			require.NoError(t, readErr, "the rollback returns the outgoing scope — and its guidance — to live")
			assert.Equal(t, afSkillDoc, string(got))
			assert.NoDirExists(t, f.w.NextAccountDir())
			assert.NoDirExists(t, f.w.PrevAccountDir())
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "work", acct)
		})
	}
}
