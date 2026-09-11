package agentaccount

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// TestLoginProgram_IsTheAgentsOwnInvocation pins the exact command af runs, per
// agent. These are not derived from the binary name — see loginCommands — so a
// table is the only thing that can hold them, and a wrong one is worse than
// none: the operator concludes the account is broken when af's sentence was.
func TestLoginProgram_IsTheAgentsOwnInvocation(t *testing.T) {
	for agent, want := range map[string]string{
		"claude": "claude auth login",
		"codex":  "codex login --device-auth",
		"gemini": "gemini",
	} {
		got, err := LoginProgram(agent)
		if err != nil {
			t.Fatalf("LoginProgram(%q) returned an error: %v", agent, err)
		}
		if got != want {
			t.Fatalf("LoginProgram(%q) = %q, want %q", agent, got, want)
		}
	}
}

// TestLoginProgram_RefusesAgentsWithNoLoginCommand is the roster refusal. af
// must never guess `<agent> login` for an agent whose flow it has not verified
// (#3057), and the refusal has to say which agents CAN, or the operator is left
// guessing which of the seven works.
func TestLoginProgram_RefusesAgentsWithNoLoginCommand(t *testing.T) {
	for _, agent := range []string{"amp", "aider", "opencode", "devin", "", "nonesuch"} {
		program, err := LoginProgram(agent)
		if err == nil {
			t.Fatalf("LoginProgram(%q) = %q, want a refusal", agent, program)
		}
		if !errors.Is(err, ErrUnsupportedAgent) {
			t.Fatalf("LoginProgram(%q) error %v is not ErrUnsupportedAgent", agent, err)
		}
		for _, want := range []string{"claude", "codex", "gemini"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("LoginProgram(%q) error %q does not name the agent %q that can log in", agent, err, want)
			}
		}
	}
}

// TestLoginAgents_MatchesTheAccountRoster is the invariant that keeps the two
// tables from drifting, and it reads the ROSTER rather than a copy of it — a
// hardcoded list here would go stale the moment a fourth agent is added, which
// is the one moment it needs to speak.
//
// Both directions are failures. An agent af can scope a session to but cannot log
// in leaves an operator with a registered account and no way to fill it (which is
// what gemini was before #3384). An agent af offers to log in but cannot scope
// spends a real login on a directory no session can select.
func TestLoginAgents_MatchesTheAccountRoster(t *testing.T) {
	roster := sessionenv.AccountAgents()
	if !slices.Equal(LoginAgents(), roster) {
		t.Fatalf("LoginAgents() = %v, want exactly the account roster %v — every account-scoped agent needs a "+
			"verified login command, and af must not offer a login for an agent it cannot scope",
			LoginAgents(), roster)
	}
}

// TestLoggedIn_ReadsTheAgentsOwnArtifact is the verification requirement: af
// reports logged-in from the file the agent's own flow writes, never from the
// login command's exit code.
func TestLoggedIn_ReadsTheAgentsOwnArtifact(t *testing.T) {
	for agent, artifact := range map[string]string{
		"claude": ".credentials.json",
		"codex":  "auth.json",
		"gemini": ".gemini/gemini-credentials.json",
	} {
		home := t.TempDir()
		dir, err := Register(home, agent, "work")
		if err != nil {
			t.Fatalf("register %s account: %v", agent, err)
		}
		loggedIn, err := LoggedIn(home, agent, "work")
		if err != nil {
			t.Fatalf("LoggedIn(%s) before login: %v", agent, err)
		}
		if loggedIn {
			t.Fatalf("%s: a freshly registered account reports logged in", agent)
		}
		path := filepath.Join(dir, artifact)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("prepare %s artifact directory: %v", agent, err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s artifact: %v", agent, err)
		}
		loggedIn, err = LoggedIn(home, agent, "work")
		if err != nil {
			t.Fatalf("LoggedIn(%s) after login: %v", agent, err)
		}
		if !loggedIn {
			t.Fatalf("%s: account holding %s does not report logged in", agent, artifact)
		}
	}
}

// TestLoggedIn_NeverOpensTheCredential is the design principle made checkable.
// af sets a directory and hands the terminal to the agent; it must not read what
// the agent wrote there. A credential the process cannot open at all still
// answers the question, because the question is answered by stat.
func TestLoggedIn_NeverOpensTheCredential(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions, so an unreadable file proves nothing here")
	}
	home := t.TempDir()
	dir, err := Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"token":"unreadable"}`), 0o000); err != nil {
		t.Fatalf("write unreadable credential: %v", err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Fatal("fixture is not unreadable; the assertion below would prove nothing")
	}
	loggedIn, err := LoggedIn(home, "codex", "work")
	if err != nil {
		t.Fatalf("LoggedIn on an unreadable credential: %v", err)
	}
	if !loggedIn {
		t.Fatal("LoggedIn could not answer without reading the credential")
	}
}

// TestLoggedIn_IgnoresAnEmptyArtifact keeps a truncated or half-written file
// from reporting an identity that is not there. A zero-length auth.json is what
// an interrupted login leaves behind, and reporting it as logged in produces
// exactly the dead account #3384 requires be reported as a failure.
func TestLoggedIn_IgnoresAnEmptyArtifact(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), nil, 0o600); err != nil {
		t.Fatalf("write empty artifact: %v", err)
	}
	loggedIn, err := LoggedIn(home, "codex", "work")
	if err != nil {
		t.Fatalf("LoggedIn: %v", err)
	}
	if loggedIn {
		t.Fatal("an empty credential file reported the account as logged in")
	}
}

// TestLoggedIn_RefusesAnUnregisteredOrUnsupportedAccount routes the probe
// through the same Dir() validation every other reader uses, so a traversal name
// or an off-roster agent cannot make af stat an arbitrary path.
func TestLoggedIn_RefusesAnUnregisteredOrUnsupportedAccount(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct{ agent, name string }{
		{"amp", "work"},
		{"codex", "../../etc"},
		{"codex", "never-registered"},
	} {
		if _, err := LoggedIn(home, tc.agent, tc.name); err == nil {
			t.Fatalf("LoggedIn(%q, %q) succeeded, want a refusal", tc.agent, tc.name)
		}
	}
}

// TestCodexKeyringCollapse_RefusesAKeyringBackedAccount is the trap from
// #3384's preconditions comment: with cli_auth_credentials_store = "keyring",
// codex ignores the account's auth.json and uses the machine-wide identity, so
// every account authenticates as the same person while af reports several. A
// login whose result will be ignored must not run.
func TestCodexKeyringCollapse_RefusesAKeyringBackedAccount(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	config := "model = \"gpt-5\"\ncli_auth_credentials_store = \"keyring\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write account config: %v", err)
	}
	_, err = CheckLoginPreconditions("codex", dir)
	if err == nil {
		t.Fatal("a keyring-backed codex account was accepted for login")
	}
	for _, want := range []string{"cli_auth_credentials_store", "keyring", "config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// TestCodexKeyringCollapse_AcceptsTheFileBackedModes proves the guard is about
// the keyring value and not about the setting existing.
func TestCodexKeyringCollapse_AcceptsTheFileBackedModes(t *testing.T) {
	for _, config := range []string{
		"",
		"model = \"gpt-5\"\n",
		"cli_auth_credentials_store = \"file\"\n",
		"cli_auth_credentials_store = \"auto\"\n",
	} {
		home := t.TempDir()
		dir, err := Register(home, "codex", "work")
		if err != nil {
			t.Fatalf("register account: %v", err)
		}
		if config != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
				t.Fatalf("write account config: %v", err)
			}
		}
		if _, err := CheckLoginPreconditions("codex", dir); err != nil {
			t.Fatalf("config %q was refused: %v", config, err)
		}
	}
}

// TestCheckLoginPreconditions_SaysWhatCodexHomeRelocates is the second
// precondition: CODEX_HOME moves codex's WHOLE home — auth, history, settings
// and skills — so a fresh account reads as "af broke my codex" unless af says
// so once, at the moment the operator is about to use it.
func TestCheckLoginPreconditions_SaysWhatCodexHomeRelocates(t *testing.T) {
	home := t.TempDir()
	dir, err := Register(home, "codex", "work")
	if err != nil {
		t.Fatalf("register account: %v", err)
	}
	notices, err := CheckLoginPreconditions("codex", dir)
	if err != nil {
		t.Fatalf("CheckLoginPreconditions: %v", err)
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"CODEX_HOME", "history"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("codex notices %q do not mention %q", joined, want)
		}
	}
}

// TestLoginSessionName_IsStableAndScoped keeps two accounts from sharing one
// login pane, and keeps the name inside the af_ namespace every af cleanup path
// recognizes.
func TestLoginSessionName_IsStableAndScoped(t *testing.T) {
	first := LoginSessionName("codex", "work")
	if first != LoginSessionName("codex", "work") {
		t.Fatal("LoginSessionName is not stable for one account")
	}
	seen := map[string]string{}
	for _, tc := range []struct{ agent, name string }{
		{"codex", "work"}, {"codex", "personal"}, {"claude", "work"}, {"gemini", "work"},
	} {
		got := LoginSessionName(tc.agent, tc.name)
		if other, clash := seen[got]; clash {
			t.Fatalf("%s/%s and %s share the login session name %q", tc.agent, tc.name, other, got)
		}
		seen[got] = tc.agent + "/" + tc.name
	}
}

// TestLoginSessionName_HexNamespaceSeparation verifies that an account whose raw
// name looks like a hex string cannot collide with the newly-encoded form of a
// different account. For example, the account named "776f726b" must not share a
// session title with the account named "work" (whose hex encoding is "776f726b").
// The "x-" infix in the new format separates the two namespaces: the legacy form
// "af-login-<agent>-776f726b" and the new form "af-login-<agent>-x-776f726b" are
// distinct even though the hex body is identical.
func TestLoginSessionName_HexNamespaceSeparation(t *testing.T) {
	// "work" hex-encodes to "776f726b"; an account literally named "776f726b"
	// must not produce the same session title as the account named "work".
	work := LoginSessionName("codex", "work")
	hexLiteral := LoginSessionName("codex", "776f726b")
	if work == hexLiteral {
		t.Fatalf("account %q and account %q share the session name %q — hex-namespace separation is broken", "work", "776f726b", work)
	}
}

// TestLoginSessionName_IsInjectiveAcrossDotVersusUnderscore is the collision this
// package cannot see at the tmux boundary but must prevent regardless: the titles
// it produces reach tmux's sanitizer (toTmuxName), which rewrites '.' to '_', so
// two names that differ only by '.' vs '_' must not collapse onto one title. This
// package cannot import session/tmux, so the stable-rune policy is mirrored here
// rather than cross-checked — the policy is "letters, digits, '_', '-' only".
func TestLoginSessionName_IsInjectiveAcrossDotVersusUnderscore(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"work.proj", "work_proj"},
		{"a.b.c", "a_b_c"},
		{"work.proj", "work-proj"},
		{"x.", "x_"},
		{".x", "_x"},
	} {
		gotA := LoginSessionName("codex", tc.a)
		gotB := LoginSessionName("codex", tc.b)
		if gotA == gotB {
			t.Fatalf("%q and %q produce the same login session name %q", tc.a, tc.b, gotA)
		}
	}
}

// TestLoginSessionName_ProducesTmuxStableRunes mirrors the positive punctuation
// policy tmux's toTmuxName uses (session/tmux/session.go: stableTmuxNameRune) —
// letters, digits, '_' and '-' — without importing that package, which this one
// cannot. Every character of the title must survive the sanitizer unchanged, or
// the name af probes could be rewritten to one it does not own.
func TestLoginSessionName_ProducesTmuxStableRunes(t *testing.T) {
	for _, name := range []string{
		"work.proj", "work_proj", "work-proj",
		"a.b.c", "MiXeD.CaSe-1", "x...y", "_undot",
		"with space", "tab\there", "p@ss", "ünïcödé",
	} {
		got := LoginSessionName("codex", name)
		// toTmuxName prepends "af_" and writes spaces to nothing; everything that
		// reaches the sanitizer from the body must be a stable rune.
		for _, r := range got {
			if !(strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", r)) {
				t.Fatalf("LoginSessionName(%q) produced non-stable rune %q in %q", name, r, got)
			}
		}
	}
}
