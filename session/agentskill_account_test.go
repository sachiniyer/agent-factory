package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/git"
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

// skillTestInstance is a session with a launch directory. An unscoped launch
// resolves its config root from its command in that directory, exactly as
// conversation capture does, so a session without one places nothing.
func skillTestInstance(t *testing.T, program, account string) *Instance {
	t.Helper()
	work := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(work, work, "skill-test", "main", "", true, false)
	require.NoError(t, err)
	return &Instance{Program: program, Account: account, gitWorktree: gw}
}

// ambientSkillTarget is the target of a bare, unscoped launch of agent — the
// daemon's own config root — resolved by the production resolver. Call it after
// the test has set HOME and the agent's variable.
func ambientSkillTarget(t *testing.T, agent string) skillTarget {
	t.Helper()
	target := resolveSkillTarget(skillTestInstance(t, agent, ""), agent)
	require.NotEmpty(t, target.root, "a bare launch must resolve: %v", target.why)
	return target
}

// skillPathUnder is where af writes its guidance for each agent, given that
// agent's CONFIG ROOT — the value the account boundary installs for a scoped
// session, and the one the launch command resolves to for an unscoped one.
func geminiSkillPathUnder(root string) string {
	return filepath.Join(root, ".gemini", "skills", afSkillDirName, "SKILL.md")
}

func codexSkillPathUnder(root string) string {
	return filepath.Join(root, "skills", afSkillDirName, "SKILL.md")
}

// resolveSkillTarget answers several different questions, and only a resolved
// root places anything (#3645, #4501).
func TestResolveSkillTarget_DistinguishesUnscopedFromUnresolvable(t *testing.T) {
	home := agentHome(t)
	t.Setenv("GEMINI_CLI_HOME", "")
	grantGlobalAgentSkills(t)
	dir := registerAccount(t, "gemini", "work")

	unscoped := resolveSkillTarget(skillTestInstance(t, "gemini", ""), "gemini")
	require.Equal(t, skillTarget{root: home}, unscoped,
		"a session with no account reads the root its command resolves to — for a bare command, the daemon's")

	scoped := resolveSkillTarget(skillTestInstance(t, "gemini", "work"), "gemini")
	require.Equal(t, skillTarget{root: dir, legacy: home}, scoped,
		"a scoped session's skill belongs under the directory the boundary will install, and the daemon's "+
			"root is only a place an older af may have left one")

	// UNRESOLVABLE, not unscoped. Falling back to the daemon's root here is the
	// defect: it writes into a directory the operator did not select, for a session
	// that reads somewhere else.
	missing := resolveSkillTarget(skillTestInstance(t, "gemini", "no-such-account"), "gemini")
	require.Empty(t, missing.root, "an account af cannot resolve must not fall back to the daemon's root")
	require.Empty(t, missing.legacy, "nor clean anything on its behalf")
	require.ErrorIs(t, missing.unplaceable(), errUnresolvedSkillRoot)

	// An account named for an agent that cannot be scoped at all. The launch
	// refuses this; until it does, af must not guess a directory.
	unsupported := resolveSkillTarget(skillTestInstance(t, "amp", "work"), "amp")
	require.Empty(t, unsupported.root)
	require.ErrorIs(t, unsupported.unplaceable(), errUnresolvedSkillRoot)

	// A FOREIGN NAMESPACE. The name was validated against the session's own agent,
	// and account namespaces are separate — "work" means a different identity for
	// each agent. Resolving it against the resolved command's registry would write
	// into a gemini account the operator never selected, moments before the launch
	// refuses for exactly that reason (#3082/#3108, #3645 review).
	foreign := resolveSkillTarget(skillTestInstance(t, "claude", "work"), "gemini")
	require.Empty(t, foreign.root, "a claude account name must not be resolved against gemini's registry")
	require.ErrorIs(t, foreign.unplaceable(), errUnresolvedSkillRoot)

	// NO LAUNCH DIRECTORY. An unscoped command's root can depend on the directory
	// it starts in, so a session without one cannot say where it reads (#4501).
	rootless := resolveSkillTarget(&Instance{Program: "gemini"}, "gemini")
	require.Empty(t, rootless.root, "an unscoped session with no launch directory must not guess one")
	require.ErrorIs(t, rootless.unplaceable(), errUnresolvedSkillRoot)

	// THE ZERO VALUE places nothing. There is no target meaning "use the daemon's
	// root", so a caller that skips resolution fails closed.
	require.ErrorIs(t, skillTarget{}.unplaceable(), errUnresolvedSkillRoot)
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

	scoped := skillTestInstance(t, "claude", "work")
	target := resolveSkillTarget(scoped, "gemini")
	require.Empty(t, target.root, "the handoff target's account namespace is not this session's")

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
			target := resolveSkillTarget(skillTestInstance(t, "gemini", ""), "gemini")
			require.Empty(t, target.root,
				"the container cannot know whether the host opted in, so it must neither write nor clean")
			require.Empty(t, target.legacy)
			require.ErrorIs(t, target.unplaceable(), errMountedAccountRoot)
		})
	}

	// A SIBLING with the same prefix is not the mount. Compared as path segments,
	// never as a string prefix.
	t.Setenv("GEMINI_CLI_HOME", dockerAccountHome+"-backup")
	require.Equal(t, skillTarget{root: dockerAccountHome + "-backup"},
		resolveSkillTarget(skillTestInstance(t, "gemini", ""), "gemini"),
		"a sibling directory that merely shares the prefix is an ordinary ambient root")

	// The COMMAND can name the mount too, and the resolved root is what the launch
	// reads, so the same refusal applies to it (#4501).
	t.Setenv("GEMINI_CLI_HOME", "")
	named := resolveSkillTarget(skillTestInstance(t, "gemini", ""), "GEMINI_CLI_HOME="+dockerAccountHome+" gemini")
	require.Empty(t, named.root, "a command pointed at the mount must neither write nor clean there")
	require.ErrorIs(t, named.unplaceable(), errMountedAccountRoot)

	// And a daemon root inside the mount is never cleaned on behalf of a launch
	// that reads elsewhere: that directory is still the host's.
	t.Setenv("HOME", dockerAccountHome+"/home")
	elsewhere := t.TempDir()
	require.Equal(t, skillTarget{root: elsewhere},
		resolveSkillTarget(skillTestInstance(t, "gemini", ""), "GEMINI_CLI_HOME="+elsewhere+" gemini"),
		"the daemon's root is under the mount, so it must not become a cleanup target")
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
	scoped := resolveSkillTarget(skillTestInstance(t, "gemini", "work"), "gemini")
	require.Equal(t, skillTarget{root: dir, legacy: ambient}, scoped)

	// A prior af version wrote into the ambient root; this one writes into the
	// account root. Both af-marked, both af's to clean.
	_, err := ensureGeminiSkillDir(ambientSkillTarget(t, "gemini"))
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
func TestSkillsBase_IsAFunctionOfTheConfigRootValue(t *testing.T) {
	const account = "/afhome/accounts/x/work"
	require.Equal(t, filepath.Join(account, ".gemini", "skills"), geminiSkillsBase(account),
		"GEMINI_CLI_HOME is a HOME-like root, so the account directory gains the .gemini/ level")
	require.Equal(t, filepath.Join(account, "skills"), codexSkillsBase(account),
		"CODEX_HOME names the config directory itself, so the account directory takes its place directly")

	ambient := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", ambient)
	t.Setenv("CODEX_HOME", ambient)
	require.Equal(t, skillTarget{root: ambient}, ambientSkillTarget(t, "gemini"),
		"a bare unscoped launch reads the daemon's environment")
	require.Equal(t, skillTarget{root: ambient}, ambientSkillTarget(t, "codex"))
}

// A target with no root writes nothing and cleans nothing, whatever the consent.
func TestEnsureSkillDir_UnresolvedTargetTouchesNothing(t *testing.T) {
	for name, granted := range map[string]bool{"granted": true, "declined": false} {
		t.Run(name, func(t *testing.T) {
			home := agentHome(t)
			t.Setenv("GEMINI_CLI_HOME", "")
			t.Setenv("CODEX_HOME", "")
			writeAfConfig(t, granted)
			seeded := []string{geminiSkillPathUnder(home), codexSkillPathUnder(filepath.Join(home, ".codex"))}
			for _, path := range seeded {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(afSkillDoc), 0o644))
			}

			for _, ensure := range []func(skillTarget) (string, error){ensureGeminiSkillDir, ensureCodexSkillDir} {
				dir, err := ensure(skillTarget{})
				require.ErrorIs(t, err, errUnresolvedSkillRoot)
				require.Empty(t, dir, "an unresolved target must yield no skill directory")
			}
			for _, path := range seeded {
				require.FileExists(t, path, "an unresolved target must clean nothing either")
			}
		})
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
			injectSystemPrompt(tc.agent, ambientSkillTarget(t, tc.agent))
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
