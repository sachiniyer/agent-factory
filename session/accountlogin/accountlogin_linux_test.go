//go:build linux

package accountlogin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The sentinel values staged into the daemon's own environment before a login
// pane is spawned. They are recognizable strings rather than plausible ones so
// an assertion can look for the VALUE anywhere in the pane's environment, not
// only under the names it expected to check.
const (
	ambientClaudeKey = "sk-ant-AMBIENT-MUST-NOT-REACH-LOGIN"
	ambientOpenAIKey = "sk-openai-AMBIENT-MUST-NOT-REACH-LOGIN"
	ambientGeminiKey = "gem-AMBIENT-MUST-NOT-REACH-LOGIN"
	// The daemon's own BROWSER/NO_BROWSER. af decides a login pane's browser
	// behaviour per agent, so neither this value nor the name it arrived under may
	// survive into the pane (#3854).
	ambientBrowser = "AMBIENT-BROWSER-MUST-NOT-REACH-LOGIN"
)

// loginPaneCase is one agent's end-to-end login pane: the invocation af must
// run, the variable it must point at the account, the identity names it must
// remove, and the artifact the agent's own flow writes.
type loginPaneCase struct {
	agent string
	// argv is what the pane process must receive AFTER its own name — the
	// agent's own login words and nothing af invented.
	argv string
	// configVar relocates this agent's credential root.
	configVar string
	// credentialNames must be absent from the pane's environment: for all of
	// these CLIs an ambient key outranks the config directory, so a login that
	// ran with one still set would authenticate as whoever that key belongs to
	// while the account directory stayed empty (#2983).
	credentialNames []string
	// artifact is where this agent writes its credential, relative to the
	// account directory.
	artifact string
	// loginEnv is what makes THIS agent's sign-in browser-free (#3854), asserted
	// as NAME=VALUE on the running pane. codex's lever is a flag rather than a
	// variable, so its map is empty.
	loginEnv map[string]string
	// absentEnv are the browser levers this agent must NOT carry — the other
	// agent's, and the ambient copies staged below. A working session gets
	// neither, and a login pane gets only its own.
	absentEnv []string
}

func loginPaneCases() []loginPaneCase {
	return []loginPaneCase{
		{
			agent:           "claude",
			argv:            "auth login",
			configVar:       "CLAUDE_CONFIG_DIR",
			credentialNames: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
			artifact:        ".credentials.json",
			// claude 2.1.261 has no browser-free flag; its URL opener reads BROWSER
			// and spawns it with the URL, so the no-op is the lever.
			loginEnv:  map[string]string{"BROWSER": "true"},
			absentEnv: []string{"NO_BROWSER"},
		},
		{
			agent:     "codex",
			argv:      "login --device-auth",
			configVar: "CODEX_HOME",
			// The flag IS the lever, so the pane needs nothing from the environment
			// and must not carry the other agents' levers either.
			credentialNames: []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
			artifact:        "auth.json",
			absentEnv:       []string{"NO_BROWSER", "BROWSER"},
		},
		{
			agent:           "gemini",
			argv:            "",
			configVar:       "GEMINI_CLI_HOME",
			credentialNames: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"},
			artifact:        filepath.Join(".gemini", "gemini-credentials.json"),
			loginEnv:        map[string]string{"NO_BROWSER": "true"},
			absentEnv:       []string{"BROWSER"},
		},
	}
}

// TestLoginPaneRunsTheAgentsOwnFlowInTheAccountEnvironment is the whole of
// #3384 asserted where it is decidable: in the REAL pane process's own
// /proc/<pid>/environ, on a real tmux server, through the production launch
// chain (Start → tmux → the internal exec shim → the agent-named program).
//
// It is deliberately not a check that af composed the right strings. af composed
// the right strings before this issue too — it printed them. What was never
// established is that the process which ends up holding the terminal receives
// exactly the account's credential root and NONE of the ambient identity, which
// is the property that makes the login land in the account the operator asked
// for rather than silently re-authenticating the machine's own identity. The
// method is the one #3388's verification used: a staged ambient identity with
// sentinel values, and evidence read from the agent process rather than from
// af's own configuration.
func TestLoginPaneRunsTheAgentsOwnFlowInTheAccountEnvironment(t *testing.T) {
	for _, tc := range loginPaneCases() {
		t.Run(tc.agent, func(t *testing.T) {
			testguard.IsolateTmux(t)
			home := testguard.SocketTempDir(t)
			t.Setenv("AGENT_FACTORY_HOME", home)

			// A staged ambient identity, including a credential root pointing
			// somewhere else entirely. Without the subtraction the pane would
			// authenticate against THESE.
			ambientRoot := filepath.Join(home, "ambient-"+tc.agent)
			if err := os.MkdirAll(ambientRoot, 0o700); err != nil {
				t.Fatalf("stage ambient credential root: %v", err)
			}
			t.Setenv(tc.configVar, ambientRoot)
			t.Setenv("ANTHROPIC_API_KEY", ambientClaudeKey)
			t.Setenv("OPENAI_API_KEY", ambientOpenAIKey)
			t.Setenv("GEMINI_API_KEY", ambientGeminiKey)
			// And the daemon's own browser configuration, which must not decide how
			// a login pane behaves: af sets these itself, per agent (#3854). Values
			// af would never write, so an assertion below can tell "af's" from
			// "inherited".
			t.Setenv("BROWSER", ambientBrowser)
			t.Setenv("NO_BROWSER", ambientBrowser)

			binDir := t.TempDir()
			reportPath := filepath.Join(binDir, "pane-environment")
			writeLoginAgentFixture(t, binDir, tc, reportPath)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			supervisor := New()
			t.Cleanup(supervisor.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			login, err := supervisor.Start(ctx, Request{Home: home, Agent: tc.agent, Name: "work"})
			if err != nil {
				t.Fatalf("start login pane: %v", err)
			}
			if login.Reused {
				t.Fatal("a first login reported that it reused a pane")
			}
			if login.LoggedIn {
				t.Fatal("a never-logged-in account reported logged in at spawn")
			}

			accountDir, err := agentaccount.Dir(home, tc.agent, "work")
			if err != nil {
				t.Fatalf("resolve account directory: %v", err)
			}
			if login.Dir != accountDir {
				t.Fatalf("login reported directory %q, want the registered account %q", login.Dir, accountDir)
			}

			report := waitForReport(t, reportPath)

			// 1. The pane ran the AGENT'S OWN login invocation, with no words af
			//    invented — the #3057 refusal, asserted on the running process.
			wantArgv := "argv=[" + tc.argv + "]"
			if !strings.Contains(report, wantArgv+"\n") {
				t.Fatalf("pane argv is not the agent's own login command; want %q in:\n%s", wantArgv, report)
			}

			// 2. INJECTION: the credential root the pane holds is the account's.
			if !strings.Contains(report, tc.configVar+"="+accountDir+"\n") {
				t.Fatalf("pane did not receive %s=%s; environment:\n%s", tc.configVar, accountDir, report)
			}
			if strings.Contains(report, tc.configVar+"="+ambientRoot+"\n") {
				t.Fatalf("pane kept the ambient %s; environment:\n%s", tc.configVar, report)
			}

			// 3. SUBTRACTION: no identity that would outrank that directory.
			for _, name := range tc.credentialNames {
				for _, line := range strings.Split(report, "\n") {
					if strings.HasPrefix(line, name+"=") && strings.TrimPrefix(line, name+"=") != "" {
						t.Fatalf("pane kept the ambient identity %q; environment:\n%s", line, report)
					}
				}
			}
			for _, sentinel := range []string{ambientClaudeKey, ambientOpenAIKey, ambientGeminiKey} {
				if strings.Contains(report, sentinel) {
					t.Fatalf("pane environment carries the ambient sentinel %q under some name; environment:\n%s",
						sentinel, report)
				}
			}

			// 4. BROWSER-FREE (#3854): the pane holds exactly this agent's lever,
			//    with af's value rather than the daemon's, and none of the others.
			//    Read off the real process, because the whole chain — the tmux
			//    session environment, the exec shim's default-deny re-filter, the
			//    account boundary — sits between af's table and this pane, and any
			//    of them could drop it.
			for name, want := range tc.loginEnv {
				if !strings.Contains(report, name+"="+want+"\n") {
					t.Fatalf("pane did not receive %s=%s, so the sign-in is not browser-free; environment:\n%s",
						name, want, report)
				}
			}
			for _, name := range tc.absentEnv {
				for _, line := range strings.Split(report, "\n") {
					if strings.HasPrefix(line, name+"=") {
						t.Fatalf("pane carries %q, which is not this agent's browser lever; environment:\n%s",
							line, report)
					}
				}
			}
			if strings.Contains(report, ambientBrowser) {
				t.Fatalf("pane inherited the daemon's own browser configuration %q; environment:\n%s",
					ambientBrowser, report)
			}

			// 5. The login is reported from the AGENT'S OWN artifact. The fixture
			//    wrote it through the variable af injected, so this also proves the
			//    directory the pane holds is the one af reports.
			loggedIn := waitForLogin(t, home, tc.agent, "work")
			if !loggedIn {
				t.Fatalf("account did not report logged in after the flow wrote %s", tc.artifact)
			}
		})
	}
}

// TestLoginPaneIsReusedRatherThanDuplicated covers the second `af accounts
// login` for an account whose flow is still open. Two concurrent logins race
// over one auth.json, and the second pane would also be invisible to the
// operator sitting in the first.
func TestLoginPaneIsReusedRatherThanDuplicated(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	binDir := t.TempDir()
	// A flow that never finishes, which is what an OAuth prompt awaiting a human
	// looks like.
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	ctx := context.Background()
	first, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("start first login pane: %v", err)
	}
	second, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("start second login for the same account: %v", err)
	}
	if !second.Reused {
		t.Fatal("a second login for an account whose flow is open started a competing pane")
	}
	if second.TmuxName != first.TmuxName {
		t.Fatalf("second login named %q, want the live pane %q", second.TmuxName, first.TmuxName)
	}

	// A DIFFERENT account of the same agent is a different flow and must get its
	// own pane.
	other, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "personal"})
	if err != nil {
		t.Fatalf("start login for a second account: %v", err)
	}
	if other.Reused || other.TmuxName == first.TmuxName {
		t.Fatalf("a second account reused %q", first.TmuxName)
	}
}

// TestLoginRefusesAgentsWithNoVerifiedFlow keeps the roster refusal at the verb,
// not only in the table: an agent af cannot log in must never reach tmux.
func TestLoginRefusesAgentsWithNoVerifiedFlow(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	supervisor := New()
	t.Cleanup(supervisor.Stop)
	for _, agent := range []string{"amp", "aider", "opencode", "devin"} {
		if _, err := supervisor.Start(context.Background(), Request{Home: home, Agent: agent, Name: "work"}); err == nil {
			t.Fatalf("login for %q was accepted", agent)
		}
	}
}

// TestLoginRefusesAKeyringBackedCodexAccountBeforeSpawning is the precondition
// that must be enforced where it is detectable. A login whose result codex would
// ignore must not be run at all, and it must certainly not leave a pane behind.
func TestLoginRefusesAKeyringBackedCodexAccountBeforeSpawning(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	dir, err := agentaccount.Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("cli_auth_credentials_store = \"keyring\"\n"), 0o600); err != nil {
		t.Fatalf("write account config: %v", err)
	}
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	if _, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"}); err == nil {
		t.Fatal("a keyring-backed codex account was logged in")
	}
	if supervisor.Live("codex", "work") {
		t.Fatal("the refused login left a pane behind")
	}
}

// TestLoginRefusesWhenTheAgentIsNotInstalled turns a pane that would exit 127
// with no explanation into a sentence naming what is missing.
func TestLoginRefusesWhenTheAgentIsNotInstalled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", t.TempDir())
	supervisor := New()
	t.Cleanup(supervisor.Stop)
	_, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if err == nil {
		t.Fatal("login ran with no codex on PATH")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("refusal %q does not name the missing binary and where af looked", err)
	}
}

// TestLoginRegistersTheAccountItIsAskedFor keeps the verb idempotent from the
// user's side: logging in to an account nobody has registered yet is the
// ordinary first use, not an error.
func TestLoginRegistersTheAccountItIsAskedFor(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	login, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "fresh"})
	if err != nil {
		t.Fatalf("log in to an unregistered account: %v", err)
	}
	names, err := agentaccount.List(home, "codex")
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(names) != 1 || names[0] != "fresh" {
		t.Fatalf("accounts after login = %v, want [fresh]", names)
	}
	info, err := os.Stat(login.Dir)
	if err != nil {
		t.Fatalf("stat the registered account: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("registered account mode = %v, want 0700", info.Mode().Perm())
	}
}

// TestStopClosesEveryLoginPane keeps a half-finished OAuth flow from outliving
// the daemon that spawned it. A login pane has no Instance, so nothing else
// knows it exists — the #1093/#1104 orphan class.
func TestStopClosesEveryLoginPane(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	if _, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"}); err != nil {
		t.Fatalf("start login pane: %v", err)
	}
	if !supervisor.Live("codex", "work") {
		t.Fatal("the login pane is not live right after a successful start")
	}
	supervisor.Stop()
	if supervisor.Live("codex", "work") {
		t.Fatal("a login pane survived the supervisor's shutdown")
	}
}

// writeLoginAgentFixture installs a stand-in for the agent's login command that
// publishes its OWN process environment and argv, then writes the credential
// artifact through the variable af pointed at the account — so the artifact
// assertion is about af's boundary rather than about the fixture's knowledge of
// where the account is.
//
// Every command in it is absolute or a shell builtin: a stand-in that shadows
// the agent name sits FIRST on PATH, and a relative helper call resolved through
// that same directory would silently do nothing (the class recorded in
// reference_standin_on_path_needs_absolute_commands).
func writeLoginAgentFixture(t *testing.T, dir string, tc loginPaneCase, reportPath string) {
	t.Helper()
	artifact := "${" + tc.configVar + ":-}/" + tc.artifact
	script := "#!/bin/sh\n" +
		"{\n" +
		"  printf 'argv=[%s]\\n' \"$*\"\n" +
		"  /usr/bin/tr '\\000' '\\n' < /proc/$$/environ\n" +
		"} > " + shellquote.Quote(reportPath+".partial") + " && " +
		"/bin/mv -f " + shellquote.Quote(reportPath+".partial") + " " + shellquote.Quote(reportPath) + "\n" +
		"if [ -n \"${" + tc.configVar + ":-}\" ]; then\n" +
		"  /bin/mkdir -p \"$(/usr/bin/dirname \"" + artifact + "\")\" && printf '{}' > \"" + artifact + "\"\n" +
		"fi\n" +
		// Then WAIT, because that is what a real login flow does: it holds the
		// terminal until the human finishes the browser or device-code step. A
		// fixture that exited here would exercise the flow-ended-early path
		// instead (TestLoginReportsAFlowThatEndedBeforeTheHandover).
		"while :; do sleep 1; done\n"
	path := filepath.Join(dir, tc.agent)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write %s login fixture: %v", tc.agent, err)
	}
}

// writeBlockingAgentFixture installs a stand-in that never exits — what an OAuth
// prompt waiting on a human looks like from tmux's side.
func writeBlockingAgentFixture(t *testing.T, dir, agent string) {
	t.Helper()
	path := filepath.Join(dir, agent)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatalf("write %s fixture: %v", agent, err)
	}
}

// reportDeadline bounds how long the pane has to publish its report. It is a
// liveness bound only: the fixture publishes by rename, so no value of it can
// expose a half-written report.
const reportDeadline = 10 * time.Second

func waitForReport(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(reportDeadline)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the login pane never published its environment at %s within %s (last error: %v)",
				path, reportDeadline, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForLogin(t *testing.T, home, agent, name string) bool {
	t.Helper()
	deadline := time.Now().Add(reportDeadline)
	for {
		loggedIn, err := agentaccount.LoggedIn(home, agent, name)
		if err != nil {
			t.Fatalf("probe logged-in state: %v", err)
		}
		if loggedIn || time.Now().After(deadline) {
			return loggedIn
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLoginReportsAFlowThatEndedBeforeTheHandover covers the login that
// completes without ever needing the terminal — `codex login` against a
// credential that is already there, or a flow that answers itself. tmux.Start
// reports that as a pane that vanished, worded for a broken install; af has to
// tell the two apart by the ACCOUNT, not by the launch error.
func TestLoginReportsAFlowThatEndedBeforeTheHandover(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	binDir := t.TempDir()
	dir, err := agentaccount.Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	// Writes the credential and exits at once — a completed login that never
	// asked the human anything.
	script := "#!/bin/sh\nprintf '{}' > " + shellquote.Quote(filepath.Join(dir, "auth.json")) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	login, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("a completed login was reported as a failure: %v", err)
	}
	if !login.Finished {
		t.Fatal("a flow that ended before the handover did not report Finished")
	}
	if !login.LoggedIn {
		t.Fatal("a flow that wrote the credential did not report the account logged in")
	}
	if login.TmuxName != "" {
		t.Fatalf("a finished flow named %q to attach to", login.TmuxName)
	}
}

// TestLoginReportsANoOpAsFailure is #3384's verification requirement at its
// sharpest: a flow that exits leaving the account empty must report failure. The
// alternative is a registered account that looks fine and fails much later, at
// session start, naming none of this.
func TestLoginReportsANoOpAsFailure(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	binDir := t.TempDir()
	// Exits 0 and writes nothing: the shape of an OAuth flow the user abandoned
	// at the browser step, which several of these CLIs report as success.
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	_, err := supervisor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if err == nil {
		t.Fatal("a login that left the account empty was reported as success")
	}
	for _, want := range []string{"without leaving a credential", "not logged in"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("failure %q does not say the account is still not logged in (missing %q)", err, want)
		}
	}
}

// TestLoginJoinsAPaneThisSupervisorDidNotSpawn is the daemon-restart case. A
// login pane can outlive the process that tracked it — a daemon killed with
// SIGKILL never runs its teardown — so the map says "no flow" while the tmux name
// is taken by a login sitting there waiting for its human. Creating it again
// would fail with "tmux session already exists" and, worse, be diagnosed as a
// login that left no credential.
func TestLoginJoinsAPaneThisSupervisorDidNotSpawn(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The daemon that spawned it. Its supervisor is then abandoned WITHOUT Stop,
	// which is exactly what a SIGKILL leaves behind.
	abandoned := New()
	first, err := abandoned.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("start the pane that will be orphaned: %v", err)
	}

	// The next daemon, which has never heard of it.
	successor := New()
	t.Cleanup(successor.Stop)
	rejoined, err := successor.Start(context.Background(), Request{Home: home, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("a successor daemon could not join the orphaned login pane: %v", err)
	}
	if !rejoined.Reused {
		t.Fatal("the successor started a competing pane instead of joining the orphan")
	}
	if rejoined.TmuxName != first.TmuxName {
		t.Fatalf("successor named %q, want the orphaned pane %q", rejoined.TmuxName, first.TmuxName)
	}
	// And having adopted it, the successor owns it: its Stop must reap the pane
	// the abandoned supervisor left, or the orphan simply outlives another daemon.
	successor.Stop()
	if successor.Live("codex", "work") {
		t.Fatal("the adopted pane survived the successor's shutdown")
	}
}

// TestLoginDoesNotAdoptAnotherHomesPane is the cross-home half of the
// daemon-restart case above. The login-pane name is derived from {agent, name}
// and carries no home component, and two agent-factory homes for one OS user
// share one tmux server, so two homes with the same {agent, name} target one tmux
// session name. Home B's Start must NOT adopt home A's in-flight pane: reusing it
// would silently no-op home B's login (the foreign flow keeps writing to home A's
// account dir) while reporting Reused, and home B's Stop/Reap could then kill a
// pane it does not own — the cross-home hazard CleanupSessions is hardened
// against at session/tmux/cleanup.go:658-702. adopt verifies the pane's AF_HOME
// marker names THIS home before reusing it, so a foreign pane is left alone and
// home B falls through to its own flow (here failing: the home-blind name is
// already taken by home A, so it reports a failure rather than a silent reuse).
func TestLoginDoesNotAdoptAnotherHomesPane(t *testing.T) {
	testguard.IsolateTmux(t)
	homeA := testguard.SocketTempDir(t)
	homeB := testguard.SocketTempDir(t)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Home A starts and holds an in-flight login pane on the shared tmux server.
	// The AF_HOME marker it stamps is homeA, resolved from the env at spawn time.
	t.Setenv("AGENT_FACTORY_HOME", homeA)
	abandoned := New()
	t.Cleanup(abandoned.Stop)
	homeAFirst, err := abandoned.Start(context.Background(), Request{Home: homeA, Agent: "codex", Name: "work"})
	if err != nil {
		t.Fatalf("start home A's login pane: %v", err)
	}
	if homeAFirst.Reused {
		t.Fatal("home A's first login reported it reused a pane")
	}

	// Home B is a DIFFERENT agent-factory home on the SAME tmux server, asking
	// for the same {agent, name}. adopt must refuse home A's pane rather than
	// reuse it; the home-blind name is already taken, so home B's own Start then
	// fails instead of silently reporting Reused against the foreign flow.
	t.Setenv("AGENT_FACTORY_HOME", homeB)
	successor := New()
	t.Cleanup(successor.Stop)
	homeBSecond, err := successor.Start(context.Background(), Request{Home: homeB, Agent: "codex", Name: "work"})
	if err == nil {
		t.Fatal("home B adopted home A's pane instead of refusing it; got a successful login against another home's flow")
	}
	if homeBSecond.Reused {
		t.Fatal("home B reported it reused home A's login pane")
	}

	// Harm (a): home A's pane must still be live and untouched — home B neither
	// adopted it into its own supervisor nor killed it.
	if !abandoned.Live("codex", "work") {
		t.Fatal("home A's login pane is not live after home B's start")
	}

	// Harm (b): home B's shutdown must not kill home A's pane. Before the fix
	// adopt had recorded home A's pane into home B's supervisor map, so Stop
	// would kill-session it; with the marker check, home B's map is empty and its
	// Stop touches nothing.
	successor.Stop()
	if !abandoned.Live("codex", "work") {
		t.Fatal("home B's Stop killed home A's login pane — a pane home B never owned")
	}
}

// TestLoginRefusesADotUnderscoreCollisionRatherThanAdoptingTheWrongPane is the
// same-home punctuation hazard that the cross-home marker guard cannot reach:
// two accounts for ONE agent whose names differ only by '.' vs '_' (proj.test
// and proj_test) both register to distinct directories but collapse to ONE tmux
// login-pane name, because toTmuxName folds '.' to '_'. adopt keys pane reuse on
// that sanitized name plus the (account-agnostic, same-home) AF_HOME marker, so
// before the fix a login for proj_test was handed proj.test's already-running
// pane — the operator's credential then landed in proj.test's directory while
// the CLI reported Reused=true for proj_test (#3835).
//
// The registration guard (agentaccount.refuseSanitizationCollision) refuses the
// second name before a pane is ever created, so the operator gets a collision
// error naming the conflicting account instead of a silent wrong-pane attach.
// This asserts the fix where it is decidable: through the production supervisor
// Start path on a real tmux server, with proj.test's pane left live so the
// pre-fix adopt path would have found it.
func TestLoginRefusesADotUnderscoreCollisionRatherThanAdoptingTheWrongPane(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	ctx := context.Background()

	// proj.test opens and holds a live login pane — the precondition for the
	// pre-fix collision, since adopt only finds a pane that ProbeSession can see.
	first, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "proj.test"})
	if err != nil {
		t.Fatalf("start proj.test login pane: %v", err)
	}
	if first.Reused {
		t.Fatal("the first login reported it reused a pane")
	}
	if !supervisor.Live("codex", "proj.test") {
		t.Fatal("proj.test's login pane is not live right after a successful start")
	}

	// proj_test folds to the same tmux login-pane name. This must be REFUSED at
	// registration rather than returning Reused=true against proj.test's pane.
	second, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "proj_test"})
	if err == nil {
		t.Fatal("a login for proj_test was accepted alongside a live proj.test login " +
			"instead of being refused — the dot/underscore collision was not caught")
	}
	if second.Reused {
		t.Fatal("the refused proj_test login was handed proj.test's pane (Reused=true) " +
			"instead of being refused before any pane adoption")
	}
	if !strings.Contains(err.Error(), "collides with existing account") {
		t.Fatalf("the refusal must name the collision, got: %v", err)
	}
	if !strings.Contains(err.Error(), "proj.test") {
		t.Fatalf("the refusal must name the conflicting account proj.test, got: %v", err)
	}

	// proj.test's pane must still be live and untouched — the refused login
	// neither adopted it nor left a competing pane behind.
	if !supervisor.Live("codex", "proj.test") {
		t.Fatal("proj.test's login pane did not survive the refused proj_test login")
	}
	// And proj_test must not have been tracked as a login pane at all.
	if supervisor.Live("codex", "proj_test") {
		t.Fatal("the refused proj_test login left a tracked pane behind")
	}
}

// TestLoginDoesNotCollideADotUnderscorePairAcrossAgents keeps the guard scoped
// per agent. The login-pane name carries the agent, so codex/proj.test and
// claude/proj.test derive to distinct tmux names and must NOT be refused for
// colliding — the collision is within one agent's namespace, exactly as
// refuseCaseCollision is.
func TestLoginDoesNotCollideADotUnderscorePairAcrossAgents(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeBlockingAgentFixture(t, binDir, "codex")
	writeBlockingAgentFixture(t, binDir, "claude")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := New()
	t.Cleanup(supervisor.Stop)
	ctx := context.Background()
	if _, err := supervisor.Start(ctx, Request{Home: home, Agent: "codex", Name: "proj.test"}); err != nil {
		t.Fatalf("start codex/proj.test: %v", err)
	}
	// A same-named account for a DIFFERENT agent must start its own pane, not be
	// refused as a collision.
	other, err := supervisor.Start(ctx, Request{Home: home, Agent: "claude", Name: "proj.test"})
	if err != nil {
		t.Fatalf("a login for a different agent with the same name must not be refused: %v", err)
	}
	if other.Reused {
		t.Fatal("claude/proj.test reused codex/proj.test's pane; the agent segment must keep them apart")
	}
}
