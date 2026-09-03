package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// registerAccount puts a real account in this test's af home and returns its
// directory, so the resolution under test runs against the same registry the
// account boundary reads rather than a hand-built path.
func registerAccount(t *testing.T, agent, name string) string {
	t.Helper()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	dir, err := agentaccount.Register(home, agent, name)
	require.NoError(t, err)
	return dir
}

// skillPathUnder is where af writes its guidance for each agent, given that
// agent's CONFIG ROOT — the value the account boundary installs for a scoped
// session, and the daemon's own for an unscoped one.
func geminiSkillPathUnder(root string) string {
	return filepath.Join(root, ".gemini", "skills", afSkillDirName, "SKILL.md")
}

func codexSkillPathUnder(root string) string {
	return filepath.Join(root, "skills", afSkillDirName, "SKILL.md")
}

// resolveSkillTarget answers three different questions, and the third one is the
// reason the type is not a bool (#3645).
func TestResolveSkillTarget_DistinguishesUnscopedFromUnresolvable(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)
	dir := registerAccount(t, "gemini", "work")

	unscoped := resolveSkillTarget(&Instance{Program: "gemini"}, "gemini")
	require.Equal(t, skillTarget{}, unscoped,
		"a session with no account reads the daemon's own config root, exactly as before")

	scoped := resolveSkillTarget(&Instance{Program: "gemini", Account: "work"}, "gemini")
	require.Equal(t, skillTarget{root: dir}, scoped,
		"a scoped session's skill belongs under the directory the boundary will install")

	// UNRESOLVABLE, not unscoped. Falling back to the daemon's root here is the
	// defect: it writes into a directory the operator did not select, for a session
	// that reads somewhere else.
	missing := resolveSkillTarget(&Instance{Program: "gemini", Account: "no-such-account"}, "gemini")
	require.True(t, missing.unresolved, "an account af cannot resolve must not fall back to the daemon's root")
	require.Empty(t, missing.root)

	// An account named for an agent that cannot be scoped at all. The launch
	// refuses this; until it does, af must not guess a directory.
	unsupported := resolveSkillTarget(&Instance{Program: "amp", Account: "work"}, "amp")
	require.True(t, unsupported.unresolved)

	// A FOREIGN NAMESPACE. The name was validated against the session's own agent,
	// and account namespaces are separate — "work" means a different identity for
	// each agent. Resolving it against the resolved command's registry would write
	// into a gemini account the operator never selected, moments before the launch
	// refuses for exactly that reason (#3082/#3108, #3645 review).
	foreign := resolveSkillTarget(&Instance{Program: "claude", Account: "work"}, "gemini")
	require.True(t, foreign.unresolved,
		"a claude account name must not be resolved against gemini's registry")
	require.Empty(t, foreign.root)
}

// A handoff is the path that reaches the foreign-namespace case in production.
//
// PrepareAgentSwap freezes the TARGET agent's command and runs injectSystemPrompt
// on it, and it runs BEFORE SwapAgent refuses every account-scoped handoff. So a
// claude session scoped to "work", handed off to gemini while a gemini account
// also called "work" exists, would write af's skill into that gemini account — an
// account the operator never selected, for a process that can never launch.
func TestPrepareAgentSwap_DoesNotWriteIntoTheTargetAgentsAccount(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)
	ambient := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", ambient)
	geminiDir := registerAccount(t, "gemini", "work")
	registerAccount(t, "claude", "work")

	scoped := &Instance{Program: "claude", Account: "work"}
	target := resolveSkillTarget(scoped, "gemini")
	require.True(t, target.unresolved, "the handoff target's account namespace is not this session's")

	injectSystemPrompt("gemini", target)
	require.NoFileExists(t, geminiSkillPathUnder(geminiDir),
		"a handoff that will be refused must not mutate the target agent's account directory")
	require.NoFileExists(t, geminiSkillPathUnder(ambient),
		"and it must not fall back to the daemon's root either")
}

// An af-MOUNTED account root belongs to the host, and the process reading it may
// be the agent-server inside the container af mounted it into.
//
// That server builds its instance with no account and the local backend, while
// docker has pointed the agent's config variable at /af-account — a writable bind
// mount of the host's account directory. Read as an ordinary ambient root, the
// container's own config (global_agent_skills defaults false) would DELETE the
// af-marked skill the host's scoped launch just wrote (#3645 review).
func TestResolveSkillTarget_RefusesAMountedAccountRoot(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)

	for name, root := range map[string]string{
		"the mount itself": dockerAccountHome,
		"a path inside it": dockerAccountHome + "/nested",
		"a trailing slash": dockerAccountHome + "/",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GEMINI_CLI_HOME", root)
			target := resolveSkillTarget(&Instance{Program: "gemini"}, "gemini")
			require.True(t, target.unresolved,
				"the container cannot know whether the host opted in, so it must neither write nor clean")
		})
	}

	// A SIBLING with the same prefix is not the mount. Compared as path segments,
	// never as a string prefix.
	t.Setenv("GEMINI_CLI_HOME", dockerAccountHome+"-backup")
	require.Equal(t, skillTarget{}, resolveSkillTarget(&Instance{Program: "gemini"}, "gemini"),
		"a sibling directory that merely shares the prefix is an ordinary ambient root")
}

// #1977's promise: af's edits to the user's global config must not outlive the
// decision not to make them. Moving the WRITE target to the account root must not
// quietly narrow it — an operator who launches only scoped sessions would
// otherwise keep a stale af-managed skill in their ambient config forever.
func TestEnsureSkillDir_DeclinedCleansTheLegacyAmbientLocationToo(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)
	ambient := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", ambient)
	dir := registerAccount(t, "gemini", "work")
	scoped := skillTarget{root: dir}

	// A prior af version wrote into the ambient root; this one writes into the
	// account root. Both af-marked, both af's to clean.
	_, err := ensureGeminiSkillDir(skillTarget{})
	require.NoError(t, err)
	_, err = ensureGeminiSkillDir(scoped)
	require.NoError(t, err)
	require.FileExists(t, geminiSkillPathUnder(ambient))
	require.FileExists(t, geminiSkillPathUnder(dir))

	// The operator now declines. A SCOPED launch must clean both.
	writeAfConfig(t, false)
	_, err = ensureGeminiSkillDir(scoped)
	require.NoError(t, err)
	require.NoFileExists(t, geminiSkillPathUnder(dir),
		"the account copy is af's and the operator declined")
	require.NoFileExists(t, geminiSkillPathUnder(ambient),
		"the ambient copy an older af left is af's too, and only a scoped launch may ever visit it again")
}

// The skills base is a function of the agent's CONFIG-ROOT VALUE, and scoping a
// session changes that value. One rule, two shapes: CODEX_HOME names the config
// directory itself, GEMINI_CLI_HOME is a HOME-like root the CLI appends .gemini/
// to (#3387).
func TestSkillsBaseDir_SubstitutesTheAccountRootForTheDaemonEnvironment(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", ambient)
	t.Setenv("CODEX_HOME", ambient)
	const account = "/afhome/accounts/x/work"

	gemini, err := geminiSkillsBaseDir(skillTarget{root: account})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(account, ".gemini", "skills"), gemini,
		"GEMINI_CLI_HOME is a HOME-like root, so the account directory gains the .gemini/ level")

	geminiAmbient, err := geminiSkillsBaseDir(skillTarget{})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(ambient, ".gemini", "skills"), geminiAmbient,
		"an unscoped session keeps reading the daemon's environment")

	codex, err := codexSkillsBaseDir(skillTarget{root: account})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(account, "skills"), codex,
		"CODEX_HOME names the config directory itself, so the account directory takes its place directly")

	codexAmbient, err := codexSkillsBaseDir(skillTarget{})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(ambient, "skills"), codexAmbient)

	for _, base := range []func(skillTarget) (string, error){geminiSkillsBaseDir, codexSkillsBaseDir} {
		path, err := base(skillTarget{unresolved: true})
		require.ErrorIs(t, err, errUnresolvedAccountSkillRoot)
		require.Empty(t, path, "an unresolved account must yield no path to write into")
	}
}

// THE #3645 REGRESSION.
//
// af writes its guidance from the daemon, before the account boundary installs the
// session's config root — so reading that root from the daemon's own environment
// described the daemon, not the session. An account-scoped codex or gemini session
// searched its account directory while af had written into the operator's ambient
// home, and the opted-in guidance was simply absent. Both halves are asserted: the
// skill lands where the session reads, AND it does not land where it used to.
func TestInjectSystemPrompt_WritesTheGuidanceWhereAScopedSessionReads(t *testing.T) {
	for _, tc := range []struct {
		agent    string
		envVar   string
		skillAt  func(root string) string
		accounts string
	}{
		{"gemini", "GEMINI_CLI_HOME", geminiSkillPathUnder, "gemini"},
		{"codex", "CODEX_HOME", codexSkillPathUnder, "codex"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			agentHome(t)
			grantGlobalAgentSkills(t)
			ambient := t.TempDir()
			t.Setenv(tc.envVar, ambient)
			dir := registerAccount(t, tc.accounts, "work")

			program := injectSystemPrompt(tc.agent,
				resolveSkillTarget(&Instance{Program: tc.agent, Account: "work"}, tc.agent))
			require.Equal(t, tc.agent, program,
				"the skill is placed through the filesystem; the command is not rewritten")

			require.FileExists(t, tc.skillAt(dir),
				"the scoped session reads its account directory, so the guidance has to be there")
			require.NoFileExists(t, tc.skillAt(ambient),
				"writing into the daemon's own config root is the defect: invisible to the session, and an "+
					"edit to a directory the operator did not select")

			// The unscoped control still writes where it always did, so the fix is a
			// redirection for scoped sessions rather than a change of default.
			injectSystemPrompt(tc.agent, skillTarget{})
			require.FileExists(t, tc.skillAt(ambient),
				"an unscoped session's guidance still belongs in the daemon's config root")
		})
	}
}

// An account af cannot resolve writes NOTHING, and does not fail the launch.
// Falling back would put the file in the operator's ambient home for a session
// that reads elsewhere — the defect, reached through an error path.
func TestInjectSystemPrompt_WritesNothingWhenTheAccountIsUnresolvable(t *testing.T) {
	agentHome(t)
	grantGlobalAgentSkills(t)
	ambient := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", ambient)

	program := injectSystemPrompt("gemini",
		resolveSkillTarget(&Instance{Program: "gemini", Account: "no-such-account"}, "gemini"))

	require.Equal(t, "gemini", program, "an unplaceable skill must not break the launch")
	require.NoFileExists(t, geminiSkillPathUnder(ambient),
		"af could not say where this session reads, so it must not write into the daemon's root instead")
	entries, err := os.ReadDir(ambient)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing at all may be created for an unresolvable account")
}
