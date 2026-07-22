package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// #1977: af must not edit the user's GLOBAL per-agent config directories
// (codex's $CODEX_HOME/skills, gemini's ~/.gemini/skills, amp's
// ~/.config/amp/skills) as a side effect of creating a session. Those files
// reach outside af and outlive it — they survive archiving the session and
// uninstalling af, and still load when the user runs that agent by hand.
//
// The gate lives at ensureAfSkillDir, the one choke point all three share.

// agentHome points HOME at a temp dir so every global agent skills base
// resolves inside the test, and returns it. The af home is sandboxed
// package-wide by TestMain, and each test that needs a specific af CONFIG
// overrides AGENT_FACTORY_HOME itself via grantGlobalAgentSkills /
// declineGlobalAgentSkills.
func agentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeAfConfig gives this test its own af home holding a config with
// global_agent_skills set as given.
func writeAfConfig(t *testing.T, enabled bool) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.GlobalAgentSkills = enabled
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("save af config: %v", err)
	}
}

// grantGlobalAgentSkills opts this test's af home into af managing the global
// agent skills directories — the consent the feature now requires (#1977).
// Tests that assert on WHERE and HOW the skill file is written need it; without
// it the default config declines and nothing is written at all.
func grantGlobalAgentSkills(t *testing.T) {
	t.Helper()
	writeAfConfig(t, true)
}

// ampSkillPath is where af would write amp's copy under home.
func ampSkillPath(home string) string {
	return filepath.Join(home, ".config", "amp", "skills", "agent-factory", "SKILL.md")
}

// seedAfOwnedSkill writes a marker-stamped SKILL.md exactly as a PRIOR af
// version would have left it, and returns its path.
func seedAfOwnedSkill(t *testing.T, home string) string {
	t.Helper()
	path := ampSkillPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("seed mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(afSkillDoc), 0644); err != nil {
		t.Fatalf("seed af-owned skill: %v", err)
	}
	return path
}

// TestGlobalAgentSkills_DefaultWritesNothingIntoTheUsersConfig is the #1977
// regression. On a default install — no config key set, which is every install
// — creating a session for codex, gemini or amp wrote a SKILL.md into the
// user's own agent config directory. Nothing may land there now.
func TestGlobalAgentSkills_DefaultWritesNothingIntoTheUsersConfig(t *testing.T) {
	cases := []struct {
		agent string
		run   func() (string, error)
		path  func(home string) string
	}{
		{"amp", ensureAmpSkillDir, ampSkillPath},
		{"codex", ensureCodexSkillDir, func(h string) string {
			return filepath.Join(h, ".codex", "skills", "agent-factory", "SKILL.md")
		}},
		{"gemini", ensureGeminiSkillDir, func(h string) string {
			return filepath.Join(h, ".gemini", "skills", "agent-factory", "SKILL.md")
		}},
		{"devin", ensureDevinSkillDir, func(h string) string {
			return filepath.Join(h, ".config", "devin", "skills", "agent-factory", "SKILL.md")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			home := agentHome(t)
			writeAfConfig(t, false) // the default; written explicitly so the intent is visible

			dir, err := tc.run()
			if err != nil {
				t.Fatalf("a declined write must not be an error: %v", err)
			}
			if dir != "" {
				t.Errorf("expected no skill dir reported when af may not write one, got %q", dir)
			}
			if _, err := os.Stat(tc.path(home)); !os.IsNotExist(err) {
				t.Errorf("af wrote into the user's global %s config at %s (stat err: %v)", tc.agent, tc.path(home), err)
			}
		})
	}
}

// TestGlobalAgentSkills_DefaultConfigDeclines pins the DEFAULT itself, not just
// the behavior under an explicitly-false config: a brand-new af home with no
// config file at all must still decline. A default that flipped to true would
// reintroduce the whole issue while every test above still passed.
func TestGlobalAgentSkills_DefaultConfigDeclines(t *testing.T) {
	if config.DefaultConfig().GlobalAgentSkills {
		t.Fatal("global_agent_skills must default to false: creating a session is not consent to edit the user's global agent config")
	}

	home := agentHome(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir()) // empty af home, no config written

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir: %v", err)
	}
	if _, err := os.Stat(ampSkillPath(home)); !os.IsNotExist(err) {
		t.Errorf("a fresh af home with no config must not write into the user's amp config")
	}
}

// TestGlobalAgentSkills_GrantedStillWrites is the other half: the capability is
// not removed, only gated. A user who asks for it gets exactly what af wrote
// before.
func TestGlobalAgentSkills_GrantedStillWrites(t *testing.T) {
	home := agentHome(t)
	grantGlobalAgentSkills(t)

	dir, err := ensureAmpSkillDir()
	if err != nil {
		t.Fatalf("ensureAmpSkillDir: %v", err)
	}
	if want := filepath.Dir(ampSkillPath(home)); dir != want {
		t.Errorf("skill dir = %q, want %q", dir, want)
	}
	content, err := os.ReadFile(ampSkillPath(home))
	if err != nil {
		t.Fatalf("expected SKILL.md written under consent: %v", err)
	}
	if !strings.Contains(string(content), afSkillMarker) {
		t.Errorf("expected the af-managed marker, got %q", content)
	}
}

// TestGlobalAgentSkills_DeclinedRemovesAfsOwnFile covers the issue's first
// objection — "it outlives af". A user upgrading past this change still has the
// file a PRIOR af version wrote; af removes its own, so the edit does not
// survive the decision not to make it.
func TestGlobalAgentSkills_DeclinedRemovesAfsOwnFile(t *testing.T) {
	home := agentHome(t)
	path := seedAfOwnedSkill(t, home)
	writeAfConfig(t, false)

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("af's own file must not outlive af's decision not to write it (stat err: %v)", err)
	}
	// The af-namespaced directory goes too, once empty.
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the emptied agent-factory/ dir should be removed as well (stat err: %v)", err)
	}
}

// TestGlobalAgentSkills_DeclinedNeverTouchesTheUsersOwnFile is the destruction
// guard. Removal is gated on af's MARKER, a positive observed fact — a file at
// af's path without it belongs to the user (or another tool) and must survive,
// exactly as writeAfMarkedFile refuses to overwrite it.
func TestGlobalAgentSkills_DeclinedNeverTouchesTheUsersOwnFile(t *testing.T) {
	home := agentHome(t)
	path := ampSkillPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("seed mkdir: %v", err)
	}
	const userSkill = "---\nname: agent-factory\ndescription: my own skill\n---\nhand-written, keep me\n"
	if err := os.WriteFile(path, []byte(userSkill), 0644); err != nil {
		t.Fatalf("seed user skill: %v", err)
	}
	writeAfConfig(t, false)

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the user's own file must survive: %v", err)
	}
	if string(got) != userSkill {
		t.Errorf("the user's un-marked skill was modified: %q", got)
	}
}

// TestGlobalAgentSkills_DeclinedKeepsSiblingFiles proves the directory removal
// cannot take anything with it: os.Remove (not RemoveAll) means the kernel
// itself refuses a non-empty directory, so we never have to predict what else
// the user put in there.
func TestGlobalAgentSkills_DeclinedKeepsSiblingFiles(t *testing.T) {
	home := agentHome(t)
	path := seedAfOwnedSkill(t, home)
	sibling := filepath.Join(filepath.Dir(path), "notes.md")
	if err := os.WriteFile(sibling, []byte("the user's notes\n"), 0644); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	writeAfConfig(t, false)

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("ensureAmpSkillDir: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("af's own SKILL.md should still be removed")
	}
	if _, err := os.ReadFile(sibling); err != nil {
		t.Errorf("a file the user placed beside af's must survive: %v", err)
	}
}

// TestGlobalAgentSkills_UnreadableConfigTouchesNothing is the three-value rule
// (#1933). A config af could not read is UNKNOWN, not "declined": af neither
// writes the skill nor deletes the existing one. Collapsing unknown into "no"
// would let an unparseable config authorize a deletion — the fabricated-negative
// shape behind #1969 and #2011.
func TestGlobalAgentSkills_UnreadableConfigTouchesNothing(t *testing.T) {
	home := agentHome(t)
	path := seedAfOwnedSkill(t, home)

	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	if err := os.WriteFile(filepath.Join(afHome, "config.toml"), []byte("this is = not [valid toml\n"), 0644); err != nil {
		t.Fatalf("seed broken config: %v", err)
	}

	if _, err := ensureAmpSkillDir(); err != nil {
		t.Fatalf("an unreadable config must not be fatal to the launch: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("af must not delete anything on the strength of a config it could not read: %v", err)
	}
}

// TestGlobalAgentSkills_AfOwnedSeamsAreUnaffected is the adjacent-site audit as
// a test. The gate is specifically about the USER'S directories; claude
// (--plugin-dir), aider (--read) and opencode (OPENCODE_CONFIG) all point at
// af-OWNED files under af's own config dir, so they keep injecting af guidance
// unconditionally and nothing of af's escapes into the user's config on those
// paths.
func TestGlobalAgentSkills_AfOwnedSeamsAreUnaffected(t *testing.T) {
	agentHome(t)
	writeAfConfig(t, false)

	if got := injectSystemPrompt("claude"); !strings.Contains(got, "--plugin-dir") {
		t.Errorf("claude's af-owned plugin seam must be unaffected by the global-config gate, got %q", got)
	}
	if got := injectSystemPrompt("aider"); !strings.Contains(got, "--read") {
		t.Errorf("aider's af-owned --read seam must be unaffected by the global-config gate, got %q", got)
	}
	if got := injectSystemPrompt("opencode"); !strings.Contains(got, "OPENCODE_CONFIG=") {
		t.Errorf("opencode's af-owned config seam must be unaffected by the global-config gate, got %q", got)
	}
}
