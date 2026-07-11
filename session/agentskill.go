package session

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// afSkillDirName is the directory- and skill-name every agent that auto-discovers
// a SKILL.md (amp, codex, gemini) reads under its per-agent skills base. It is
// deliberately "agent-factory", NOT "af": those skills dirs are the user's GLOBAL
// per-agent config, shared with skills they install by hand, and "af" is exactly
// the short name a user or another tool would pick for their own af helper skill.
// Namespacing to "agent-factory" makes the directory unambiguously af-owned so we
// never collide with a user's "af" skill (#1585 review).
const afSkillDirName = "agent-factory"

// afSkillMarker stamps every file we write so a later launch can tell a file it
// owns (safe to regenerate) from a user's/tool's own file that happens to sit at
// our path (never clobber). See writeAfMarkedFile.
const afSkillMarker = "managed by agent-factory (af): regenerated on each af session launch; do not edit"

// afSkillDoc is the SKILL.md served to every agent that auto-discovers a
// name+description skill (amp, codex, gemini). Its body is afUsageReference
// (systemprompt.go) — the SAME text Claude receives via the plugin skill and
// aider via --read — so the agents' af knowledge cannot drift (#1043). All three
// CLIs require only name + description frontmatter (verified against `amp skill
// list`, the codex skill-creator, and the gemini skills docs); the description is
// what each surfaces for lazy activation, so it mirrors the Claude plugin skill's
// description. The afSkillMarker rides in an HTML comment just below the
// frontmatter — invisible in rendered markdown, and the token writeAfMarkedFile
// checks for ownership.
var afSkillDoc = "---\n" +
	"name: " + afSkillDirName + "\n" +
	"description: Manage Agent Factory (af) sessions, tabs, scheduled tasks, and the daemon via the af CLI\n" +
	"---\n" +
	"<!-- " + afSkillMarker + " -->\n" +
	"\n" +
	afUsageReference + "\n"

// writeAfMarkedFile writes content to path NON-DESTRUCTIVELY and reports whether it
// wrote. It only (over)writes a file that carries afSkillMarker (a file we own); a
// file already at path WITHOUT the marker belongs to the user (or another tool)
// and is left untouched (wrote=false, no error) — we never silently destroy it.
// The parent directory is created as needed. An af-owned file is rewritten
// unconditionally, so edits to afUsageReference or the description propagate on the
// next session launch.
func writeAfMarkedFile(path, content string) (bool, error) {
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Contains(existing, []byte(afSkillMarker)) {
			return false, nil
		}
	} else if !os.IsNotExist(readErr) {
		return false, readErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if err := config.AtomicWriteFile(path, []byte(content), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// ensureAfSkillDir writes the af-managed "agent-factory" skill into the given
// per-agent skills base directory and returns the skill directory path. The agent
// auto-discovers skills there with NO command-line flag, so this injects
// afUsageReference WITHOUT touching the launch command — the spawn stays
// byte-identical to the working no-injection launch. That is the whole point of a
// file seam over a flag: an unknown flag would kill the spawn as an opaque
// readiness timeout (the #1043/#1116/#1131 class). A write failure just means the
// agent loses the af guidance for this launch; callers log and carry on. The write
// is non-destructive (writeAfMarkedFile): a user's own un-marked SKILL.md at our
// path is left untouched and logged.
func ensureAfSkillDir(base string) (string, error) {
	skillDir := filepath.Join(base, afSkillDirName)
	path := filepath.Join(skillDir, "SKILL.md")
	wrote, err := writeAfMarkedFile(path, afSkillDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		log.WarningLog.Printf("af skill: %s exists but is not af-managed; leaving it untouched (af guidance not injected)", path)
	}
	return skillDir, nil
}

// codexSkillsBaseDir returns codex's skills-discovery base: $CODEX_HOME/skills, or
// $HOME/.codex/skills when CODEX_HOME is unset. Verified against codex-cli 0.144.1,
// whose skill-creator documents placing a skill in "$CODEX_HOME/skills (or
// ~/.codex/skills when CODEX_HOME is unset) so Codex can discover it
// automatically". This retires the #1043 wall: codex 0.144.1 auto-discovers user
// skills dropped here (its own built-ins live under a sibling .system/ dir), so af
// no longer needs to stuff afUsageReference into -c developer_instructions.
func codexSkillsBaseDir() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "skills"), nil
}

// ensureCodexSkillDir writes the af skill into codex's skills base. See
// ensureAfSkillDir and codexSkillsBaseDir.
func ensureCodexSkillDir() (string, error) {
	base, err := codexSkillsBaseDir()
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(base)
}

// geminiSkillsBaseDir returns gemini's USER-scope skills base:
// $GEMINI_CLI_HOME/.gemini/skills, or $HOME/.gemini/skills when GEMINI_CLI_HOME is
// unset. Verified against gemini-cli 0.42.0: user skills are discovered under
// ~/.gemini/skills, and GEMINI_CLI_HOME relocates the .gemini dir (it creates a
// .gemini folder inside the given path — enterprise docs). Gemini scans this dir at
// session start and ENABLES a dropped skill automatically (verified: `gemini skills
// list --all` reports agent-factory [Enabled]).
func geminiSkillsBaseDir() (string, error) {
	if geminiHome := os.Getenv("GEMINI_CLI_HOME"); geminiHome != "" {
		return filepath.Join(geminiHome, ".gemini", "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "skills"), nil
}

// ensureGeminiSkillDir writes the af skill into gemini's user-scope skills base.
// See ensureAfSkillDir and geminiSkillsBaseDir.
func ensureGeminiSkillDir() (string, error) {
	base, err := geminiSkillsBaseDir()
	if err != nil {
		return "", err
	}
	return ensureAfSkillDir(base)
}

// aiderReadDoc is the read-only context file aider loads via --read. Aider has NO
// auto-discovered global skills/instructions directory (its only persistent
// global-instructions seams are the --read flag and the user-owned .aider.conf.yml
// "read:" list; verified against aider 0.86.2). So — unlike amp/codex/gemini — af
// injects the skill by pointing a --read flag at this af-owned file, exactly as
// claude uses --plugin-dir. It is plain markdown (aider adds it to the chat as a
// read-only file), not a SKILL.md, so it needs no frontmatter; the afSkillMarker
// rides in a leading HTML comment for ownership.
var aiderReadDoc = "<!-- " + afSkillMarker + " -->\n\n" + afUsageReference + "\n"

// aiderReadFilePath returns the path of the af-owned aider context file, under the
// af config dir (config.GetConfigDir()). It is NOT written into the user's worktree
// (that would pollute git status) nor into .aider.conf.yml (that is the user's
// file). The path is af-exclusive, so --read can point at it safely.
func aiderReadFilePath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "aider", "af-skill.md"), nil
}

// ensureAiderReadFile writes aiderReadDoc to the af-owned aider context file and
// returns its path, non-destructively. An empty path (with nil error) means the
// file exists but is NOT af-managed — the caller must then skip injecting --read
// rather than point it at a file we do not own. A non-af file at our own path is
// nearly impossible (af owns the dir), but the marker guard is honored uniformly.
func ensureAiderReadFile() (string, error) {
	path, err := aiderReadFilePath()
	if err != nil {
		return "", err
	}
	wrote, err := writeAfMarkedFile(path, aiderReadDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		log.WarningLog.Printf("af skill: %s exists but is not af-managed; not injecting --read for aider", path)
		return "", nil
	}
	return path, nil
}
