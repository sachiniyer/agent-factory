package session

import (
	"os"
	"path/filepath"
)

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
// skills directory and returns the skill directory path. Amp auto-discovers skills
// there with NO command-line flag (amp has no --plugin-dir / developer_instructions
// equivalent), so this is how afUsageReference reaches amp sessions WITHOUT touching
// the launch command — see ensureAfSkillDir for the shared, non-destructive writer
// and the file-seam rationale (#1582; the unknown-flag-kills-spawn class is
// #1043/#1116/#1131).
func ensureAmpSkillDir() (string, error) {
	base, err := ampSkillsBaseDir()
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(base)
}
