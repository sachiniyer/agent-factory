package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// listPrograms answers from the agent enum and a resolved config — it never
// touches session state — so a controlServer carrying nothing but a manager (for
// the global default) is the whole fixture.
func listPrograms(t *testing.T, mgr *Manager, repoPath string) ListProgramsResponse {
	t.Helper()
	var resp ListProgramsResponse
	require.NoError(t, (&controlServer{manager: mgr}).ListPrograms(ListProgramsRequest{RepoPath: repoPath}, &resp))
	return resp
}

// programNames flattens the catalog to the wire values a client would render.
func programNames(resp ListProgramsResponse) []string {
	out := make([]string, 0, len(resp.Programs))
	for _, opt := range resp.Programs {
		out = append(out, opt.Name)
	}
	return out
}

// recordProgramThenFailTheCreate installs a backend factory that records the
// InstanceOptions the daemon handed session.NewInstance and then FAILS, so the
// create returns immediately.
//
// Failing on purpose is what keeps this fast and on-topic. The contract under test
// is which PROGRAM a create with no explicit program resolves to — decided at the
// top of CreateSession, before anything is provisioned — and the options are
// recorded either way. Letting the create proceed instead drags in the readiness
// poll, which is AGENT-SPECIFIC: the fake backend's pane ("ready\n❯") satisfies
// claude's heuristic and not gemini's, so simply changing the default agent parks
// the test on the full 60s start timeout for a reason that has nothing to do with
// defaulting. (That is not hypothetical — it is what the first version of this test
// did.)
func recordProgramThenFailTheCreate(t *testing.T) *[]session.InstanceOptions {
	t.Helper()
	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		seen = append(seen, opts)
		return nil, errors.New("programs_test: create stopped after the program was resolved")
	})
	t.Cleanup(restore)
	return &seen
}

// writeRepoProgramConfig writes an in-repo .agent-factory/config.json carrying a
// default_program — the file a user edits to make one repo default to a different
// agent than the rest.
func writeRepoProgramConfig(t *testing.T, repoRoot, program string) {
	t.Helper()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	blob, err := json.Marshal(map[string]any{"default_program": program})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), blob, 0o644))
}

// TestListPrograms_OffersEverySupportedProgram is the core of #1970: the daemon —
// not a hand-typed list in a client — decides which agents a create may select,
// and it offers them in the canonical order clients render.
func TestListPrograms_OffersEverySupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)

	resp := listPrograms(t, nil, repo)

	assert.Equal(t, tmux.SupportedPrograms, programNames(resp),
		"the catalog is tmux.SupportedPrograms verbatim, in its canonical (append-only) order")
}

// TestListPrograms_NewProgramReachesClientsWithNoClientChange is THE anti-drift
// test for #1970 (the daemon half; web/src/programs.test.ts is the other half).
//
// It is the acceptance criterion the issue names: add a seventh agent the way a
// real one is added — one entry in the canonical enum — and it must flow out of
// the RPC, and therefore into every client that renders the response, with no
// edit to this handler and none to web/src. If someone later reintroduces a
// hardcoded list inside ListPrograms, this fails.
func TestListPrograms_NewProgramReachesClientsWithNoClientChange(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)

	require.NotContains(t, tmux.SupportedPrograms, "cursor", "precondition: the fake agent must not already exist")

	prev := tmux.SupportedPrograms
	tmux.SupportedPrograms = append(append([]string{}, prev...), "cursor")
	t.Cleanup(func() { tmux.SupportedPrograms = prev })

	resp := listPrograms(t, nil, repo)

	assert.Contains(t, programNames(resp), "cursor")
	assert.Equal(t, "cursor", resp.Programs[len(resp.Programs)-1].Name,
		"it lands in enum order, not appended by a special case")
}

// TestListPrograms_AnswersWithoutARepo pins the deliberate difference from
// ListBackends (which errors on a repo-less request).
//
// A backend's availability is a property of a specific repo, so there is no
// repo-less answer. The agent ENUM is global — the same agents exist everywhere —
// so a caller with no repo in hand still deserves the list. Only Default is
// repo-scoped, and it falls back to the global default exactly as a create does.
func TestListPrograms_AnswersWithoutARepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	mgr := &Manager{cfg: &config.Config{DefaultProgram: tmux.ProgramCodex}}

	resp := listPrograms(t, mgr, "")

	assert.Equal(t, tmux.SupportedPrograms, programNames(resp), "the enum does not depend on a repo")
	assert.Equal(t, tmux.ProgramCodex, resp.Default, "with no repo, the default is the daemon's global default")
}

// TestListPrograms_DefaultSources pins where Default actually comes from, and the
// one case where there is none.
//
// The two sources are not interchangeable, and the precedence is inherited from
// the create path rather than chosen here: a RESOLVABLE repo answers from its
// resolved config (global config file overlaid by the in-repo one), and the
// manager's in-memory global default is only the FALLBACK for when the repo cannot
// be resolved. So a nil manager does not make the default unknown — the repo still
// answers.
//
// The empty case is the honesty case, and the reason Default may be empty on the
// wire: when NEITHER source has an answer, the response says so instead of naming
// the obvious guess. A picker promising a choice nobody made is the failure this
// whole issue is about, one layer down. Clients render "Repo default" with no
// parenthetical (web/src/programs.ts).
func TestListPrograms_DefaultSources(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("a resolvable repo answers from its resolved config, with or without a manager", func(t *testing.T) {
		repo := setupControlRepo(t)
		writeRepoProgramConfig(t, repo, tmux.ProgramGemini)

		assert.Equal(t, tmux.ProgramGemini, listPrograms(t, nil, repo).Default,
			"the repo's own default_program is the answer — the manager is not consulted when the repo resolves")
	})

	t.Run("with neither a manager nor a repo there is no default to report", func(t *testing.T) {
		resp := listPrograms(t, nil, "")

		assert.Empty(t, resp.Default, "an unknown default is reported as unknown, not guessed")
		assert.NotEmpty(t, resp.Programs, "the enum is still served — only the default is unknown")
	})
}

// TestListPrograms_DefaultMatchesTheCreatePath is the contract the web relies on
// to avoid overriding a repo default: Default reports what a create with NO
// explicit program actually resolves to. The web renders it as a label and sends
// nothing.
//
// It is deliberately driven end-to-end through Manager.CreateSession rather than
// asserted against defaultProgramFor, which would be tautological now that both
// call it. And it discriminates: the repo declares an agent that is NOT the global
// default, so a catalog (or a create) that ignored the in-repo override would
// report the global "claude" and fail here. Pinning a repo whose default happened
// to equal the global one would pass no matter which side was wrong.
func TestListPrograms_DefaultMatchesTheCreatePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := recordProgramThenFailTheCreate(t)
	repo := setupControlRepo(t)

	global := config.DefaultConfig()
	require.NotEqual(t, tmux.ProgramGemini, global.DefaultProgram,
		"precondition: the repo override must differ from the global default, or this test cannot tell them apart")
	writeRepoProgramConfig(t, repo, tmux.ProgramGemini)

	mgr, err := NewManager(global)
	require.NoError(t, err)

	// What a real create with no explicit program picks. The create is EXPECTED to
	// fail — see recordProgramThenFailTheCreate — because the program is chosen long
	// before anything is provisioned, and letting the create run on would test the
	// readiness poll rather than the defaulting.
	_, err = mgr.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "captain-default-program",
		RepoPath: repo,
		Program:  "",
	})
	require.Error(t, err, "the recording fixture fails the create on purpose")
	require.Len(t, *seen, 1)
	created := (*seen)[0].Program

	// What the catalog tells a client that create will pick.
	reported := listPrograms(t, mgr, repo).Default

	assert.Equal(t, tmux.ProgramGemini, created, "a create with no program honours the repo's default_program")
	assert.Equal(t, created, reported,
		"the program the catalog labels 'repo default' must be the one a real create picks — "+
			"they are the same decision function (defaultProgramFor) precisely so they cannot disagree")
}

// TestDefaultProgramFor_FallsBackWhenTheRepoCannotBeResolved pins the fallback
// half of the shared decision function.
//
// An unresolvable repo path does not fail the create — reserveCreate surfaces the
// path problem right after, with more context — so the program falls back to the
// global default. The catalog must describe that same behavior rather than
// reporting an empty default for a create that will in fact run claude.
func TestDefaultProgramFor_FallsBackWhenTheRepoCannotBeResolved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	assert.Equal(t, tmux.ProgramAider, defaultProgramFor(tmux.ProgramAider, ""),
		"no repo path at all falls back to the global default")
	assert.Equal(t, tmux.ProgramAider, defaultProgramFor(tmux.ProgramAider, filepath.Join(t.TempDir(), "not-a-repo")),
		"a path that is not a repo falls back to the global default")
}
