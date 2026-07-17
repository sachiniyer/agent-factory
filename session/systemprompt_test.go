package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
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

// TestInjectSystemPrompt_Opencode pins opencode's file seam. The launch command
// must come back byte-identical: opencode's TUI rejects unknown flags by printing
// its help and exiting (verified — `opencode --dangerously-skip-permissions` and
// `opencode --bogus-flag` produce identical help output and never start the TUI),
// so any injected flag would kill the spawn as an opaque readiness timeout
// (#1043/#1116/#1131).
func TestInjectSystemPrompt_Opencode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Explicitly clear XDG_CONFIG_HOME so this case exercises the $HOME/.config
	// fallback — and, just as importantly, so a dev box that happens to export
	// XDG_CONFIG_HOME cannot make this test write into the real user's config.
	t.Setenv("XDG_CONFIG_HOME", "")

	result := injectSystemPrompt("opencode")

	if result != "opencode" {
		t.Errorf("expected opencode command unchanged (file seam, no flag), got %q", result)
	}

	agentsPath := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	content, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("expected af instructions written to %s: %v", agentsPath, err)
	}
	if !strings.Contains(string(content), "af sessions whoami") {
		t.Errorf("expected afUsageReference in opencode AGENTS.md, got %q", content)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected opencode AGENTS.md to carry the af-managed marker, got %q", content)
	}
	// AGENTS.md is plain instructions, NOT a name+description skill: opencode
	// discovers the file itself rather than a skill registry, so skill frontmatter
	// would just be stray text in the model's context.
	if strings.HasPrefix(string(content), "---\n") {
		t.Errorf("opencode AGENTS.md must not carry skill frontmatter, got %q", content)
	}
}

// TestInjectSystemPrompt_OpencodeHonorsXDGConfigHome pins the one place opencode's
// seam deliberately DIVERGES from amp's.
//
// ampSkillsBaseDir ignores XDG_CONFIG_HOME because amp itself ignores it. opencode
// does NOT: it resolves its config dir through XDG_CONFIG_HOME, verified by
// experiment — with a marker file at BOTH $HOME/.config/opencode/AGENTS.md and
// $XDG_CONFIG_HOME/opencode/AGENTS.md, `opencode run` reported the XDG one. So
// hardcoding $HOME/.config here would write af guidance where opencode never looks
// for every user who sets XDG_CONFIG_HOME — a silent miss with no error.
func TestInjectSystemPrompt_OpencodeHonorsXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if result := injectSystemPrompt("opencode"); result != "opencode" {
		t.Errorf("expected opencode command unchanged, got %q", result)
	}

	xdgPath := filepath.Join(xdg, "opencode", "AGENTS.md")
	if _, err := os.ReadFile(xdgPath); err != nil {
		t.Fatalf("expected af instructions at XDG_CONFIG_HOME path %s: %v", xdgPath, err)
	}
	// And NOT at the $HOME fallback, which opencode would not read in this config.
	homePath := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Errorf("must not write to %s when XDG_CONFIG_HOME is set — opencode reads the XDG path", homePath)
	}
}

// TestInjectSystemPrompt_OpencodeDoesNotClobberUserAgentsFile pins the caveat that
// makes opencode's seam riskier than every other agent's.
//
// amp/codex/gemini get an af-exclusive "agent-factory" SKILL.md subdirectory and
// aider gets a file under af's own config dir — nobody else writes there. But
// ~/.config/opencode/AGENTS.md is opencode's ONE global instructions file, and it
// is exactly where a user keeps their own standing instructions. Silently
// overwriting it would destroy user content af never owned.
//
// The contract: a file without the af marker is left byte-for-byte untouched, af
// guidance is simply not injected, and the launch still proceeds normally.
func TestInjectSystemPrompt_OpencodeDoesNotClobberUserAgentsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	agentsPath := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(agentsPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	userContent := "# My own instructions\nAlways write tests first.\n"
	if err := os.WriteFile(agentsPath, []byte(userContent), 0644); err != nil {
		t.Fatalf("seed user file: %v", err)
	}

	// The spawn must still proceed unchanged; losing af guidance is not fatal.
	if result := injectSystemPrompt("opencode"); result != "opencode" {
		t.Errorf("expected opencode command unchanged, got %q", result)
	}

	got, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != userContent {
		t.Errorf("af overwrote the user's own AGENTS.md.\n got: %q\nwant: %q", got, userContent)
	}
}

// TestInjectSystemPrompt_OpencodeRewritesItsOwnFile is the other half of the
// non-clobber contract: a file af DOES own must be regenerated, so edits to
// afUsageReference reach opencode on the next launch instead of going stale.
func TestInjectSystemPrompt_OpencodeRewritesItsOwnFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	agentsPath := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(agentsPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := "<!-- " + afSkillMarker + " -->\n\nstale af content from an older version\n"
	if err := os.WriteFile(agentsPath, []byte(stale), 0644); err != nil {
		t.Fatalf("seed af-owned file: %v", err)
	}

	injectSystemPrompt("opencode")

	got, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(got), "stale af content") {
		t.Error("af-owned AGENTS.md must be regenerated, not left stale")
	}
	if !strings.Contains(string(got), "af sessions whoami") {
		t.Errorf("regenerated AGENTS.md must carry afUsageReference, got %q", got)
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
	// The file-seam rows write under $HOME (~/.config/amp, ~/.codex, ~/.gemini,
	// ~/.config/opencode); keep them off the real home, and force the HOME
	// fallbacks. XDG_CONFIG_HOME must be cleared too: opencode's seam honors it
	// (unlike amp's), so a dev box exporting it would send this test's writes into
	// the real user config.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	tests := []struct {
		name     string
		resolved string
		want     string // "" = resolved must come back unchanged (file seam / no agent)
	}{
		// name→name (no override) for all supported agents.
		{"claude bare", "claude", "--plugin-dir"},
		{"codex bare", "codex", ""},
		{"aider bare", "aider", "--read"},
		{"gemini bare", "gemini", ""},
		{"amp bare", "amp", ""},
		{"opencode bare", "opencode", ""},

		// name→path and name→path+flags overrides.
		{"claude override path", "/opt/claude-next/bin/claude", "--plugin-dir"},
		{"claude override path with flags", "/opt/claude-next/bin/claude --model opus", "--plugin-dir"},
		{"codex override path with flags", "/usr/local/bin/codex --full-auto", ""},
		{"aider override path", "/usr/local/bin/aider --no-auto-commits", "--read"},
		{"gemini override path", "/usr/local/bin/gemini", ""},
		{"amp override path", "/home/me/.amp/bin/amp --no-ide", ""},
		// opencode's default install path — the common case, not an exotic one.
		{"opencode override path", "/home/me/.opencode/bin/opencode", ""},
		{"opencode override path with flags", "/home/me/.opencode/bin/opencode --model anthropic/claude-opus-4-5", ""},

		// name→other-agent: the RESOLVED agent's seam, not the key's.
		{"claude key resolved to codex is file seam", "codex --full-auto", ""},
		{"codex key resolved to claude gets claude flag", "/usr/bin/claude", "--plugin-dir"},

		// name→non-agent binary: no injection at all (#1116, #1131).
		{"claude key resolved to bash (#1131)", "bash", ""},
		{"claude key resolved to unknown tool (#1116)", "/usr/bin/some-other-tool --foo", ""},
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
