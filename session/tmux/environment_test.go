package tmux

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandEnvironmentFromCommand(t *testing.T) {
	workDir := filepath.Join(string(filepath.Separator), "launch")
	tests := []struct {
		name       string
		command    string
		key        string
		want       CommandEnvOverride
		wantDir    string
		wantExe    string
		wantAgent  string
		unknownDir bool
		launch     string
		wantErr    string
	}{
		{name: "leading assignment", command: "CODEX_HOME='/tmp/codex home' codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/codex home", Present: true, Set: true, Literal: true}},
		{name: "env assignment", command: "/usr/bin/env CODEX_HOME=/tmp/one codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/one", Present: true, Set: true, Literal: true}},
		{name: "last assignment wins", command: "CODEX_HOME=/tmp/one CODEX_HOME=/tmp/two codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/two", Present: true, Set: true, Literal: true}},
		{name: "unset separate", command: "env -u CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "unset attached short", command: "env -uCODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "unset attached long", command: "env --unset=CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "clear then restore", command: "env -i HOME=/tmp/isolated codex", key: "HOME", want: CommandEnvOverride{Value: "/tmp/isolated", Present: true, Set: true, Literal: true}},
		{name: "clear inherited variable", command: "env -i HOME=/tmp/isolated codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "chdir separate", command: "env -C /tmp codex", key: "CODEX_HOME", wantDir: "/tmp"},
		{name: "chdir attached", command: "env -Crelative codex", key: "CODEX_HOME", wantDir: filepath.Join(workDir, "relative")},
		{name: "nested env cwd", command: "env -C /tmp env -C child CODEX_HOME=rel codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "rel", Present: true, Set: true, Literal: true}, wantDir: "/tmp/child"},
		{name: "dash terminates options before chdir", command: "env - -C /evilpath codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "dash terminates options models option-looking command", command: "env - -C /evilpath CODEX_HOME=mystore codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "codex chdir separate", command: "codex -C /tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex chdir attached", command: "codex -C/tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex long chdir separate", command: "codex --cd /tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex long chdir attached", command: "codex --cd=/tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex option delimiter", command: "codex -- -C", key: "CODEX_HOME"},
		{name: "inherited through wrapper", command: "ionice -c 3 codex", key: "CODEX_HOME", unknownDir: true},
		{name: "shell cd is unmodeled", command: "cd /tmp && codex", key: "CODEX_HOME", wantExe: "codex", wantAgent: ProgramCodex, unknownDir: true},
		{name: "opaque wrapper", command: "/opt/bin/my-agent-wrapper --ready", key: "CODEX_HOME", wantExe: "/opt/bin/my-agent-wrapper"},
		{name: "agent behind wrapper", command: "ionice -c 3 /opt/bin/codex", key: "CODEX_HOME", wantExe: "/opt/bin/codex", wantAgent: ProgramCodex, unknownDir: true},
		{name: "dynamic value refused", command: "CODEX_HOME=$ALT_CODEX_HOME codex", key: "CODEX_HOME", wantErr: "uses shell expansion"},
		{name: "dynamic chdir refused", command: "env -C $OTHER codex", key: "CODEX_HOME", wantErr: "unsupported env invocation"},
		{name: "unknown env option refused", command: "env --future-option codex", key: "CODEX_HOME", wantErr: "unknown option"},
		{name: "split string refused", command: "env -S CODEX_HOME=/tmp codex", key: "CODEX_HOME", wantErr: "split-string"},
		{name: "terminal env option refused", command: "env --help codex", key: "CODEX_HOME", wantErr: "without running a command"},
		{name: "relative launch directory refused", command: "codex", key: "CODEX_HOME", launch: "relative", wantErr: "launch directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			launchDir := workDir
			if tc.launch != "" {
				launchDir = tc.launch
			}
			got, err := CommandEnvironmentFromCommand(tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Override(tc.key))
			if tc.wantExe != "" {
				require.Equal(t, tc.wantExe, got.Executable)
				require.Equal(t, tc.wantAgent, got.Agent)
			}
			wantDir := tc.wantDir
			if wantDir == "" {
				wantDir = workDir
			}
			require.Equal(t, wantDir, got.WorkingDir)
			require.Equal(t, !tc.unknownDir, got.WorkingDirKnown())
		})
	}
}

func TestCodexHomeFromCommandUsesEffectiveCwd(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	got, err := CodexHomeFromCommand("env -C /tmp CODEX_HOME=relative codex", launchDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(string(filepath.Separator), "tmp", "relative"), got)
}

// ConfigRootFromCommand is the one answer to "where does this launch read its
// config" (#4501): Codex capture and af's skill placement both take it, so the
// two agents' shapes and the daemon fallback are pinned here once.
func TestConfigRootFromCommand(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	t.Setenv("HOME", "/daemon-home")
	t.Setenv("CODEX_HOME", "/daemon-codex")
	t.Setenv("GEMINI_CLI_HOME", "")
	tests := []struct {
		name, agent, command, want, wantErr string
	}{
		{name: "codex inherits the daemon variable", agent: ProgramCodex, command: "codex", want: "/daemon-codex"},
		{name: "codex command variable wins", agent: ProgramCodex, command: "CODEX_HOME=/srv/codex codex", want: "/srv/codex"},
		{name: "codex HOME fallback names the config dir", agent: ProgramCodex, command: "env -u CODEX_HOME HOME=/h codex", want: "/h/.codex"},
		{name: "gemini empty daemon variable falls back to HOME", agent: ProgramGemini, command: "gemini", want: "/daemon-home"},
		{name: "gemini command variable wins", agent: ProgramGemini, command: "GEMINI_CLI_HOME=/srv/gemini gemini", want: "/srv/gemini"},
		{name: "gemini root is HOME-like", agent: ProgramGemini, command: "HOME=/h gemini", want: "/h"},
		{name: "gemini empty command variable falls back to HOME", agent: ProgramGemini, command: "GEMINI_CLI_HOME= HOME=/h gemini", want: "/h"},
		{name: "gemini relative variable uses launch cwd", agent: ProgramGemini, command: "env -C sub GEMINI_CLI_HOME=rel gemini", want: "/launch/sub/rel"},
		{name: "gemini dynamic variable refused", agent: ProgramGemini, command: "GEMINI_CLI_HOME=$X gemini", wantErr: "uses shell expansion"},
		{name: "gemini dynamic HOME refused", agent: ProgramGemini, command: "HOME=$X gemini", wantErr: "uses shell expansion"},
		{name: "gemini parse failure names the agent", agent: ProgramGemini, command: "env --future-option gemini", wantErr: "cannot resolve Gemini environment"},
		{name: "relative variable behind a wrapper has no known cwd", agent: ProgramGemini, command: "ionice -c 3 env GEMINI_CLI_HOME=rel gemini", wantErr: "cannot tell which directory"},
		{name: "relative HOME behind a wrapper has no known cwd", agent: ProgramCodex, command: "ionice -c 3 env -u CODEX_HOME HOME=rel codex", wantErr: "cannot tell which directory"},
		{name: "absolute variable behind a wrapper still resolves", agent: ProgramGemini, command: "ionice -c 3 env GEMINI_CLI_HOME=/srv/g gemini", want: "/srv/g"},
		{name: "agent without a followed root", agent: ProgramClaude, command: "claude", wantErr: "does not follow a config root"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConfigRootFromCommand(tc.agent, tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.FromSlash(tc.want), got)
		})
	}
}

func TestCodexHomeFromCommandModelsDashClearThenAssignment(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	got, err := CodexHomeFromCommand("env - CODEX_HOME=mystore codex", launchDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(launchDir, "mystore"), got)
}

func TestCodexHomeFromCommandRefusesPhantomChdirAfterDash(t *testing.T) {
	prev := osAccountHome
	t.Cleanup(func() { osAccountHome = prev })
	osAccountHome = func() (string, error) { return "/os-home", nil }
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	got, err := CodexHomeFromCommand("env - -C /evilpath CODEX_HOME=mystore codex", launchDir)
	// The dash clears the environment and `-C /evilpath` is the command, not a
	// chdir, so the assignment after it never reaches codex: CODEX_HOME and HOME
	// are both absent and codex reads the OS account home (#5986). Neither the
	// phantom chdir nor the phantom assignment may leak into the answer.
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/os-home/.codex"), got)
	require.NotContains(t, got, "evilpath")
	require.NotContains(t, got, "mystore")
}

// The OS-home fallback (#5986): with HOME and the agent's variable both absent
// from the launch, the agent reads the account's passwd home, so af resolves the
// same directory instead of reporting the root unknowable.
func TestConfigRootFromCommandFallsBackToTheOSAccountHome(t *testing.T) {
	prev := osAccountHome
	t.Cleanup(func() { osAccountHome = prev })
	osAccountHome = func() (string, error) { return "/os-home", nil }
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	t.Setenv("HOME", "/daemon-home")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GEMINI_CLI_HOME", "")

	tests := []struct{ name, agent, command, want, wantErr string }{
		{name: "gemini env -i", agent: ProgramGemini, command: "env -i gemini", want: "/os-home"},
		{name: "gemini env -u HOME", agent: ProgramGemini, command: "env -u HOME gemini", want: "/os-home"},
		{name: "codex env -i", agent: ProgramCodex, command: "env -i codex", want: "/os-home/.codex"},
		{name: "codex empty HOME falls back, as Rust home_dir does", agent: ProgramCodex, command: "env HOME= codex", want: "/os-home/.codex"},
		{name: "gemini empty HOME is not a home af can name", agent: ProgramGemini, command: "env HOME= gemini", wantErr: "HOME is empty"},
		{name: "an explicit HOME still wins", agent: ProgramGemini, command: "env -i HOME=/h gemini", want: "/h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConfigRootFromCommand(tc.agent, tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.FromSlash(tc.want), got)
		})
	}

	t.Run("a failed account lookup is an error, never a guess", func(t *testing.T) {
		osAccountHome = func() (string, error) { return "", errors.New("no passwd entry") }
		got, err := ConfigRootFromCommand(ProgramGemini, "env -i gemini", launchDir)
		require.ErrorContains(t, err, "could not look up the account home")
		require.Empty(t, got)
	})
}

// #6004: ConfigRootFromCommand answers "where does the agent read" as the agent
// resolves it. Each row is one divergence the review found between the modeled
// launch and the real one.
func TestConfigRootFromCommandFollowsTheAgentsOwnResolution(t *testing.T) {
	prev := osAccountHome
	t.Cleanup(func() { osAccountHome = prev })
	osAccountHome = func() (string, error) { return "/os-home", nil }
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	t.Setenv("HOME", "/daemon-home")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GEMINI_CLI_HOME", "")

	tests := []struct{ name, agent, command, want, wantErr string }{
		// codex -C/--cd moves the working root AFTER Codex resolves its home, so
		// the launch cwd af knows still anchors a relative root.
		{name: "codex -C keeps a relative CODEX_HOME at the launch cwd", agent: ProgramCodex, command: "CODEX_HOME=rel codex -C nested", want: "/launch/rel"},
		{name: "codex --cd= keeps it too", agent: ProgramCodex, command: "CODEX_HOME=rel codex --cd=nested", want: "/launch/rel"},
		{name: "codex -Cdir attached keeps it too", agent: ProgramCodex, command: "CODEX_HOME=rel codex -Cnested", want: "/launch/rel"},
		{name: "codex -C keeps a relative HOME at the launch cwd", agent: ProgramCodex, command: "env -u CODEX_HOME HOME=h codex -C nested", want: "/launch/h/.codex"},
		{name: "codex -C behind env -C still anchors at env's directory", agent: ProgramCodex, command: "env -C sub CODEX_HOME=rel codex -C nested", want: "/launch/sub/rel"},
		{name: "a wrapper that may chdir stays unknowable even with -C", agent: ProgramCodex, command: "ionice -c 3 env CODEX_HOME=rel codex -C nested", wantErr: "cannot tell which directory"},
		{name: "absolute root behind a wrapper with -C still resolves", agent: ProgramCodex, command: "ionice -c 3 env CODEX_HOME=/srv/c codex -C nested", want: "/srv/c"},
		// A whitespace-only HOME is a nonempty value: Rust's home_dir keeps it, so
		// the root is that directory under the launch cwd, not the account home.
		{name: "whitespace-only HOME is a literal directory name", agent: ProgramCodex, command: "env -u CODEX_HOME HOME='   ' codex", want: "/launch/   /.codex"},
		{name: "whitespace-only HOME is a literal directory name for gemini", agent: ProgramGemini, command: "env HOME='   ' gemini", want: "/launch/   "},
		{name: "whitespace-only agent variable is a literal path too", agent: ProgramCodex, command: "CODEX_HOME='  ' codex", want: "/launch/  "},
		{name: "empty HOME still falls back for codex", agent: ProgramCodex, command: "env -u CODEX_HOME HOME= codex", want: "/os-home/.codex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConfigRootFromCommand(tc.agent, tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.FromSlash(tc.want), got)
		})
	}
}

// Receipt correlation still treats codex -C as an unknown working directory;
// only config-root resolution may rely on the pre-exec directory.
func TestCommandEnvironmentSeparatesTheLaunchDirFromTheFinalWorkingDir(t *testing.T) {
	for _, tc := range []struct {
		command         string
		workingDirKnown bool
		launchDirKnown  bool
	}{
		{"codex", true, true},
		{"codex -C /tmp", false, true},
		{"codex --cd=/tmp", false, true},
		{"ionice -c 3 codex", false, false},
		{"ionice -c 3 codex -C /tmp", false, false},
	} {
		got, err := CommandEnvironmentFromCommand(tc.command, "/launch")
		require.NoError(t, err, tc.command)
		require.Equal(t, tc.workingDirKnown, got.WorkingDirKnown(), tc.command)
		require.Equal(t, tc.launchDirKnown, got.LaunchDirKnown(), tc.command)
	}
}

// The account-home lookup must come from the account database alone (#6004). On
// a CGO-off build user.Current() would synthesize a user from the daemon's HOME
// when the uid has no passwd entry; user.LookupId does not.
func TestOSAccountHomeComesFromTheAccountDatabase(t *testing.T) {
	t.Setenv("HOME", "/not-the-account-home")
	t.Setenv("USER", "someone-else")
	want, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		got, gerr := osAccountHome()
		require.Error(t, gerr, "a uid with no passwd entry must be unknowable, got %q", got)
		return
	}
	got, err := osAccountHome()
	require.NoError(t, err)
	require.Equal(t, want.HomeDir, got)
	require.NotEqual(t, "/not-the-account-home", got)
}
