package daemon

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// The #3870 boundary: a session created with `--account work` gets that
// account's credential root in every tmux pane, and must get it in the
// daemon-spawned VS Code editor too. The editor's integrated terminals and any
// extension that shells out inherit exactly one environment — the one fixed in
// the child's environ at exec — so an unscoped editor authenticates as the
// AMBIENT identity while every visible signal in af reports the selected
// account. That is #3051's failure mode reached through the door #3051 did not
// close.
//
// These drive the REAL spawn seam: manager.ensureVSCodeServer, the same entry
// the webtab proxy uses, through startOne's exec into the fake editor. The fake
// reports its own os.Environ() over its socket, which IS the environ it was
// exec'd with, so the assertions read the spawned process rather than the
// function that built its environment. (Reading /proc/<pid>/environ would say the
// same thing on Linux and nothing at all in the macOS Test job.)

// accountVSCodeTitle is the fixture session's title, shared by the fixture and
// every caller that needs its daemon instance key.
const accountVSCodeTitle = "vscodeaccount"

// The fake editor's environment routes. They exist so a test can ask the SPAWNED
// process what it actually received, and what a process it spawns in turn
// receives — the two halves of the claim this file makes.
const (
	// fakeVSCodeEnvironPath returns the editor process's own environment.
	fakeVSCodeEnvironPath = "/af-test/environ"
	// fakeVSCodeTerminalPath returns the environment of a SHELL the editor
	// spawned, modelling an integrated terminal: VS Code's terminal is a child of
	// the server process and inherits its environment, so this is where the
	// editor's boundary either reaches a human's shell or does not.
	fakeVSCodeTerminalPath = "/af-test/terminal-environ"
)

// registerFakeVSCodeEnvironmentRoutes adds the two routes above to the fake
// editor's mux. It lives here, beside the tests that read it, so the fixture in
// vscode_server_test.go stays the argv/socket/flavor model it already is.
func registerFakeVSCodeEnvironmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc(fakeVSCodeEnvironPath, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Join(os.Environ(), "\n"))
	})
	mux.HandleFunc(fakeVSCodeTerminalPath, func(w http.ResponseWriter, _ *http.Request) {
		// Inherits the editor's environment, exactly as an integrated terminal
		// does. /bin/sh -c reads no startup file, so what comes back is the
		// inheritance alone — the startup-file half is measured separately by
		// TestVSCodeIntegratedTerminal_StartupFileDefeatsTheAccountBoundary.
		out, err := exec.Command("/bin/sh", "-c", "env").Output()
		if err != nil {
			http.Error(w, "integrated terminal failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
}

// newAccountVSCodeFixture is newVSCodeFixture for an ACCOUNT-SCOPED session: a
// started local instance that selected a registered account and holds a VS Code
// tab, wired to the fake editor. It returns the manager, the instance, its repo
// id, and the account's credential directory.
func newAccountVSCodeFixture(t *testing.T, binary, account string) (*Manager, *session.Instance, string, string) {
	t.Helper()
	home := shortAFHome(t)
	accountDir, err := agentaccount.Register(home, sessiontmux.ProgramClaude, account)
	if err != nil {
		t.Fatalf("registering the fixture account: %v", err)
	}
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	inst := startedAccountTabInstance(t, manager, repo.ID, repoPath, account)
	manager.vscode.configuredBinary = func() string { return binary }
	// Keep the suite fast: the real grace is tuned for a cold Node start.
	manager.vscode.startGrace = 5 * time.Second
	manager.vscode.cooldown = 50 * time.Millisecond
	t.Cleanup(manager.vscode.Stop)

	if _, err := manager.CreateTab(CreateTabRequest{
		Title: accountVSCodeTitle, RepoID: repo.ID, Kind: "vscode",
	}); err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	// The tab fixture's instance is tmux-mocked and never materializes its
	// worktree on disk, but a real editor is a real process with a real cwd.
	if err := os.MkdirAll(inst.GetWorktreePath(), 0o755); err != nil {
		t.Fatalf("creating the fixture worktree: %v", err)
	}
	return manager, inst, repo.ID, accountDir
}

// startedAccountTabInstance is startedLocalTabInstance with a selected account.
// The account is set at CONSTRUCTION rather than assigned afterwards: Instance
// guards it behind its own lock, and the daemon reads it from the proxy
// goroutine.
func startedAccountTabInstance(t *testing.T, m *Manager, repoID, repoPath, account string) *session.Instance {
	t.Helper()
	const agentName = "af_" + accountVSCodeTitle + "_agent"
	mockExec := tabNameKeyedExec(map[string]bool{agentName: true})
	pty := tabPtyFactory{t: t, cmdExec: mockExec}

	gw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), accountVSCodeTitle,
		accountVSCodeTitle+"-branch", "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   accountVSCodeTitle,
		Path:    repoPath,
		Program: sessiontmux.ProgramClaude,
		Account: account,
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedNameWithDeps(
		agentName, sessiontmux.ProgramClaude, pty, mockExec))
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)

	seedDiskInstance(t, repoID, accountVSCodeTitle, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, accountVSCodeTitle)] = inst
	m.mu.Unlock()
	return inst
}

// setAmbientClaudeIdentity puts a complete ambient claude identity in the
// daemon's own environment — the state a developer box is actually in, and the
// one an unscoped editor hands straight to its integrated terminals.
func setAmbientClaudeIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "ambient-claude"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token-ambient")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-ambient")
	// The login-pane browser suppressors (#3854). They are set here the way a
	// daemon inherits them — from whatever autostarted it — because that is the
	// only way they can reach an editor.
	t.Setenv("NO_BROWSER", "1")
	t.Setenv("BROWSER", "true")
}

// editorEnvironment asks the running editor for an environment over its own
// socket. path selects the editor's own environment or a shell it spawned.
func editorEnvironment(t *testing.T, m *Manager, key, path string) []string {
	t.Helper()
	m.vscode.mu.Lock()
	server := m.vscode.servers[key]
	m.vscode.mu.Unlock()
	if server == nil {
		t.Fatal("no editor is registered for the session; there is no environment to read")
	}
	client := &http.Client{Transport: server.transport, Timeout: 10 * time.Second}
	resp, err := client.Get(vscodeUpstreamURL + path)
	if err != nil {
		t.Fatalf("reading %s from the editor: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s body from the editor: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reading %s from the editor: status %d: %s", path, resp.StatusCode, body)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// assertAccountScopedEnvironment is the whole contract in one place: the
// account's credential root is INJECTED, every other identity-bearing name for
// the agent is REMOVED, the login-pane browser suppressors are gone, and nothing
// else is — it is a subtraction of identity, not a wipe.
func assertAccountScopedEnvironment(t *testing.T, env []string, subject, accountDir string) {
	t.Helper()
	switch got, ok := envValue(env, "CLAUDE_CONFIG_DIR"); {
	case !ok:
		t.Errorf("%s has no CLAUDE_CONFIG_DIR: the session selected an account, so the editor "+
			"must carry that account's credential root and not fall back to ~/.claude", subject)
	case got != accountDir:
		t.Errorf("%s has CLAUDE_CONFIG_DIR=%q, want the account's credential root %q: the session "+
			"reports the selected account while authenticating as someone else (#3051)", subject, got, accountDir)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v, ok := envValue(env, name); ok {
			t.Errorf("%s still carries the ambient %s=%q: an API key OUTRANKS the credential "+
				"directory, so injection without subtraction leaves the selection silently ignored", subject, name, v)
		}
	}
	// Named with what each one actually does, because the two differ: gemini reads
	// NO_BROWSER and stops offering the browser leg, while claude EXECS whatever
	// BROWSER names with the URL — so an inherited BROWSER=true has an agent
	// launching a program literally called "true" as the operator's browser.
	for name, consequence := range map[string]string{
		"NO_BROWSER": "an agent run from an integrated terminal would silently stop offering its browser sign-in",
		"BROWSER":    "an agent run from an integrated terminal would exec that value as the operator's browser",
	} {
		if v, ok := envValue(env, name); ok {
			t.Errorf("%s carries the login-pane suppressor %s=%q: it belongs to af's own account "+
				"login flow (#3854), and %s", subject, name, v, consequence)
		}
	}
	if _, ok := envValue(env, "PATH"); !ok {
		t.Errorf("%s lost PATH: this boundary subtracts IDENTITY, not the environment", subject)
	}
}

// TestVSCodeEditor_RunsInTheSelectedAccountEnvironment is #3870's headline: the
// editor process itself. Everything the editor launches — the extension host,
// every extension that shells out, every integrated terminal — inherits this one
// environ, so it is the single place the account either reaches the editor or
// does not.
func TestVSCodeEditor_RunsInTheSelectedAccountEnvironment(t *testing.T) {
	setAmbientClaudeIdentity(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, inst, repoID, accountDir := newAccountVSCodeFixture(t, binary, "work")

	if _, err := manager.ensureVSCodeServer(inst, repoID, accountVSCodeTitle); err != nil {
		t.Fatalf("ensureVSCodeServer: %v", err)
	}
	key := daemonInstanceKey(repoID, accountVSCodeTitle)
	assertAccountScopedEnvironment(t,
		editorEnvironment(t, manager, key, fakeVSCodeEnvironPath), "the editor process", accountDir)
}

// TestVSCodeEditor_IntegratedTerminalInheritsTheAccountEnvironment is the half
// that matters to a human. An integrated terminal is a child of the editor
// server and inherits its environment, so a scoped editor is only worth having
// if the scope survives that fork — which is what the swap's admission refusal
// is arguing about.
//
// It proves INHERITANCE and nothing more: the shell here is `/bin/sh -c`, which
// reads no startup file. The startup-file case is the next test, and it is the
// one that decides whether the refusal can be lifted.
func TestVSCodeEditor_IntegratedTerminalInheritsTheAccountEnvironment(t *testing.T) {
	setAmbientClaudeIdentity(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, inst, repoID, accountDir := newAccountVSCodeFixture(t, binary, "work")

	if _, err := manager.ensureVSCodeServer(inst, repoID, accountVSCodeTitle); err != nil {
		t.Fatalf("ensureVSCodeServer: %v", err)
	}
	key := daemonInstanceKey(repoID, accountVSCodeTitle)
	assertAccountScopedEnvironment(t,
		editorEnvironment(t, manager, key, fakeVSCodeTerminalPath),
		"a shell the editor spawned (an integrated terminal)", accountDir)
}

// TestVSCodeEditor_RefusesWhenTheSelectedAccountCannotBeResolved is the
// fail-closed half. Every other account surface refuses rather than falling
// through to the ambient identity, because an unprovable launch is not evidence
// of a correct one (#3051) — and an editor is the surface where that fallback
// would be least visible, since it comes up looking perfectly healthy.
func TestVSCodeEditor_RefusesWhenTheSelectedAccountCannotBeResolved(t *testing.T) {
	setAmbientClaudeIdentity(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, inst, repoID, accountDir := newAccountVSCodeFixture(t, binary, "work")
	// The account is deregistered out from under the session — a directory moved
	// or removed after the session was created.
	if err := os.RemoveAll(accountDir); err != nil {
		t.Fatalf("removing the account directory: %v", err)
	}

	_, err := manager.ensureVSCodeServer(inst, repoID, accountVSCodeTitle)
	if err == nil {
		t.Fatal("an editor was started for a session whose selected account could not be resolved; " +
			"it would have run on the ambient identity while af reported the account")
	}
	if !errors.Is(err, errVSCodeAccountScope) {
		t.Errorf("the refusal is not the sentinel the pane renderer matches on: %v", err)
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("the refusal does not name the account that could not be resolved: %v", err)
	}
	if vscodeRegistered(manager, daemonInstanceKey(repoID, accountVSCodeTitle)) {
		t.Fatal("the refused spawn left an editor registered")
	}

	// And the operator sees it. The pane iframes this route, so a refusal that
	// answers the generic JSON envelope reads as garbage exactly where the person
	// who can fix it is looking.
	tabs := inst.GetTabs()
	tabID := tabs[len(tabs)-1].ID
	rec := httptest.NewRecorder()
	newHTTPMux(&controlServer{manager: manager}).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(inst.ID, tabID, ""), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "af accounts add") {
		t.Errorf("the pane does not tell the operator how to repair the account; body = %q", body)
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("the account refusal notice self-refreshes; the supervisor replays it for a " +
			"cooldown, so the page would only re-render the same error")
	}
}

// TestVSCodeIntegratedTerminal_StartupFileDefeatsTheAccountBoundary is the
// MEASUREMENT behind requirement 3 of #3870 — whether admitAccountSwap's VS Code
// refusal can be lifted now that the editor is scoped. It cannot, and this is
// why, run rather than argued.
//
// An integrated terminal is an interactive shell. Interactive bash reads
// /etc/bash.bashrc and ~/.bashrc, and one `export CLAUDE_CONFIG_DIR=…` there
// outranks the boundary af installed. Nothing af can put in the environment
// prevents it: the ambient credential root is a DEFAULT PATH under $HOME, so a
// startup file need only unset the variable to reach it.
//
// af's own panes escape this because af chooses their command — the exec shim
// hands the agent to `/bin/sh -c`, which reads no startup file, and an
// account-scoped shell tab is pinned to `bash --noprofile --norc -i`. The
// editor's terminal profile is not af's to choose the same way: it lives in
// code-server's user-data directory, which af deliberately SHARES across every
// session, and which is rewritable from inside the very editor being scoped. A
// default written there is a default, not a boundary.
func TestVSCodeIntegratedTerminal_StartupFileDefeatsTheAccountBoundary(t *testing.T) {
	accountDir := filepath.Join(t.TempDir(), "account")
	ambientDir := filepath.Join(t.TempDir(), "ambient")
	scoped, err := sessionenv.ApplyAccountEnvironment(nil, "", sessionenv.Account{
		Agent: sessiontmux.ProgramClaude, Name: "work", Dir: accountDir,
	})
	if err != nil {
		t.Fatalf("ApplyAccountEnvironment: %v", err)
	}
	if got, _ := envValue(scoped, "CLAUDE_CONFIG_DIR"); got != accountDir {
		t.Fatalf("precondition: the boundary did not install the account root, got %q", got)
	}

	home := t.TempDir()
	rc := "export CLAUDE_CONFIG_DIR=" + ambientDir + "\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0o600); err != nil {
		t.Fatalf("writing the startup file: %v", err)
	}
	// An INTERACTIVE bash, which is what a terminal profile spawns, and the
	// scoped environment the editor would have handed it.
	//
	// The value is read back through a FILE rather than from stdout, because a
	// startup file is free to print: this box's /etc/bash.bashrc emits the sudo
	// lecture, which lands on stdout ahead of the value and would be captured as
	// part of it. That is not noise to route around — it is the same mechanism
	// this test is about, arriving from a file the operator may not even control
	// per-account — so the reading is made immune to it instead of the assertion
	// being loosened to tolerate it.
	valuePath := filepath.Join(t.TempDir(), "resolved")
	shell := exec.Command("/bin/bash", "-i", "-c", "printenv CLAUDE_CONFIG_DIR > '"+valuePath+"'")
	shell.Env = append(append([]string(nil), scoped...), "HOME="+home, "PATH="+os.Getenv("PATH"))
	if err := shell.Run(); err != nil {
		t.Fatalf("running the interactive shell: %v", err)
	}
	out, err := os.ReadFile(valuePath)
	if err != nil {
		t.Fatalf("reading what the interactive shell resolved: %v", err)
	}

	got := strings.TrimSpace(string(out))
	if got == accountDir {
		t.Fatalf("an interactive shell kept the account root %q despite a ~/.bashrc export: "+
			"the startup-file boundary this test documents as UNPROVABLE now appears to hold, so "+
			"admitAccountSwap's VS Code refusal and the file comment in vscode_account_env.go both "+
			"need revisiting rather than trusting", accountDir)
	}
	if got != ambientDir {
		t.Fatalf("interactive shell resolved CLAUDE_CONFIG_DIR=%q, want the startup file's %q; "+
			"this test proves nothing unless it is the ~/.bashrc export that won", got, ambientDir)
	}
}

// TestVSCodeEditor_RespawnsWhenTheSessionAccountChanges is the reuse half. The
// credential root is fixed in the child's environ at exec and can never be
// changed afterwards, so an editor whose session has since selected a different
// account cannot be re-scoped — it can only be replaced. Without the account in
// the reuse identity, the supervisor's "same key, same worktree, still alive"
// test hands the next render an editor holding the PREVIOUS identity's
// credentials, under a session that now reports the new one.
//
// This is also what makes the account swap's teardown a belt rather than the
// only brace: #3869 stops the editor before the boundary changes, and this makes
// a later render start the right one rather than adopt the old.
func TestVSCodeEditor_RespawnsWhenTheSessionAccountChanges(t *testing.T) {
	setAmbientClaudeIdentity(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, inst, repoID, _ := newAccountVSCodeFixture(t, binary, "work")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	replacementDir, err := agentaccount.Register(home, sessiontmux.ProgramClaude, "spare")
	if err != nil {
		t.Fatalf("registering the replacement account: %v", err)
	}

	if _, err := manager.ensureVSCodeServer(inst, repoID, accountVSCodeTitle); err != nil {
		t.Fatalf("ensureVSCodeServer: %v", err)
	}
	key := daemonInstanceKey(repoID, accountVSCodeTitle)
	manager.vscode.mu.Lock()
	first := manager.vscode.servers[key].cmd.Process.Pid
	manager.vscode.mu.Unlock()

	// The swap's own selection path, through the states it requires: a session
	// only swaps identity from a usage-limit wall, under the limit-resume fence.
	inst.SetLimitReached(time.Now().Add(time.Hour))
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatalf("BeginLimitResume: %v", err)
	}
	if _, err := inst.SelectAccountAutomatically("work", "spare"); err != nil {
		t.Fatalf("SelectAccountAutomatically: %v", err)
	}
	if !inst.EndLimitResume() {
		t.Fatal("the limit-resume fence was not held by this test")
	}
	// Settle the swap, which is what the production path does once the replacement
	// notice is delivered (daemon/limit.go). It is not tidying-up: #3869 makes
	// TabSpawnBlocked refuse a session with a swap still pending, so an editor
	// cannot spawn at all until this clears — and a test that skipped it would
	// fail on that refusal with the old editor still registered, reporting a
	// respawn failure it never actually reached. The window this test is about is
	// the SETTLED one: the later render that stopVSCodeForAccountSwap relies on to
	// relaunch the editor.
	if !inst.ClearPendingAccountSwap("work", "spare") {
		t.Fatal("the pending account swap was not the one this test committed")
	}

	if _, err := manager.ensureVSCodeServer(inst, repoID, accountVSCodeTitle); err != nil {
		t.Fatalf("ensureVSCodeServer after the account changed: %v", err)
	}
	manager.vscode.mu.Lock()
	second := manager.vscode.servers[key].cmd.Process.Pid
	manager.vscode.mu.Unlock()
	if second == first {
		t.Fatalf("the editor (pid %d) was reused after the session moved from account %q to %q; "+
			"its credential root is fixed at exec, so reuse serves the previous identity", first, "work", "spare")
	}
	assertAccountScopedEnvironment(t,
		editorEnvironment(t, manager, key, fakeVSCodeEnvironPath),
		"the editor respawned after the account changed", replacementDir)
}
