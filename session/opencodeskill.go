package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
)

// opencodeAgentsDoc is the af guidance written to opencode's global instructions
// file. opencode reads AGENTS.md as PLAIN instructions, not as a name+description
// skill, so this carries no frontmatter — the same shape as aiderReadDoc rather
// than afSkillDoc. The body is afUsageReference, so opencode's af knowledge cannot
// drift from the other agents' (#1043).
var opencodeAgentsDoc = "<!-- " + afSkillMarker + " -->\n\n" + afUsageReference + "\n"

// opencodeAgentsFilePath returns opencode's global instructions file:
// $XDG_CONFIG_HOME/opencode/AGENTS.md, falling back to $HOME/.config/opencode
// when XDG_CONFIG_HOME is unset.
//
// opencode auto-discovers this file with NO flag and NO project file, verified by
// experiment against 0.0.0-main-202604230742: a marker phrase written here came
// back verbatim from `opencode run "what is the secret marker phrase?"` under a
// throwaway HOME.
//
// XDG_CONFIG_HOME is honored here, deliberately UNLIKE ampSkillsBaseDir (which
// ignores it because amp itself does). opencode resolves its config dir through
// XDG_CONFIG_HOME, so hardcoding $HOME/.config would write where opencode never
// looks for any user who sets it — a silent miss. os.UserConfigDir() is NOT used:
// it returns ~/Library/Application Support on macOS, whereas opencode uses
// ~/.config/opencode on every platform.
func opencodeAgentsFilePath() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "opencode", "AGENTS.md"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "AGENTS.md"), nil
}

// ensureOpencodeAgentsFile writes the af guidance into opencode's global
// instructions file and returns its path. opencode auto-discovers it with no
// command-line flag, so the launch command stays byte-identical — the file seam
// the #1043/#1116/#1131 unknown-flag-kills-spawn rule calls for.
//
// CAVEAT — this path is SHARED and USER-OWNED, unlike every other agent's seam.
// amp/codex/gemini get an af-exclusive "agent-factory" SKILL.md subdirectory and
// aider gets a file under af's own config dir; nobody else writes there. But
// ~/.config/opencode/AGENTS.md is opencode's ONE global instructions file, and it
// is exactly where a user keeps their own standing instructions. A collision here
// is therefore far more likely than for any other agent.
//
// writeAfMarkedFile is what makes that safe: a file at this path WITHOUT the af
// marker belongs to the user and is left completely untouched (wrote=false), so we
// never clobber their instructions. The cost of that choice is that af guidance is
// then not injected for opencode at all — hence the warning, matching how the
// other agents report the same condition. We do NOT merge into or append to the
// user's file: appending would make af a co-author of a file it does not own and
// leave residue af cannot cleanly remove later.
func ensureOpencodeAgentsFile() (string, error) {
	path, err := opencodeAgentsFilePath()
	if err != nil {
		return "", err
	}
	wrote, err := writeAfMarkedFile(path, opencodeAgentsDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		log.WarningLog.Printf("af skill: %s exists but is not af-managed; leaving it untouched (af guidance not injected for opencode)", path)
	}
	return path, nil
}
