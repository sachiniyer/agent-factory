package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #4501: an unscoped launch's af skill belongs in the config root the LAUNCHED
// COMMAND reads, which is not always the daemon's. `program_overrides.codex =
// "CODEX_HOME=/srv/codex codex"` reads /srv/codex, and conversation capture has
// followed the command there since #2228 — while the skill kept landing under the
// daemon's own CODEX_HOME, where this launch never looks. These tests pin the two
// derivations to one answer through the real launch plans, so they cannot drift
// apart again from either side.

// launchRootFixture is one unscoped launch of agent through command.
type launchRootFixture struct {
	agent   string // the configured agent the command is installed under
	stub    string // absolute path of a stand-in agent executable
	home    string // the daemon's HOME
	ambient string // the daemon's own agent config variable, when set
	root    string // a literal root for the command to name
	work    string // the session worktree, which is the launch directory
}

// newLaunchRootFixture sandboxes HOME and the agent's config variable, and
// returns the directories the command templates below refer to. daemonVar says
// whether the DAEMON's own variable is set; when false it is unset outright, not
// merely empty.
func newLaunchRootFixture(t *testing.T, agent, variable string, daemonVar bool) launchRootFixture {
	t.Helper()
	f := launchRootFixture{
		agent:   agent,
		home:    agentHome(t),
		ambient: t.TempDir(),
		root:    filepath.Join(t.TempDir(), "command-root"),
		work:    t.TempDir(),
	}
	t.Setenv(variable, f.ambient)
	if !daemonVar {
		require.NoError(t, os.Unsetenv(variable)) // t.Setenv restores it
	}
	// `env -C sub` must name a directory that exists for handoff preflight.
	require.NoError(t, os.MkdirAll(filepath.Join(f.work, "sub"), 0o755))
	bin := t.TempDir()
	f.stub = filepath.Join(bin, agent)
	require.NoError(t, os.WriteFile(f.stub, []byte("#!/bin/sh\n"), 0o755))
	return f
}

// expand fills a command or path template.
func (f launchRootFixture) expand(template string) string {
	return strings.NewReplacer(
		"{agent}", f.stub,
		"{ambient}", f.ambient,
		"{home}", f.home,
		"{root}", f.root,
		"{work}", f.work,
	).Replace(template)
}

// saveLaunchConfig gives this test a fresh af home whose config routes the
// fixture's agent through command and grants or declines global_agent_skills.
func (f launchRootFixture) saveLaunchConfig(t *testing.T, granted bool, command string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.GlobalAgentSkills = granted
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = make(map[string]string)
	}
	cfg.ProgramOverrides[f.agent] = f.expand(command)
	require.NoError(t, config.SaveConfig(cfg))
}

// launchPath is one production seam that places the skill AND freezes a launch.
type launchPath struct {
	name string
	// plan runs the seam for the fixture's agent and returns the Codex capture
	// root it froze ("" for an agent without capture) and the seam's error.
	plan func(t *testing.T, f launchRootFixture) (string, error)
}

var launchRootPaths = []launchPath{
	{"create", func(t *testing.T, f launchRootFixture) (string, error) {
		inst := launchRootInstance(t, f, f.agent)
		plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)
		return plan.conversationCapture.codexHome, err
	}},
	{"handoff", func(t *testing.T, f launchRootFixture) (string, error) {
		inst := launchRootInstance(t, f, tmux.ProgramClaude)
		plan, err := (&LocalBackend{}).PrepareAgentSwap(inst, f.agent)
		return plan.conversationCapture.codexHome, err
	}},
}

func launchRootInstance(t *testing.T, f launchRootFixture, program string) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "skill-root", Path: f.work, Program: program})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(f.work, f.work, inst.Title, "main", "", true, false)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// launchRootCase is one command form and the config root it must resolve to.
type launchRootCase struct {
	name      string
	command   string
	daemonVar bool
	want      string
}

// daemonRoot is where the pre-#4501 code wrote: the daemon's variable, else HOME.
func (c launchRootCase) daemonRoot(f launchRootFixture, homeSubdir string) string {
	if c.daemonVar {
		return f.ambient
	}
	return filepath.Join(f.home, homeSubdir)
}

func TestLaunchPlans_CodexSkillRootIsTheCaptureRoot(t *testing.T) {
	cases := []launchRootCase{
		{"VAR prefix", "CODEX_HOME={root} {agent}", true, "{root}"},
		{"env VAR", "env CODEX_HOME={root} {agent}", true, "{root}"},
		{"env -u VAR", "env -u CODEX_HOME {agent}", true, "{home}/.codex"},
		{"HOME prefix", "HOME={root} {agent}", false, "{root}/.codex"},
		// CODEX_HOME outranks HOME, in the daemon's environment as in the command's.
		{"HOME prefix under a daemon CODEX_HOME", "HOME={root} {agent}", true, "{ambient}"},
		{"env -u VAR with HOME", "env -u CODEX_HOME HOME={root} {agent}", true, "{root}/.codex"},
		{"env -i with HOME", "env -i HOME={root} {agent}", true, "{root}/.codex"},
		{"relative VAR", "CODEX_HOME=rel {agent}", true, "{work}/rel"},
		{"env -C with a relative VAR", "env -C sub CODEX_HOME=rel {agent}", true, "{work}/sub/rel"},
		{"bare command", "{agent}", true, "{ambient}"},
		{"bare command without a daemon CODEX_HOME", "{agent}", false, "{home}/.codex"},
	}
	for _, path := range launchRootPaths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				f := newLaunchRootFixture(t, tmux.ProgramCodex, "CODEX_HOME", tc.daemonVar)
				f.saveLaunchConfig(t, true, tc.command)
				want := filepath.Clean(f.expand(tc.want))

				captureRoot, err := path.plan(t, f)
				require.NoError(t, err)
				require.Equal(t, want, captureRoot,
					"the capture root is the reference this test pins the skill to")
				require.FileExists(t, codexSkillPathUnder(captureRoot),
					"the af skill must be where this launch reads its config: the root capture watches")
				if daemon := tc.daemonRoot(f, ".codex"); daemon != want {
					require.NoFileExists(t, codexSkillPathUnder(daemon),
						"the daemon's own root is not where this launch reads")
				}
			})
		}
	}
}

func TestLaunchPlans_GeminiSkillRootFollowsTheCommand(t *testing.T) {
	// GEMINI_CLI_HOME is a HOME-like root, so every expected value is the root the
	// CLI appends .gemini/ to (#3387).
	cases := []launchRootCase{
		{"VAR prefix", "GEMINI_CLI_HOME={root} {agent}", true, "{root}"},
		{"env VAR", "env GEMINI_CLI_HOME={root} {agent}", true, "{root}"},
		{"env -u VAR", "env -u GEMINI_CLI_HOME {agent}", true, "{home}"},
		{"HOME prefix", "HOME={root} {agent}", false, "{root}"},
		{"HOME prefix under a daemon GEMINI_CLI_HOME", "HOME={root} {agent}", true, "{ambient}"},
		{"env -u VAR with HOME", "env -u GEMINI_CLI_HOME HOME={root} {agent}", true, "{root}"},
		{"relative VAR", "GEMINI_CLI_HOME=rel {agent}", true, "{work}/rel"},
		{"env -C with a relative VAR", "env -C sub GEMINI_CLI_HOME=rel {agent}", true, "{work}/sub/rel"},
		{"bare command", "{agent}", true, "{ambient}"},
		{"bare command without a daemon GEMINI_CLI_HOME", "{agent}", false, "{home}"},
	}
	for _, path := range launchRootPaths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				f := newLaunchRootFixture(t, tmux.ProgramGemini, "GEMINI_CLI_HOME", tc.daemonVar)
				f.saveLaunchConfig(t, true, tc.command)
				want := filepath.Clean(f.expand(tc.want))

				_, err := path.plan(t, f)
				require.NoError(t, err)
				require.FileExists(t, geminiSkillPathUnder(want),
					"the af skill must be where this launch reads its config")
				if daemon := tc.daemonRoot(f, ""); daemon != want {
					require.NoFileExists(t, geminiSkillPathUnder(daemon),
						"the daemon's own root is not where this launch reads")
				}
			})
		}
	}
}

// A command that sets the variable to something af cannot read leaves af unable
// to say where the agent will look. That is the account path's "unresolved"
// answer, and it has the same consequence: write nothing, anywhere — not the
// daemon's root, which is exactly the directory the command moved away from.
func TestCreateLaunch_NonLiteralConfigRootWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, agent, variable, homeSubdir, command string
		daemonVar                                  bool
	}{
		{"codex dynamic VAR", tmux.ProgramCodex, "CODEX_HOME", ".codex", "CODEX_HOME=$AF_TEST_ROOT {agent}", true},
		{"codex dynamic HOME", tmux.ProgramCodex, "CODEX_HOME", ".codex", "HOME=$AF_TEST_ROOT {agent}", false},
		{"gemini dynamic VAR", tmux.ProgramGemini, "GEMINI_CLI_HOME", "", "GEMINI_CLI_HOME=$AF_TEST_ROOT {agent}", true},
		{"gemini dynamic HOME", tmux.ProgramGemini, "GEMINI_CLI_HOME", "", "HOME=$AF_TEST_ROOT {agent}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchRootFixture(t, tc.agent, tc.variable, tc.daemonVar)
			t.Setenv("AF_TEST_ROOT", f.root)
			f.saveLaunchConfig(t, true, tc.command)

			inst := launchRootInstance(t, f, tc.agent)
			plan, err := (&LocalBackend{}).prepareCreateLaunch(inst)
			if tc.agent == tmux.ProgramCodex {
				// Capture already refuses this launch; the skill must not have been
				// placed on the way there.
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, f.expand(tc.command), plan.program,
					"an unplaceable skill must not rewrite or break the launch")
			}

			daemon := launchRootCase{daemonVar: tc.daemonVar}.daemonRoot(f, tc.homeSubdir)
			for _, root := range []string{daemon, f.root, f.home, f.ambient} {
				require.NoFileExists(t, filepath.Join(root, "skills", afSkillDirName, "SKILL.md"))
				require.NoFileExists(t, filepath.Join(root, tc.homeSubdir, "skills", afSkillDirName, "SKILL.md"))
				require.NoFileExists(t, geminiSkillPathUnder(root))
			}
		})
	}
}

// #1977's promise, on the command's root: once the operator declines, af removes
// the skill it placed. Cleaning only the daemon's root would orphan the copy a
// granted launch wrote where the command reads — permanently, since nothing else
// would ever visit that directory for af. The daemon's root is still cleaned: it
// is where every af before #4501 put this launch's skill.
func TestCreateLaunch_DeclinedCleansTheCommandRootAndTheDaemonRoot(t *testing.T) {
	for _, tc := range []struct {
		agent, variable, command string
		skillAt                  func(root string) string
	}{
		{tmux.ProgramCodex, "CODEX_HOME", "CODEX_HOME={root} {agent}", codexSkillPathUnder},
		{tmux.ProgramGemini, "GEMINI_CLI_HOME", "GEMINI_CLI_HOME={root} {agent}", geminiSkillPathUnder},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			f := newLaunchRootFixture(t, tc.agent, tc.variable, true)
			for _, root := range []string{f.root, f.ambient} {
				path := tc.skillAt(root)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(afSkillDoc), 0o644))
			}

			f.saveLaunchConfig(t, false, tc.command)
			_, err := (&LocalBackend{}).prepareCreateLaunch(launchRootInstance(t, f, tc.agent))
			require.NoError(t, err)

			require.NoFileExists(t, tc.skillAt(f.root),
				"the skill af placed where the command reads is af's, and the operator declined")
			require.NoFileExists(t, tc.skillAt(f.ambient),
				"the copy an earlier af left in the daemon's root is af's too")
		})
	}
}
