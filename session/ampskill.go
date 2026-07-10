package session

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// ampSkillDirName is the directory- and skill-name amp discovers under its home
// skills path. It is deliberately "agent-factory", NOT "af": amp's skills dir is
// the user's GLOBAL amp config, shared with skills they install by hand, and "af"
// is exactly the short name a user or another tool would pick for their own af
// helper skill. Namespacing to "agent-factory" makes the directory unambiguously
// af-owned so we never collide with a user's "af" skill (#1585 review).
const ampSkillDirName = "agent-factory"

// ampSkillMarker stamps every SKILL.md we write so a later launch can tell a
// file it owns (safe to regenerate) from a user's/tool's own skill that happens
// to sit at our path (never clobber). See ensureAmpSkillDir.
const ampSkillMarker = "managed by agent-factory (af): regenerated on each af session launch; do not edit"

// ampSkillDoc is the SKILL.md served to amp. Its body is afUsageReference
// (systemprompt.go) — the SAME text Claude receives via the plugin skill and
// Codex via developer_instructions — so the three agents' af knowledge cannot
// drift (#1043). Amp requires only name + description frontmatter for a skill
// (verified against `amp skill list`); the description is what amp surfaces for
// lazy activation, so it mirrors the Claude plugin skill's description. The
// ampSkillMarker rides in an HTML comment just below the frontmatter — invisible
// in rendered markdown, and the token ensureAmpSkillDir checks for ownership.
var ampSkillDoc = "---\n" +
	"name: " + ampSkillDirName + "\n" +
	"description: Manage Agent Factory (af) sessions, tabs, scheduled tasks, and the daemon via the af CLI\n" +
	"---\n" +
	"<!-- " + ampSkillMarker + " -->\n" +
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

// ensureAmpSkillDir writes the af-managed "agent-factory" skill into amp's home
// skills directory and returns the skill directory path. Amp auto-discovers
// skills there with NO command-line flag (amp has no --plugin-dir /
// developer_instructions equivalent), so this is how afUsageReference reaches
// amp sessions WITHOUT touching the launch command — the amp spawn stays
// byte-identical to the no-injection launch that already reaches ready, which is
// the whole point of choosing a file seam over a flag for amp (#1582; the
// unknown-flag-kills-spawn class is #1043/#1116/#1131).
//
// Called on every amp-based session launch (see injectSystemPrompt). It writes
// into the user's GLOBAL amp config, so the write is NON-DESTRUCTIVE: it only
// (over)writes a SKILL.md that carries ampSkillMarker (a file we own). If a
// SKILL.md already sits at our path WITHOUT the marker, it is a user's/tool's own
// skill and is left untouched (logged) — we never silently destroy it. An
// af-owned file is rewritten unconditionally, so edits to afUsageReference or the
// description still propagate on the next amp session start.
func ensureAmpSkillDir() (string, error) {
	base, err := ampSkillsBaseDir()
	if err != nil {
		return "", err
	}
	skillDir := filepath.Join(base, ampSkillDirName)
	path := filepath.Join(skillDir, "SKILL.md")

	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Contains(existing, []byte(ampSkillMarker)) {
			log.WarningLog.Printf("amp af skill: %s exists but is not af-managed; leaving it untouched (af guidance not injected for amp)", path)
			return skillDir, nil
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", err
	}
	if err := config.AtomicWriteFile(path, []byte(ampSkillDoc), 0644); err != nil {
		return "", err
	}
	return skillDir, nil
}
