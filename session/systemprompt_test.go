package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"no quotes", "'no quotes'"},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.expected {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestInjectSystemPrompt_Claude(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", dir)

	result := injectSystemPrompt("claude")

	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
	if !strings.HasPrefix(result, "claude") {
		t.Errorf("expected result to start with 'claude', got %q", result)
	}
	if strings.Contains(result, "--append-system-prompt") {
		t.Errorf("expected no --append-system-prompt flag, got %q", result)
	}
}

func TestInjectSystemPrompt_ClaudeWithResolvedFlags(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", dir)

	// The resolved form (from program_overrides) carries the path-and-flags;
	// injectSystemPrompt appends --plugin-dir to it.
	result := injectSystemPrompt("/usr/local/bin/claude --model opus")

	if !strings.HasPrefix(result, "/usr/local/bin/claude --model opus") {
		t.Errorf("expected resolved form preserved, got %q", result)
	}
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
}

// Codex now gets a FILE seam (its skills folder, 0.144.1+), not the old
// -c developer_instructions= blob (#1043 retired): the launch command comes back
// UNCHANGED and the af skill is written where codex auto-discovers it.
func TestInjectSystemPrompt_Codex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "") // force the ~/.codex fallback under the temp HOME

	result := injectSystemPrompt("codex")

	if result != "codex" {
		t.Errorf("expected codex command unchanged (file seam, no flag), got %q", result)
	}
	if strings.Contains(result, "developer_instructions=") {
		t.Errorf("developer_instructions must no longer be injected, got %q", result)
	}

	skillPath := filepath.Join(home, ".codex", "skills", "agent-factory", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("expected af skill written to %s: %v", skillPath, err)
	}
	if !strings.Contains(string(content), "af sessions whoami") {
		t.Errorf("expected afUsageReference in codex SKILL.md, got %q", content)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected codex SKILL.md to carry the af-managed marker, got %q", content)
	}
}

func TestInjectSystemPrompt_CodexWithResolvedFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	result := injectSystemPrompt("codex --full-auto")

	if result != "codex --full-auto" {
		t.Errorf("expected resolved form unchanged (file seam), got %q", result)
	}
}

// Aider has no auto-discovered skills folder, so it keeps a FLAG seam: af points a
// --read at an af-owned context file carrying afUsageReference.
func TestInjectSystemPrompt_Aider(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", dir)

	result := injectSystemPrompt("aider")

	if !strings.HasPrefix(result, "aider --read ") {
		t.Errorf("expected aider to gain a --read flag, got %q", result)
	}
	readPath := filepath.Join(dir, "aider", "af-skill.md")
	if !strings.Contains(result, readPath) {
		t.Errorf("expected --read to point at %q, got %q", readPath, result)
	}
	content, err := os.ReadFile(readPath)
	if err != nil {
		t.Fatalf("expected af context file written to %s: %v", readPath, err)
	}
	if !strings.Contains(string(content), "af sessions whoami") {
		t.Errorf("expected afUsageReference in aider context file, got %q", content)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected aider context file to carry the af-managed marker, got %q", content)
	}
}

// Gemini gets a FILE seam (its user skills folder, 0.42.0+): launch command
// UNCHANGED, af skill written where gemini auto-discovers and enables it.
func TestInjectSystemPrompt_Gemini(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "") // force the ~/.gemini fallback under the temp HOME

	result := injectSystemPrompt("gemini")

	if result != "gemini" {
		t.Errorf("expected gemini command unchanged (file seam, no flag), got %q", result)
	}

	skillPath := filepath.Join(home, ".gemini", "skills", "agent-factory", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("expected af skill written to %s: %v", skillPath, err)
	}
	if !strings.Contains(string(content), "af sessions whoami") {
		t.Errorf("expected afUsageReference in gemini SKILL.md, got %q", content)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected gemini SKILL.md to carry the af-managed marker, got %q", content)
	}
}

func TestInjectSystemPrompt_Amp(t *testing.T) {
	// Amp's seam is a file, not a flag: point HOME at a temp dir so the write
	// lands there instead of the real ~/.config/amp (amp discovers skills under
	// $HOME/.config, ignoring XDG_CONFIG_HOME).
	home := t.TempDir()
	t.Setenv("HOME", home)

	result := injectSystemPrompt("amp")

	// The launch command must come back byte-identical — that is what keeps the
	// amp spawn safe (#1582), since amp dies on unknown flags (#1116/#1131).
	if result != "amp" {
		t.Errorf("expected amp command unchanged (file seam, no flag), got %q", result)
	}

	// The af skill must have been written where amp discovers it, in the
	// af-owned "agent-factory" namespace, carrying the same afUsageReference the
	// other agents receive plus the af-managed marker.
	skillPath := filepath.Join(home, ".config", "amp", "skills", "agent-factory", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("expected af skill written to %s: %v", skillPath, err)
	}
	if !strings.Contains(string(content), "af sessions whoami") {
		t.Errorf("expected afUsageReference in amp SKILL.md, got %q", content)
	}
	if !strings.HasPrefix(string(content), "---\nname: agent-factory\n") {
		t.Errorf("expected amp SKILL.md to start with name frontmatter, got %q", content)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected amp SKILL.md to carry the af-managed marker, got %q", content)
	}
}

// TestInjectSystemPrompt_Opencode pins opencode's ENV seam.
//
// opencode has no instructions flag, so af points OPENCODE_CONFIG at an af-OWNED
// config that adds an af-owned instructions file. Verified against
// 0.0.0-main-202604230742: opencode MERGES that config with the user's own (with
// `instructions` set in both, the resolved config listed both entries), and the
// af-owned instructions really do reach the model.
func TestInjectSystemPrompt_Opencode(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	home := t.TempDir()
	t.Setenv("HOME", home)

	result := injectSystemPrompt("opencode")

	// The command word must survive verbatim; only an env assignment leads it.
	if !strings.HasSuffix(result, " opencode") {
		t.Errorf("expected the opencode command preserved verbatim, got %q", result)
	}
	if !strings.HasPrefix(result, "OPENCODE_CONFIG=") {
		t.Errorf("expected OPENCODE_CONFIG env prefix, got %q", result)
	}

	configPath := filepath.Join(afHome, "opencode", "af-config.jsonc")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected af-owned opencode config at %s: %v", configPath, err)
	}
	cfg := string(raw)
	if !strings.Contains(result, configPath) {
		t.Errorf("expected OPENCODE_CONFIG to point at %q, got %q", configPath, result)
	}
	if !strings.Contains(cfg, afSkillMarker) {
		t.Errorf("expected af-managed marker in the config, got %q", cfg)
	}
	// The marker MUST be a jsonc comment, never a JSON key: opencode validates its
	// config and rejects unrecognized keys ("Unrecognized key: \"//\""), so a key
	// marker would make every opencode session start on a config error.
	if !strings.Contains(cfg, "// "+afSkillMarker) {
		t.Errorf("marker must ride as a jsonc comment (opencode rejects unknown keys), got %q", cfg)
	}

	instructionsPath := filepath.Join(afHome, "opencode", "af-skill.md")
	doc, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatalf("expected af instructions at %s: %v", instructionsPath, err)
	}
	if !strings.Contains(string(doc), "af sessions whoami") {
		t.Errorf("expected afUsageReference in opencode instructions, got %q", doc)
	}
	// Plain instructions, not a skill: opencode's `instructions` key takes markdown
	// files, so name/description frontmatter would be stray context.
	if strings.HasPrefix(string(doc), "---\n") {
		t.Errorf("opencode instructions must not carry skill frontmatter, got %q", doc)
	}
}

// TestInjectSystemPrompt_OpencodeWritesNothingOutsideAf is the whole point of the
// env seam, and the property most worth locking.
//
// af must not modify a user's GLOBAL tool configuration as a side effect of them
// creating a session. Anything written to ~/.config/opencode would outlive the
// session, survive archive and uninstall, and still load when the user ran
// `opencode` by hand tomorrow with no af involved. af's files live in af's own
// directory and are invisible to opencode unless af itself sets OPENCODE_CONFIG.
func TestInjectSystemPrompt_OpencodeWritesNothingOutsideAf(t *testing.T) {
	afHome := t.TempDir()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	injectSystemPrompt("opencode")

	// Nothing may appear in the user's opencode config dir — under either the
	// XDG path or the $HOME/.config fallback.
	for _, dir := range []string{
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".opencode"),
	} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("af wrote into the user's opencode config dir %s: %v — af must not touch a user's global tool config", dir, names)
		}
	}
	// af's own dir is where the files belong.
	if _, err := os.Stat(filepath.Join(afHome, "opencode", "af-config.jsonc")); err != nil {
		t.Errorf("expected af's opencode config under AGENT_FACTORY_HOME: %v", err)
	}
}

// TestInjectSystemPrompt_OpencodeRespectsUserConfigEnv pins that af does not fight
// a user who points opencode at their own config. Shell semantics give the LAST
// assignment precedence, so prepending af's would be silently overridden — af
// detects that and says so rather than pretending the guidance landed.
func TestInjectSystemPrompt_OpencodeRespectsUserConfigEnv(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	for _, resolved := range []string{
		"OPENCODE_CONFIG=/home/me/mine.jsonc opencode",
		// `env VAR=v <agent>` is a shape af supports elsewhere (#742), so a user's
		// OPENCODE_CONFIG can arrive behind an env wrapper too.
		"env OPENCODE_CONFIG=/home/me/mine.jsonc opencode",
	} {
		if got := injectSystemPrompt(resolved); got != resolved {
			t.Errorf("expected a user-set OPENCODE_CONFIG left untouched, got %q", got)
		}
	}

	// The mirror: OPENCODE_CONFIG appearing as an ARGUMENT is not an assignment,
	// so af must still inject. Guarding this keeps the detector from "fixing" the
	// case above with a bare strings.Contains, which would silently drop af
	// guidance for anyone whose prompt mentions the variable.
	arg := "opencode --prompt 'set OPENCODE_CONFIG=x'"
	if got := injectSystemPrompt(arg); !strings.HasPrefix(got, "OPENCODE_CONFIG=") {
		t.Errorf("OPENCODE_CONFIG as an argument is not an assignment; af should still inject, got %q", got)
	}
}

// TestInjectSystemPrompt_OpencodeDoesNotClobberAfOwnedFiles keeps the marker guard
// honest: a non-af file at af's own path is left alone and injection is skipped
// rather than pointing opencode at a file af does not own.
func TestInjectSystemPrompt_OpencodeDoesNotClobberAfOwnedFiles(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("HOME", t.TempDir())

	instructionsPath := filepath.Join(afHome, "opencode", "af-skill.md")
	if err := os.MkdirAll(filepath.Dir(instructionsPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	userContent := "# not af's file\n"
	if err := os.WriteFile(instructionsPath, []byte(userContent), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if got := injectSystemPrompt("opencode"); got != "opencode" {
		t.Errorf("expected no injection when our path holds a non-af file, got %q", got)
	}
	back, err := os.ReadFile(instructionsPath)
	if err != nil || string(back) != userContent {
		t.Errorf("non-af file must be left untouched, got %q (err %v)", back, err)
	}
}

// TestInjectSystemPrompt_ResolvedCommandMatrix pins #1116/#1131: which seam is
// used is decided by the agent the RESOLVED command actually runs — through every
// override shape (bare name, absolute path, path+flags, redirect to a different
// agent, redirect to a non-agent binary) — never by the config-name key the
// command was resolved from. Flag agents (claude → --plugin-dir, aider → --read)
// gain a flag; file-seam agents (codex, gemini, amp) come back UNCHANGED; non-agent
// binaries get nothing (the class fix: injecting a flag into e.g. bash makes it
// exit instantly and the spawn dies as an opaque timeout).
func TestInjectSystemPrompt_ResolvedCommandMatrix(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The file-seam rows write under $HOME (~/.config/amp, ~/.codex, ~/.gemini);
	// keep them off the real home, and force the HOME fallbacks. opencode's seam
	// writes under AGENT_FACTORY_HOME instead — af's own dir, never the user's
	// opencode config.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GEMINI_CLI_HOME", "")

	tests := []struct {
		name     string
		resolved string
		want     string // "" = resolved must come back unchanged (file seam / no agent)
		// envPrefix marks the one seam shape that PREPENDS rather than appends:
		// an env assignment is only honored before the command word, so opencode's
		// resolved form is preserved as a SUFFIX. Everything else appends, and the
		// #640 "original preserved verbatim as a prefix" guarantee still holds for
		// them.
		envPrefix bool
	}{
		// name→name (no override) for all supported agents.
		{name: "claude bare", resolved: "claude", want: "--plugin-dir"},
		{name: "codex bare", resolved: "codex", want: ""},
		{name: "aider bare", resolved: "aider", want: "--read"},
		{name: "gemini bare", resolved: "gemini", want: ""},
		{name: "amp bare", resolved: "amp", want: ""},
		{name: "opencode bare", resolved: "opencode", want: "OPENCODE_CONFIG=", envPrefix: true},

		// name→path and name→path+flags overrides.
		{name: "claude override path", resolved: "/opt/claude-next/bin/claude", want: "--plugin-dir"},
		{name: "claude override path with flags", resolved: "/opt/claude-next/bin/claude --model opus", want: "--plugin-dir"},
		{name: "codex override path with flags", resolved: "/usr/local/bin/codex --full-auto", want: ""},
		{name: "aider override path", resolved: "/usr/local/bin/aider --no-auto-commits", want: "--read"},
		{name: "gemini override path", resolved: "/usr/local/bin/gemini", want: ""},
		{name: "amp override path", resolved: "/home/me/.amp/bin/amp --no-ide", want: ""},
		// opencode's default install path — the common case, not an exotic one.
		{name: "opencode override path", resolved: "/home/me/.opencode/bin/opencode", want: "OPENCODE_CONFIG=", envPrefix: true},
		{name: "opencode override path with flags", resolved: "/home/me/.opencode/bin/opencode --model anthropic/claude-opus-4-5", want: "OPENCODE_CONFIG=", envPrefix: true},

		// name→other-agent: the RESOLVED agent's seam, not the key's.
		{name: "claude key resolved to codex is file seam", resolved: "codex --full-auto", want: ""},
		{name: "codex key resolved to claude gets claude flag", resolved: "/usr/bin/claude", want: "--plugin-dir"},

		// name→non-agent binary: no injection at all (#1116, #1131).
		{name: "claude key resolved to bash (#1131)", resolved: "bash", want: ""},
		{name: "claude key resolved to unknown tool (#1116)", resolved: "/usr/bin/some-other-tool --foo", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectSystemPrompt(tt.resolved)
			if tt.want == "" {
				if got != tt.resolved {
					t.Errorf("expected %q unchanged, got %q", tt.resolved, got)
				}
				return
			}
			if tt.envPrefix {
				// The command must survive VERBATIM after the assignment: an env
				// prefix that mangled the user's quoting/$VARs would be #640 again.
				if !strings.HasSuffix(got, " "+tt.resolved) {
					t.Errorf("expected resolved form preserved verbatim after the env prefix, got %q", got)
				}
				if !strings.HasPrefix(got, tt.want) {
					t.Errorf("expected %q to lead the command (env assignments only apply before it), got %q", tt.want, got)
				}
				// The agent must still be detected THROUGH the prefix, or the whole
				// per-agent machinery (resume, readiness) silently unbinds.
				if agent := tmux.DetectAgentFromCommand(got); agent != tmux.ProgramOpencode {
					t.Errorf("DetectAgentFromCommand(%q) = %q, want opencode — the env prefix broke agent detection", got, agent)
				}
				return
			}
			if !strings.HasPrefix(got, tt.resolved) {
				t.Errorf("expected resolved form preserved as prefix, got %q", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected %q injected into %q, got %q", tt.want, tt.resolved, got)
			}
			// Never more than one agent's flag: --plugin-dir and --read must
			// never both appear.
			if strings.Contains(got, "--plugin-dir") && strings.Contains(got, "--read") {
				t.Errorf("both agents' flags injected: %q", got)
			}
		})
	}
}

// Guard for #1043: the consolidated skill is minimal but must keep covering
// the entire af feature surface — future trimming must not drop capabilities.
func TestAfUsageReference_CoversFullSurface(t *testing.T) {
	required := []string{
		"af sessions whoami", "af sessions list", "af sessions get",
		"af sessions create", "af sessions send-prompt", "af sessions preview",
		"af sessions attach", "af sessions kill",
		"af sessions archive --self",
		"af sessions tab-create", "af sessions tab-delete",
		"af tasks list", "af tasks get", "af tasks add", "af tasks update",
		"af tasks trigger", "af tasks remove",
		"--cron", "--watch-cmd", "{{line}}", "--target-session",
		"af daemon install", "--repo",
		"af version", "af debug", "af upgrade", "af reset",
	}
	for _, want := range required {
		if !strings.Contains(afUsageReference, want) {
			t.Errorf("afUsageReference must document %q", want)
		}
	}
}

func TestEnsureAmpSkillDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir, err := ensureAmpSkillDir()
	if err != nil {
		t.Fatalf("ensureAmpSkillDir() failed: %v", err)
	}

	// Must land exactly where amp searches, in the af-owned namespace:
	// $HOME/.config/amp/skills/agent-factory.
	expected := filepath.Join(home, ".config", "amp", "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected amp skill dir %q, got %q", expected, skillDir)
	}

	content, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("expected SKILL.md written: %v", err)
	}
	// name + description frontmatter (amp requires both), the af-managed marker,
	// then the shared body.
	for _, want := range []string{
		"name: agent-factory",
		"description: Manage Agent Factory (af) sessions",
		afSkillMarker,
		"af sessions whoami",
		"af sessions archive --self",
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected amp SKILL.md to contain %q, got %q", want, content)
		}
	}
}

// ensureAmpSkillDir must never clobber a SKILL.md it does not own. amp's skills
// dir is the user's global amp config; a file there without the af-managed
// marker belongs to the user (or another tool) and must survive untouched
// (#1585 review, finding 1). A file WITH the marker is af-owned and regenerates.
func TestEnsureAmpSkillDir_NonDestructive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	skillDir := filepath.Join(home, ".config", "amp", "skills", "agent-factory")
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("seed mkdir: %v", err)
	}
	userSkill := "---\nname: agent-factory\ndescription: my own skill\n---\nhand-written, keep me\n"
	if err := os.WriteFile(path, []byte(userSkill), 0644); err != nil {
		t.Fatalf("seed user skill: %v", err)
	}

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir() must not error on a foreign skill: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != userSkill {
		t.Errorf("expected the user's un-marked skill left untouched, got %q", got)
	}

	// A file that DOES carry the marker is af-owned and gets regenerated in place.
	if err := os.WriteFile(path, []byte("stale\n<!-- "+afSkillMarker+" -->\n"), 0644); err != nil {
		t.Fatalf("seed af-owned skill: %v", err)
	}
	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir() on af-owned file: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != afSkillDoc {
		t.Errorf("expected af-owned skill regenerated to afSkillDoc, got %q", got)
	}
}

// ensureAmpSkillDir must resolve under $HOME/.config REGARDLESS of
// XDG_CONFIG_HOME. amp honors XDG for settings.json but NOT for skills discovery
// (verified against the amp CLI), so honoring XDG here would write the skill
// where amp never looks for a user who has XDG_CONFIG_HOME set.
func TestEnsureAmpSkillDir_IgnoresXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // a DIFFERENT dir; must be ignored

	skillDir, err := ensureAmpSkillDir()
	if err != nil {
		t.Fatalf("ensureAmpSkillDir() failed: %v", err)
	}
	expected := filepath.Join(home, ".config", "amp", "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected skill dir under HOME %q, got %q", expected, skillDir)
	}
}

func TestEnsureAmpSkillDir_Idempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir1, err := ensureAmpSkillDir()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	dir2, err := ensureAmpSkillDir()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if dir1 != dir2 {
		t.Errorf("expected same dir on repeated calls, got %q and %q", dir1, dir2)
	}
}

// Codex skills base resolves under $CODEX_HOME when set, else $HOME/.codex.
func TestEnsureCodexSkillDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	skillDir, err := ensureCodexSkillDir()
	if err != nil {
		t.Fatalf("ensureCodexSkillDir() failed: %v", err)
	}
	expected := filepath.Join(home, ".codex", "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected codex skill dir %q, got %q", expected, skillDir)
	}
	content, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("expected SKILL.md written: %v", err)
	}
	for _, want := range []string{"name: agent-factory", afSkillMarker, "af sessions whoami"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected codex SKILL.md to contain %q, got %q", want, content)
		}
	}
}

func TestEnsureCodexSkillDir_HonorsCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", codexHome)

	skillDir, err := ensureCodexSkillDir()
	if err != nil {
		t.Fatalf("ensureCodexSkillDir() failed: %v", err)
	}
	expected := filepath.Join(codexHome, "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected codex skill dir under CODEX_HOME %q, got %q", expected, skillDir)
	}
}

// Gemini skills base resolves under $GEMINI_CLI_HOME/.gemini when set, else
// $HOME/.gemini.
func TestEnsureGeminiSkillDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")

	skillDir, err := ensureGeminiSkillDir()
	if err != nil {
		t.Fatalf("ensureGeminiSkillDir() failed: %v", err)
	}
	expected := filepath.Join(home, ".gemini", "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected gemini skill dir %q, got %q", expected, skillDir)
	}
	content, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("expected SKILL.md written: %v", err)
	}
	for _, want := range []string{"name: agent-factory", afSkillMarker, "af sessions whoami"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected gemini SKILL.md to contain %q, got %q", want, content)
		}
	}
}

func TestEnsureGeminiSkillDir_HonorsGeminiCliHome(t *testing.T) {
	geminiHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GEMINI_CLI_HOME", geminiHome)

	skillDir, err := ensureGeminiSkillDir()
	if err != nil {
		t.Fatalf("ensureGeminiSkillDir() failed: %v", err)
	}
	expected := filepath.Join(geminiHome, ".gemini", "skills", "agent-factory")
	if skillDir != expected {
		t.Errorf("expected gemini skill dir under GEMINI_CLI_HOME %q, got %q", expected, skillDir)
	}
}

// The shared writer must never clobber a file it does not own — the acceptance
// non-clobber guarantee, exercised through the codex skills path (the same guard
// protects gemini, amp, and the aider context file).
func TestWriteAfMarkedFile_NonDestructive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	skillDir := filepath.Join(home, ".codex", "skills", "agent-factory")
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("seed mkdir: %v", err)
	}
	userSkill := "---\nname: agent-factory\ndescription: my own codex skill\n---\nhand-written, keep me\n"
	if err := os.WriteFile(path, []byte(userSkill), 0644); err != nil {
		t.Fatalf("seed user skill: %v", err)
	}

	if _, err := ensureCodexSkillDir(); err != nil {
		t.Fatalf("ensureCodexSkillDir() must not error on a foreign skill: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != userSkill {
		t.Errorf("expected the user's un-marked skill left untouched, got %q", got)
	}

	// A file carrying the marker is af-owned and regenerates in place.
	if err := os.WriteFile(path, []byte("stale\n<!-- "+afSkillMarker+" -->\n"), 0644); err != nil {
		t.Fatalf("seed af-owned skill: %v", err)
	}
	if _, err := ensureCodexSkillDir(); err != nil {
		t.Fatalf("ensureCodexSkillDir() on af-owned file: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != afSkillDoc {
		t.Errorf("expected af-owned skill regenerated to afSkillDoc, got %q", got)
	}
}

// The aider context file is written under the af config dir and carries the
// marker; a user's un-marked file at that path is preserved and --read is skipped.
func TestEnsureAiderReadFile(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", dir)

	path, err := ensureAiderReadFile()
	if err != nil {
		t.Fatalf("ensureAiderReadFile() failed: %v", err)
	}
	expected := filepath.Join(dir, "aider", "af-skill.md")
	if path != expected {
		t.Errorf("expected aider context file %q, got %q", expected, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected context file written: %v", err)
	}
	for _, want := range []string{afSkillMarker, "af sessions whoami", "af sessions list"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected aider context file to contain %q, got %q", want, content)
		}
	}

	// A user's un-marked file at our path is preserved, and ensureAiderReadFile
	// returns an empty path so the caller skips injecting --read.
	userFile := "my own aider read file\n"
	if err := os.WriteFile(path, []byte(userFile), 0644); err != nil {
		t.Fatalf("seed user file: %v", err)
	}
	got, err := ensureAiderReadFile()
	if err != nil {
		t.Fatalf("ensureAiderReadFile() on foreign file: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path (skip --read) for un-marked user file, got %q", got)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(back) != userFile {
		t.Errorf("expected user's aider read file untouched, got %q", back)
	}
}

func TestEnsurePluginDir(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	pluginDir, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("ensurePluginDir() failed: %v", err)
	}

	expectedDir := filepath.Join(tmpDir, "plugin")
	if pluginDir != expectedDir {
		t.Errorf("expected plugin dir %q, got %q", expectedDir, pluginDir)
	}

	// Verify plugin manifest exists
	manifestPath := filepath.Join(pluginDir, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Error("expected .claude-plugin/plugin.json manifest to exist")
	}

	commandsDir := filepath.Join(pluginDir, "commands")
	expectedFiles := []string{"af.md"}
	for _, name := range expectedFiles {
		path := filepath.Join(commandsDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected command file %s to exist", name)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("failed to read %s: %v", name, err)
			continue
		}
		if !strings.Contains(string(content), "allowed-tools") {
			t.Errorf("expected %s to contain frontmatter with allowed-tools", name)
		}
	}
}

func TestEnsurePluginDir_Idempotent(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	dir1, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	dir2, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if dir1 != dir2 {
		t.Errorf("expected same dir on repeated calls, got %q and %q", dir1, dir2)
	}
}

func TestEnsurePluginDir_PrunesStaleFiles(t *testing.T) {
	tmpDir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	pluginDir, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	commandsDir := filepath.Join(pluginDir, "commands")
	stale := filepath.Join(commandsDir, "af-removed.md")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatalf("failed to seed stale file: %v", err)
	}

	// Non-.md files and unrelated content must be left alone.
	keep := filepath.Join(commandsDir, "README.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0644); err != nil {
		t.Fatalf("failed to seed keep file: %v", err)
	}

	if _, err := ensurePluginDir(); err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("expected stale file %s to be pruned, got err=%v", stale, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("expected non-.md file %s to survive prune: %v", keep, err)
	}

	for name := range pluginCommands {
		if _, err := os.Stat(filepath.Join(commandsDir, name)); err != nil {
			t.Errorf("expected %s to still exist after prune: %v", name, err)
		}
	}
}
