package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// ampSkillName is the directory- and skill-name amp discovers under its home
// skills path.
const ampSkillName = "af"

// ampSkillDoc is the SKILL.md served to amp. Its body is afUsageReference
// (systemprompt.go) — the SAME text Claude receives via the plugin skill and
// Codex via developer_instructions — so the three agents' af knowledge cannot
// drift (#1043). Amp requires only name + description frontmatter for a skill
// (verified against `amp skill list`); the description is what amp surfaces for
// lazy activation, so it must mirror the Claude plugin skill's description.
var ampSkillDoc = "---\n" +
	"name: " + ampSkillName + "\n" +
	"description: Manage Agent Factory (af) sessions, tabs, scheduled tasks, and the daemon via the af CLI\n" +
	"---\n" +
	"\n" +
	afUsageReference + "\n"

// ampSkillsBaseDir returns amp's home-level skills directory: $HOME/.config/amp/skills.
//
// This matches amp's ACTUAL skills-discovery base, which was verified empirically
// against the amp CLI (v0.0.1783687353): amp reads skills from $HOME/.config/amp/skills
// and IGNORES $XDG_CONFIG_HOME for skills, even though it honors XDG_CONFIG_HOME
// for its settings.json. So we deliberately do NOT consult XDG_CONFIG_HOME here —
// a user with XDG_CONFIG_HOME set to a non-default path would otherwise get the
// skill written where amp never looks, a silent miss. We also do NOT use
// os.UserConfigDir(): on macOS it returns ~/Library/Application Support, whereas
// amp uses ~/.config/amp on every platform.
func ampSkillsBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "amp", "skills"), nil
}

// ensureAmpSkillDir writes the "af" skill into amp's home skills directory and
// returns the skill directory path. Amp auto-discovers skills there with NO
// command-line flag (amp has no --plugin-dir / developer_instructions
// equivalent), so this is how afUsageReference reaches amp sessions WITHOUT
// touching the launch command — the amp spawn stays byte-identical to the
// no-injection launch that already reaches ready, which is the whole point of
// choosing a file seam over a flag for amp (#1582; the unknown-flag-kills-spawn
// class is #1043/#1116/#1131).
//
// Called on every amp-based session launch (see injectSystemPrompt). Like
// ensurePluginDir it overwrites unconditionally, so an edit to afUsageReference
// or the skill description propagates on the next amp session start.
func ensureAmpSkillDir() (string, error) {
	base, err := ampSkillsBaseDir()
	if err != nil {
		return "", err
	}
	skillDir := filepath.Join(base, ampSkillName)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := config.AtomicWriteFile(path, []byte(ampSkillDoc), 0644); err != nil {
		return "", err
	}
	return skillDir, nil
}
