package sessionenv

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAgentForCommandRequiresLiteralAgentInvocation(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "bare agent", command: "codex --model o3", want: "codex"},
		{name: "agent path", command: "/opt/bin/claude --permission-mode plan", want: "claude"},
		{name: "literal assignment", command: "CODEX_HOME=/tmp/codex codex", want: "codex"},
		{name: "exec", command: "exec -- gemini --model flash", want: "gemini"},
		{name: "env", command: "env -i HOME=/tmp aider --model sonnet", want: "aider"},
		{name: "agent name used as data", command: "./collect codex"},
		{name: "agent server title lookalike", command: "/srv/af agent-server --listen :43110 --repo /workspace --title codex"},
		{name: "compound command", command: "collect; codex"},
		{name: "redirect", command: "codex >output"},
		{name: "dynamic argument", command: "codex --model $MODEL"},
		{name: "unsupported wrapper", command: "command codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AgentForCommand(test.command); got != test.want {
				t.Fatalf("AgentForCommand(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestAgentForCommandRejectsUntrustedAgentServerHandoff(t *testing.T) {
	for _, executable := range []string{"./af", "/tmp/af", "af"} {
		command := executable + " agent-server --listen :1 --repo /r --title t --program codex --program-resolved --session-env FOO"
		if got := AgentForCommand(command); got != "" {
			t.Errorf("AgentForCommand(%q) = %q, want no agent for an unauthenticated af-looking executable", command, got)
		}
	}
}

func TestAgentForCommandAuthenticatesEnvWrapper(t *testing.T) {
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "env codex", want: "codex"},
		{command: "/usr/bin/env codex", want: "codex"},
		{command: "/bin/env codex", want: "codex"},
		{command: "./env codex"},
		{command: "/tmp/env codex"},
	} {
		if got := AgentForCommand(test.command); got != test.want {
			t.Errorf("AgentForCommand(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}
func TestAgentForCommandDashTerminatesEnvOptions(t *testing.T) {
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "env - claude", want: "claude"},
		{command: "env - -i claude"},
		{command: "env - -C /evilpath claude"},
		{command: "/usr/bin/env - -i claude"},
		{command: "env -i -u PATH claude", want: "claude"},
	} {
		if got := AgentForCommand(test.command); got != test.want {
			t.Errorf("AgentForCommand(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

func TestCredentialAgentForCommandRequiresBareExecutable(t *testing.T) {
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "codex --model o3", want: "codex"},
		{command: "exec -- codex", want: "codex"},
		{command: "/usr/bin/env codex", want: "codex"},
		{command: "env codex", want: "codex"},
		// Non-resolution assignments are all tolerated: POSIX lookup does not
		// consult them, and a checked-in config is refused assignment-carrying
		// overrides at load — only operator layers can spell one (#5265 review).
		{command: "CLAUDE_CODE_USE_BEDROCK=1 claude", want: "claude"},
		{command: "env CLAUDE_CODE_USE_VERTEX=1 claude", want: "claude"},
		{command: "GOOGLE_GENAI_USE_VERTEXAI=true gemini", want: "gemini"},
		{command: "./codex"},
		{command: "/opt/bin/codex"},
		{command: "exec -- ./codex"},
		{command: "/usr/bin/env /opt/bin/codex"},
		{command: "TERM=xterm-256color codex", want: "codex"},
		{command: "env LANG=C codex", want: "codex"},
		{command: "LD_PRELOAD=./steal.so codex", want: "codex"},
		{command: "env -u HOME codex", want: "codex"},
		{command: "CLAUDE_CODE_USE_BEDROCK=1 codex", want: "codex"},
		{command: "PATH=/workspace codex"},
		{command: "env PATH=/workspace codex"},
		{command: "env -u PATH codex"},
		{command: "env --unset=PATH codex"},
		{command: "env -i codex"},
		{command: "env -C /workspace codex"},
	} {
		if got := credentialAgentForCommand(test.command); got != test.want {
			t.Errorf("credentialAgentForCommand(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

// The credential grant applies only when the executable is one af can
// attribute to the operator: a bare name whose first PATH hit is an absolute
// directory outside the session's working directory, or an absolute path
// outside it. Every spelling that lets the checked-out repository pick the
// file — a relative path, a resolution-changing environment mutation, an
// env wrapper that itself fails attribution, an empty or relative PATH entry
// ahead of the executable, an absolute path inside workDir — earns no grant,
// for every supported agent (#5265).
func TestGrantAgentForCommandRequiresOperatorAttributableExecutable(t *testing.T) {
	workDir := t.TempDir()
	// An in-tree symlink whose target is OUTSIDE the tree: the lexical
	// spelling is repository content whatever its file selects (#5265 review).
	outside := t.TempDir()
	writeExecutableFixture(t, outside, "codex")
	inside := filepath.Join(workDir, "codex")
	if err := os.Symlink(filepath.Join(outside, "codex"), inside); err != nil {
		t.Fatal(err)
	}
	with := t.TempDir()
	writeExecutableFixture(t, with, "codex")
	writeExecutableFixture(t, with, "env")
	without := t.TempDir()
	tests := []struct {
		name    string
		command string
		environ []string
		workDir string
		want    string
	}{
		{name: "bare agent", command: "codex", want: "codex"},
		{name: "bare agent PATH with hit", command: "codex", environ: []string{"PATH=" + with}, want: "codex"},
		{name: "relative dot", command: "./codex"},
		{name: "relative subdir", command: "bin/codex"},
		{name: "relative parent", command: "../codex"},
		{name: "absolute path", command: "/opt/codex", want: "codex"},
		{name: "absolute path outside workDir", command: "/opt/codex", workDir: workDir, want: "codex"},
		{name: "absolute path inside workDir", command: inside, workDir: workDir},
		{name: "absolute path deeper inside workDir", command: filepath.Join(workDir, "bin", "codex"), workDir: workDir},
		{name: "shell wrapper", command: "sh -c codex"},
		{name: "PATH assignment", command: "PATH=/opt codex"},
		{name: "env PATH mutation", command: "env PATH=/opt codex"},
		{name: "env PATH removal", command: "env -u PATH codex"},
		{name: "env clear", command: "env -i codex"},
		{name: "env chdir", command: "env -C /opt codex"},
		{name: "env relative", command: "env ./codex"},
		{name: "exec relative", command: "exec -- ./codex"},
		{name: "exec absolute", command: "exec -- /opt/codex", want: "codex"},
		// Non-resolution assignments keep the grant: the string alone cannot
		// tell an operator's own override (ANTHROPIC_BASE_URL=… claude) from a
		// repo-authored one, so checked-in config is refused them at load
		// instead (config.LoadInRepoConfig, #5265 review).
		{name: "term assignment", command: "TERM=xterm-256color codex", want: "codex"},
		{name: "agent home assignment", command: "CODEX_HOME=/tmp/codex-home codex", want: "codex"},
		{name: "preload assignment", command: "LD_PRELOAD=./steal.so codex", want: "codex"},
		{name: "cloud selector assignment", command: "CLAUDE_CODE_USE_BEDROCK=1 claude", want: "claude"},
		{name: "cloud selector env assignment", command: "env CLAUDE_CODE_USE_VERTEX=1 claude", want: "claude"},
		{name: "cloud selector on other agent", command: "CLAUDE_CODE_USE_BEDROCK=1 codex", want: "codex"},
		{name: "trusted env", command: "/usr/bin/env codex", want: "codex"},
		{name: "env non-resolution assignment", command: "env LANG=C codex", want: "codex"},
		{name: "env unset non-PATH", command: "env -u HOME codex", want: "codex"},
		{name: "bare env attributed", command: "env codex", environ: []string{"PATH=" + with}, want: "codex"},
		{name: "bare env unattributable", command: "env codex", environ: []string{"PATH=."}},
		{name: "bare env all-absolute no hit", command: "env codex", environ: []string{"PATH=" + without}, want: "codex"},
		{name: "bare env absolute operand", command: "env /opt/codex", environ: []string{"PATH=" + with}, want: "codex"},
		{name: "in-tree symlink absolute", command: inside, workDir: workDir},
		{name: "bare agent dot PATH first", command: "codex", environ: []string{"PATH=.:" + with}},
		{name: "bare agent relative PATH first", command: "codex", environ: []string{"PATH=bin:" + with}},
		{name: "bare agent hit before relative PATH", command: "codex", environ: []string{"PATH=" + with + ":bin"}, want: "codex"},
		{name: "bare agent miss before relative PATH", command: "codex", environ: []string{"PATH=" + without + ":bin"}},
		{name: "bare agent leading empty PATH", command: "codex", environ: []string{"PATH=:" + with}},
		{name: "bare agent empty PATH", command: "codex", environ: []string{"PATH="}},
		{name: "bare agent all-absolute PATH no hit", command: "codex", environ: []string{"PATH=" + without}, want: "codex"},
		{name: "bare agent no PATH entry", command: "codex", environ: []string{"HOME=/h"}, want: "codex"},
		{name: "claude bare", command: "claude", want: "claude"},
		{name: "claude relative", command: "./claude"},
		{name: "claude relative subdir", command: "bin/claude"},
		{name: "claude absolute with flags", command: "/usr/local/bin/claude --verbose", want: "claude"},
		{name: "gemini bare", command: "gemini", want: "gemini"},
		{name: "gemini relative", command: "./gemini"},
		{name: "aider relative parent", command: "../aider"},
		{name: "amp relative subdir", command: "bin/amp"},
		{name: "opencode bare", command: "opencode", want: "opencode"},
		{name: "opencode relative", command: "./opencode"},
		{name: "devin bare", command: "devin", want: "devin"},
		{name: "devin relative", command: "./devin"},
		{name: "agent name as data", command: "./collect codex"},
		{name: "compound command", command: "collect; codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := GrantAgentForCommand(test.command, test.environ, test.workDir); got != test.want {
				t.Errorf("GrantAgentForCommand(%q, %v, %q) = %q, want %q",
					test.command, test.environ, test.workDir, got, test.want)
			}
		})
	}
}

// The remote variant cannot stat the remote PATH, so a bare name or an env
// wrapper is attributed the way it was before the predicate existed — remote
// PATH is image/sshd state, not repository content. Every spelling the
// repository CAN control is denied exactly as it is locally.
func TestGrantAgentForRemoteCommand(t *testing.T) {
	workDir := t.TempDir()
	tests := []struct {
		name    string
		command string
		workDir string
		want    string
	}{
		{name: "bare name", command: "codex", want: "codex"},
		{name: "absolute outside", command: "/opt/codex --fast", want: "codex"},
		{name: "env wrapped bare", command: "env codex", want: "codex"},
		{name: "relative", command: "./codex"},
		{name: "relative nested", command: "bin/codex"},
		{name: "absolute inside workspace", command: filepath.Join(workDir, "codex"), workDir: workDir},
		{name: "env wrapped relative", command: "env ./codex"},
		{name: "assignment", command: "TERM=xterm-256color codex", want: "codex"},
		{name: "env mutation non-PATH", command: "env -u HOME codex", want: "codex"},
		{name: "selector on wrong agent", command: "CLAUDE_CODE_USE_BEDROCK=1 codex", want: "codex"},
		{name: "selector on its agent", command: "CLAUDE_CODE_USE_BEDROCK=1 claude", want: "claude"},
		{name: "untrusted env path", command: "/tmp/env codex"},
		{name: "not an agent", command: "make build"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := GrantAgentForRemoteCommand(test.command, test.workDir); got != test.want {
				t.Errorf("GrantAgentForRemoteCommand(%q, %q) = %q, want %q",
					test.command, test.workDir, got, test.want)
			}
		})
	}
}

// A PATH entry whose file carries an execute BIT the daemon cannot use is not
// a hit at all: execvp treats EACCES as "does not hold it" and continues the
// walk, so the grant must too — otherwise resolution falls through to a later
// repository-controlled entry while af attributes the wrong file (#5265).
func TestGrantAgentForCommandSkipsInaccessiblePathHit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("access(2) reports X_OK for root regardless of owner bits")
	}
	// Owner-exec cleared, other-exec set: mode bits say executable, but this
	// process — the file's owner — cannot execute it.
	noexec := t.TempDir()
	inaccessible := filepath.Join(noexec, "codex")
	if err := os.WriteFile(inaccessible, []byte("#!/bin/sh\n"), 0o001); err != nil {
		t.Fatal(err)
	}
	with := t.TempDir()
	writeExecutableFixture(t, with, "codex")

	if got := GrantAgentForCommand("codex", []string{"PATH=" + noexec + ":" + with}, ""); got != "codex" {
		t.Errorf("inaccessible entry must be skipped like execvp's EACCES, grant = %q", got)
	}
	if got := GrantAgentForCommand("codex", []string{"PATH=" + noexec + ":."}, ""); got != "" {
		t.Errorf("inaccessible entry then relative entry must deny, grant = %q", got)
	}
}

// The search-path walk models execvp: directories are tried in order, the
// first one holding an executable file wins, and only that hit decides the
// grant — entries AFTER it are never consulted, so an operator's trailing
// ':' does not cost their agents the credential grant. Entries BEFORE the
// hit still decide: an empty or relative entry resolves into the pane's
// working directory, which is repository content (#5265 review).
func TestGrantAgentForCommandWalksSearchPathLikeExecvp(t *testing.T) {
	with := t.TempDir()
	writeExecutableFixture(t, with, "codex")
	without := t.TempDir()
	without2 := t.TempDir()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "absolute with hit then empty", path: with + ":", want: "codex"},
		{name: "empty then absolute with hit", path: ":" + with},
		{name: "miss then dot then hit", path: without + ":.:" + with},
		{name: "absolute with hit then dot", path: with + ":.", want: "codex"},
		{name: "miss then hit", path: without + ":" + with, want: "codex"},
		{name: "miss then relative then hit", path: without + ":bin:" + with},
		// All-absolute walk with no hit grants: execvp can then only fail,
		// never land in the repository — denying would strip an operator's
		// grant from a bare name that is simply not installed (#5265).
		{name: "all-absolute no hit", path: without + ":" + without2, want: "codex"},
		{name: "no hit then relative", path: without + ":" + without2 + ":bin"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := GrantAgentForCommand("codex", []string{"PATH=" + test.path}, t.TempDir()); got != test.want {
				t.Errorf("GrantAgentForCommand on PATH=%q = %q, want %q", test.path, got, test.want)
			}
		})
	}

	// A hit inside the session's working tree is still repository content:
	// the first-match rule narrows WHICH entry answers, not what containment
	// means for the one that does.
	workDir := t.TempDir()
	writeExecutableFixture(t, workDir, "codex")
	if got := GrantAgentForCommand("codex", []string{"PATH=" + workDir}, workDir); got != "" {
		t.Errorf("GrantAgentForCommand with the executable inside workDir = %q, want no grant", got)
	}

	// A non-executable file is not a hit: execvp continues past EACCES, so
	// the walk falls through to the relative entry and denies.
	noexec := t.TempDir()
	if err := os.WriteFile(filepath.Join(noexec, "codex"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := GrantAgentForCommand("codex", []string{"PATH=" + noexec + ":" + with}, t.TempDir()); got != "codex" {
		t.Errorf("GrantAgentForCommand past a non-executable same-name file = %q, want %q", got, "codex")
	}
	if got := GrantAgentForCommand("codex", []string{"PATH=" + noexec + ":."}, t.TempDir()); got != "" {
		t.Errorf("GrantAgentForCommand with a non-executable file then a relative entry = %q, want no grant", got)
	}
}

// writeExecutableFixture plants an executable file the search-path walk can
// find — the walk's "does this directory hold the executable" needs a real
// file with a real execute bit, not a path that merely spells one.
func writeExecutableFixture(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestAbsoluteSystemEnvPreservesAgentCredentials(t *testing.T) {
	const credential = "OPENAI_API_KEY=fixture"
	for _, command := range []string{"/usr/bin/env codex", "/bin/env codex"} {
		agent := AgentForCommand(command)
		if got := FilterForCommand([]string{credential}, agent, command, nil); !slices.Contains(got, credential) {
			t.Errorf("authenticated system env command %q lost the Codex credential", command)
		}
	}

	command := "./env codex"
	if got := FilterForCommand([]string{credential}, AgentForCommand(command), command, nil); slices.Contains(got, credential) {
		t.Fatal("repository env lookalike received the Codex credential")
	}
}

func TestAgentServerProgramAcceptsOnlyGeneratedGrammar(t *testing.T) {
	base := []string{"agent-server", "--listen", ":43110", "--repo", "/workspace", "--title", "test"}
	withProgram := append(append([]string(nil), base...),
		"--program", "opencode --model test", "--program-resolved", "--session-env", "CUSTOM_TOKEN")
	if got, ok := agentServerProgram(withProgram); !ok || got != "opencode --model test" {
		t.Fatalf("generated explicit-program handoff = (%q, %v), want (opencode command, true)", got, ok)
	}
	if got, ok := agentServerProgram(base); !ok || got != "claude" {
		t.Fatalf("generated default-program handoff = (%q, %v), want (claude, true)", got, ok)
	}
	withEnv := append(append([]string(nil), base...), "--session-env", "CUSTOM_TOKEN")
	if got, ok := agentServerProgram(withEnv); !ok || got != "claude" {
		t.Fatalf("generated default handoff with session env = (%q, %v), want (claude, true)", got, ok)
	}
	withBranch := append(append([]string(nil), withProgram...), "--branch", "af/my-session")
	if got, ok := agentServerProgram(withBranch); !ok || got != "opencode --model test" {
		t.Fatalf("handoff with the daemon-named branch = (%q, %v), want (opencode command, true)", got, ok)
	}
	if _, ok := agentServerProgram(append(append([]string(nil), base...), "--branch")); ok {
		t.Fatal("--branch without a value must be rejected")
	}
	withPolicy := append(append([]string(nil), withBranch...), "--policy-file", "/srv/af-session/af-policy.json")
	if got, ok := agentServerProgram(withPolicy); !ok || got != "opencode --model test" {
		t.Fatalf("handoff with the operator policy file = (%q, %v), want (opencode command, true)", got, ok)
	}
	for _, bad := range []string{"", "relative.json"} {
		if _, ok := agentServerProgram(append(append([]string(nil), base...), "--policy-file", bad)); ok {
			t.Fatalf("--policy-file %q must be rejected: only an absolute path is read", bad)
		}
	}
	withAdopt := append(append([]string(nil), withBranch...), "--adopt-worktree", "/srv/af-session/workspace-wt")
	if got, ok := agentServerProgram(withAdopt); !ok || got != "opencode --model test" {
		t.Fatalf("handoff reattaching a preserved worktree = (%q, %v), want (opencode command, true)", got, ok)
	}
	for _, bad := range []string{"", "relative", "-rf"} {
		if _, ok := agentServerProgram(append(append([]string(nil), base...), "--adopt-worktree", bad)); ok {
			t.Fatalf("--adopt-worktree %q must be rejected: only an absolute path is bound", bad)
		}
	}
	malformed := append(append([]string(nil), base...), "--program-resolved")
	if got, ok := agentServerProgram(malformed); ok || got != "" {
		t.Fatalf("malformed handoff = (%q, %v), want no program", got, ok)
	}
}

func TestFilterScopesAuthenticationToSelectedAgent(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"LC_ALL=C",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"ANTHROPIC_API_KEY=present",
		"UNRELATED_DATABASE_KEY=present",
	}
	got := Filter(source, "codex", nil)

	for _, want := range []string{"PATH=/usr/bin", "LC_ALL=C", "GH_TOKEN=present", "OPENAI_API_KEY=present"} {
		if !slices.Contains(got, want) {
			t.Fatalf("filtered environment is missing %s", want[:len(want)-len("present")])
		}
	}
	for _, denied := range []string{"ANTHROPIC_API_KEY=present", "UNRELATED_DATABASE_KEY=present"} {
		if slices.Contains(got, denied) {
			t.Fatalf("filtered environment retained disallowed variable %s", denied[:len(denied)-len("=present")])
		}
	}
}

func TestFilterAllowsExplicitExactName(t *testing.T) {
	got := Filter([]string{"CUSTOM_PROVIDER_TOKEN=present", "OTHER_TOKEN=present"}, "codex", []string{"CUSTOM_PROVIDER_TOKEN"})
	if !slices.Contains(got, "CUSTOM_PROVIDER_TOKEN=present") {
		t.Fatal("explicit pass-through variable was removed")
	}
	if slices.Contains(got, "OTHER_TOKEN=present") {
		t.Fatal("unlisted variable survived the default-deny filter")
	}
}

func TestImportNamesIncludesAbsentAllowedNamesAndDynamicLocale(t *testing.T) {
	got := ImportNamesForCommand([]string{"LC_MESSAGES=C", "UNRELATED_DATABASE_KEY=present"}, "codex", "", []string{"CUSTOM_PROVIDER_TOKEN"})
	for _, want := range []string{"PATH", "OPENAI_API_KEY", "CUSTOM_PROVIDER_TOKEN", "LC_MESSAGES"} {
		if !slices.Contains(got, want) {
			t.Fatalf("tmux import names omitted %s", want)
		}
	}
	for _, denied := range []string{"ANTHROPIC_API_KEY", "UNRELATED_DATABASE_KEY"} {
		if slices.Contains(got, denied) {
			t.Fatalf("tmux import names admitted %s", denied)
		}
	}
}

func TestFilterRequiresClaudeProviderSelectorForCloudCredentials(t *testing.T) {
	source := []string{
		"ANTHROPIC_API_KEY=present",
		"AWS_ACCESS_KEY_ID=present",
		"AWS_SECRET_ACCESS_KEY=present",
		"AZURE_CLIENT_SECRET=present",
	}
	got := Filter(source, "claude", nil)
	if !slices.Contains(got, "ANTHROPIC_API_KEY=present") {
		t.Fatal("Claude's direct authentication variable was removed")
	}
	for _, denied := range []string{"AWS_ACCESS_KEY_ID=present", "AWS_SECRET_ACCESS_KEY=present", "AZURE_CLIENT_SECRET=present"} {
		if slices.Contains(got, denied) {
			t.Fatalf("Claude inherited inactive provider credential %s", strings.SplitN(denied, "=", 2)[0])
		}
	}

	bedrockSource := append(source, "CLAUDE_CODE_USE_BEDROCK=1")
	bedrock := Filter(bedrockSource, "claude", nil)
	for _, want := range []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=present", "AWS_SECRET_ACCESS_KEY=present"} {
		if !slices.Contains(bedrock, want) {
			t.Fatalf("Claude Bedrock environment omitted %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if slices.Contains(bedrock, "AZURE_CLIENT_SECRET=present") {
		t.Fatal("Claude Bedrock mode inherited inactive Foundry credentials")
	}
}

func TestFilterForCommandHonorsLiteralClaudeCloudModeSelectors(t *testing.T) {
	source := []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AZURE_CLIENT_SECRET=fixture",
	}
	directCommands := []string{
		"CLAUDE_CODE_USE_BEDROCK=1 claude",
		"env CLAUDE_CODE_USE_BEDROCK=true claude",
		"/usr/bin/env CLAUDE_CODE_USE_BEDROCK=true claude",
		"env PATH=/opt/claude CLAUDE_CODE_USE_BEDROCK=1 claude",
		"env -i CLAUDE_CODE_USE_BEDROCK=1 claude",
	}
	for _, command := range directCommands {
		got := FilterForCommand(source, "claude", command, nil)
		for _, want := range []string{"AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture"} {
			if !slices.Contains(got, want) {
				t.Fatalf("literal cloud-mode command omitted %s", strings.SplitN(want, "=", 2)[0])
			}
		}
		if slices.Contains(got, "AZURE_CLIENT_SECRET=fixture") {
			t.Fatal("Bedrock command admitted an inactive Foundry credential")
		}
	}

}

func TestFilterForCommandDoesNotTrustAgentServerArgv(t *testing.T) {
	command := "./af agent-server --listen :1 --repo /r --title t --program 'CLAUDE_CODE_USE_BEDROCK=1 claude' --program-resolved"
	got := FilterForCommand([]string{"AWS_ACCESS_KEY_ID=fixture"}, "claude", command, nil)
	if slices.Contains(got, "AWS_ACCESS_KEY_ID=fixture") {
		t.Fatal("unauthenticated agent-server argv widened the credential filter")
	}
}

func TestFilterForCommandFailsClosedOnDisabledOrDynamicCloudMode(t *testing.T) {
	source := []string{"AWS_ACCESS_KEY_ID=fixture"}
	commands := []string{
		"CLAUDE_CODE_USE_BEDROCK=0 claude",
		"CLAUDE_CODE_USE_BEDROCK=$MODE claude",
		"env CLAUDE_CODE_USE_BEDROCK=$MODE claude",
		"env CLAUDE_CODE_USE_BEDROCK=1 claude --model \"$MODEL\"",
		"CLAUDE_CODE_USE_BEDROCK=1 CLAUDE_CODE_USE_BEDROCK=off claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude --model \"$MODEL\"",
		"helper --program 'CLAUDE_CODE_USE_BEDROCK=1 claude'",
		"claude || CLAUDE_CODE_USE_BEDROCK=1 claude",
		"CLAUDE_CODE_USE_BEDROCK=1 claude || claude",
		"echo env CLAUDE_CODE_USE_BEDROCK=1 claude; ./steal",
	}
	for _, command := range commands {
		if got := FilterForCommand(source, "claude", command, nil); len(got) != 0 {
			t.Fatalf("disabled or dynamic cloud-mode command admitted provider credentials: %q", command)
		}
	}

	exported := []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=fixture"}
	explicitlyDisabled := append([]string(nil), commands[:7]...)
	explicitlyDisabled = append(explicitlyDisabled, "env -i claude", "env --ignore-environment claude")
	for _, command := range explicitlyDisabled {
		if got := FilterForCommand(exported, "claude", command, nil); slices.Contains(got, "AWS_ACCESS_KEY_ID=fixture") {
			t.Fatalf("disabled or dynamic command override inherited an exported provider credential: %q", command)
		}
	}
}

func TestResolvedAuthSelectorsReapplyOnlyValidatedProviderPolicy(t *testing.T) {
	selectors := ResolveAuthSelectors(nil, "claude", "CLAUDE_CODE_USE_BEDROCK=1 claude")
	if !slices.Equal(selectors, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Fatalf("resolved selectors = %v, want the Bedrock selector name", selectors)
	}

	got, err := FilterWithAuthSelectors([]string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AZURE_CLIENT_SECRET=fixture",
	}, "claude", selectors, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture"} {
		if !slices.Contains(got, want) {
			t.Fatalf("resolved Bedrock policy omitted %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if slices.Contains(got, "AZURE_CLIENT_SECRET=fixture") {
		t.Fatal("resolved Bedrock policy admitted Foundry credentials")
	}
}

func TestNormalizeAuthSelectorsRejectsUnknownNameWithoutRenderingIt(t *testing.T) {
	_, err := NormalizeAuthSelectors("claude", []string{"TOKEN=do-not-render"})
	if err == nil {
		t.Fatal("NormalizeAuthSelectors accepted an unknown selector")
	}
	if strings.Contains(err.Error(), "do-not-render") {
		t.Fatal("selector validation error rendered untrusted stored text")
	}
}

func TestNormalizeExtraNamesRejectsPatternsAndAssignments(t *testing.T) {
	for _, name := range []string{"PROVIDER_*", "TOKEN=value", "9TOKEN", "Å_TOKEN", ""} {
		if _, err := NormalizeExtraNames([]string{name}); err == nil {
			t.Fatalf("NormalizeExtraNames accepted invalid name %q", name)
		}
	}
}

func TestNormalizeExtraNamesDoesNotRenderInvalidAssignmentValue(t *testing.T) {
	_, err := NormalizeExtraNames([]string{"CUSTOM_TOKEN=do-not-render"})
	if err == nil {
		t.Fatal("NormalizeExtraNames accepted an assignment")
	}
	if strings.Contains(err.Error(), "do-not-render") {
		t.Fatal("validation error rendered an invalid assignment value")
	}
}

func TestDockerForwardNamesCarriesOnlyExplicitNamesWhenPresent(t *testing.T) {
	source := []string{
		"HOME=/host/home",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"HTTPS_PROXY=present",
		"UNRELATED_DATABASE_KEY=present",
		"CUSTOM_PROVIDER_TOKEN=present",
		"LC_PACKAGE_TOKEN=present",
	}
	extras := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "LC_PACKAGE_TOKEN", "OPENAI_API_KEY"}
	got := DockerForwardNames(source, "codex", extras)
	want := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "LC_PACKAGE_TOKEN", "OPENAI_API_KEY"}
	if !slices.Equal(got, want) {
		t.Fatalf("DockerForwardNames() = %v, want %v", got, want)
	}
	client := DockerCLIEnvironment(source, "codex", extras)
	for _, wantEntry := range []string{"CUSTOM_PROVIDER_TOKEN=present", "GH_TOKEN=present", "HTTPS_PROXY=present", "LC_PACKAGE_TOKEN=present", "OPENAI_API_KEY=present"} {
		if !slices.Contains(client, wantEntry) {
			t.Fatalf("explicit Docker trust grant omitted %s", strings.SplitN(wantEntry, "=", 2)[0])
		}
	}
}

func TestDockerRepoSelectedImageDoesNotReceiveBuiltInCredentials(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=fixture",
		"OPENAI_API_KEY=fixture",
		"HTTPS_PROXY=fixture",
		"SSL_CERT_FILE=/fixture/ca.pem",
		"LC_SECRET_TOKEN=fixture",
	}
	for _, entry := range DockerCLIEnvironment(source, "codex", nil) {
		name, _, _ := strings.Cut(entry, "=")
		for _, denied := range []string{"GH_TOKEN", "OPENAI_API_KEY", "HTTPS_PROXY", "SSL_CERT_FILE", "LC_SECRET_TOKEN"} {
			if name == denied {
				t.Fatalf("Docker CLI environment exposed built-in variable %s to repo-controlled run arguments", name)
			}
		}
	}
	if got := DockerForwardNames(source, "codex", nil); len(got) != 0 {
		t.Fatalf("repo-selected Docker image received built-in credential names: %v", got)
	}
}

func TestWrapCommandNeverRendersEnvironmentValues(t *testing.T) {
	wrapped, err := WrapCommand("/opt/af binary", "codex", []string{"CUSTOM_PROVIDER_TOKEN"}, "codex --model 'a b'")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"__af-session-env-exec", "CUSTOM_PROVIDER_TOKEN", "codex --model"} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("wrapped command missing %q", want)
		}
	}
}

// TestAgentNamespaceForCommandAcceptsPathQualifiedEnv is the regression test for
// the #4356 narrowing. Pre-#4356, AgentForCommand matched env by basename, so a
// session persisted with a path-qualified env wrapper such as
// /usr/local/bin/env CODEX_HOME=/x codex plus a selected account could still
// resolve its agent namespace on restore. #4356 replaced that with the strict
// isTrustedEnvExecutable set (env, /bin/env, /usr/bin/env), so the same program
// fell to "" and the daemon-spawned VS Code editor refused with
// ErrUnsupportedAgent for a session the create-time gate
// tmux.DetectAgentExecutable still accepted. AgentNamespaceForCommand keeps the
// pre-#4356 basename rule for the namespace-only callers that must agree with
// the create gate; AgentForCommand stays strict for credential grants.
func TestAgentNamespaceForCommandAcceptsPathQualifiedEnv(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "bare env", command: "env codex", want: "codex"},
		{name: "bin env", command: "/bin/env codex", want: "codex"},
		{name: "usr bin env", command: "/usr/bin/env codex", want: "codex"},
		{name: "usr local bin env", command: "/usr/local/bin/env CODEX_HOME=/x codex", want: "codex"},
		{name: "nix store env", command: "/run/current-system/sw/bin/env codex", want: "codex"},
		{name: "relative env", command: "./env codex", want: "codex"},
		{name: "tmp env", command: "/tmp/env CODEX_CONFIG_DIR=/y claude", want: "claude"},
		{name: "env with assignments and flags", command: "/usr/local/bin/env -i HOME=/h GEMINI_HOME=/g gemini --resume latest", want: "gemini"},
		{name: "bare agent still classified", command: "codex --model o3", want: "codex"},
		{name: "agent path still classified", command: "/opt/bin/claude --permission-mode plan", want: "claude"},
		{name: "exec wrapper", command: "exec -- gemini --model flash", want: "gemini"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := AgentNamespaceForCommand(test.command); got != test.want {
				t.Fatalf("AgentNamespaceForCommand(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

// TestAgentNamespaceForCommandMatchesAgentForCommandOnStrictSet confirms the two
// functions agree on every spelling the credential-bearing path still trusts, so
// a session created with one of the trusted env spellings is classified
// identically by both surfaces. Disagreement here would mean a session the
// credential path grants to is classified differently by the editor scope.
func TestAgentNamespaceForCommandMatchesAgentForCommandOnStrictSet(t *testing.T) {
	for _, command := range []string{
		"env codex",
		"/bin/env codex",
		"/usr/bin/env codex",
		"env CLAUDE_CODE_USE_BEDROCK=1 claude",
		"/usr/bin/env CODEX_HOME=/x codex --model o3",
		"codex",
		"claude --permission-mode plan",
		"/opt/bin/gemini",
		"exec -- aider",
		"CODEX_HOME=/tmp codex",
	} {
		if got, want := AgentNamespaceForCommand(command), AgentForCommand(command); got != want {
			t.Errorf("namespace and credential classifiers disagree on trusted command %q: namespace=%q credential=%q",
				command, got, want)
		}
	}
}

// TestAgentForCommandStillRejectsPathQualifiedEnv guards the #4356 security
// boundary: the credential-bearing classifier must keep rejecting path-qualified
// env spellings that AgentNamespaceForCommand now accepts. A regression here
// reopens the ./env grant leak the narrowing closed.
func TestAgentForCommandStillRejectsPathQualifiedEnv(t *testing.T) {
	for _, command := range []string{
		"/usr/local/bin/env codex",
		"/run/current-system/sw/bin/env codex",
		"./env codex",
		"/tmp/env claude",
	} {
		if got := AgentForCommand(command); got != "" {
			t.Errorf("AgentForCommand(%q) = %q, want \"\": the credential classifier must keep the strict env set from #4356",
				command, got)
		}
		if sc := credentialAgentForCommand(command); sc != "" {
			t.Errorf("credentialAgentForCommand(%q) = %q, want \"\": path-qualified env must stay credential-free",
				command, sc)
		}
	}
}

// TestAgentNamespaceForCommandRejectsNonAgentCommands ensures the lenient env
// rule widens only the env-wrapper recognition, not the supported-agent set.
// An agent-looking argument to a non-env executable, a compound command, a
// redirect, a dynamic word, and an unsupported wrapper must all still classify
// as no agent — the same reject set AgentForCommand keeps.
func TestAgentNamespaceForCommandRejectsNonAgentCommands(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{name: "agent name as data to another executable", command: "./collect codex"},
		{name: "agent server lookalike", command: "/srv/af agent-server --listen :43110 --repo /workspace --title codex"},
		{name: "compound command", command: "collect; codex"},
		{name: "redirect", command: "codex >output"},
		{name: "dynamic argument", command: "codex --model $MODEL"},
		{name: "unsupported wrapper", command: "command codex"},
		{name: "empty", command: ""},
		{name: "whitespace", command: "   "},
		{name: "bare shell", command: "bash"},
		{name: "env running unknown tool", command: "/usr/local/bin/env unknown-tool --flag"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := AgentNamespaceForCommand(test.command); got != "" {
				t.Errorf("AgentNamespaceForCommand(%q) = %q, want \"\"", test.command, got)
			}
		})
	}
}

// TestImportedCredentialNamesCoversTheExpansionUniverse pins the set a launch
// strips out of a stored tmux update-environment value: every name af's
// transient expansion can add that is credential- or agent-specific. The
// helper derives the set from the name tables, so this test's table walk is
// what keeps it honest — a regression to a hand-listed set fails here the
// moment the tables grow (#5390).
func TestImportedCredentialNamesCoversTheExpansionUniverse(t *testing.T) {
	set := make(map[string]struct{}, len(agentNames))
	for _, name := range ImportedCredentialNames() {
		set[name] = struct{}{}
	}
	for agent, names := range agentNames {
		for name := range names {
			if _, ok := set[name]; !ok {
				t.Errorf("ImportedCredentialNames() omits %s agent name %s", agent, name)
			}
		}
	}
	for agent, groups := range conditionalAgentNames {
		for _, group := range groups {
			for name := range group.names {
				if _, ok := set[name]; !ok {
					t.Errorf("ImportedCredentialNames() omits %s conditional name %s", agent, name)
				}
			}
		}
	}
	for agent, names := range accountLateEnvironmentNames {
		for name := range names {
			if _, ok := set[name]; !ok {
				t.Errorf("ImportedCredentialNames() omits %s late-environment name %s", agent, name)
			}
		}
	}
	for name := range accountShellStartupNames {
		if _, ok := set[name]; !ok {
			t.Errorf("ImportedCredentialNames() omits shell startup hook %s", name)
		}
	}
	for _, name := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"GEMINI_API_KEY", "AMP_API_KEY", "AWS_SECRET_ACCESS_KEY",
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	} {
		if _, ok := set[name]; !ok {
			t.Errorf("ImportedCredentialNames() omits credential name %s", name)
		}
	}
	// Names a plausible hand-set update-environment legitimately carries must
	// survive the strip: SSH_AUTH_SOCK is even in tmux's compiled default, and
	// the rest are ordinary routing/locale configuration.
	for _, name := range []string{
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "DISPLAY", "PATH", "HOME",
		"HTTP_PROXY", "GIT_SSH_COMMAND", "LANG", "LC_ALL",
	} {
		if _, ok := set[name]; ok {
			t.Errorf("ImportedCredentialNames() must not strip plausible operator name %s", name)
		}
	}
}
