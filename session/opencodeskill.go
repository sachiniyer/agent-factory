package session

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// opencodeInstructionsDoc is the af guidance opencode loads as plain instructions.
// It carries no frontmatter: opencode's `instructions` config key takes plain
// markdown files, not name+description skills, so skill frontmatter would just be
// stray text in the model's context. Body is afUsageReference, so opencode's af
// knowledge cannot drift from the other agents' (#1043).
var opencodeInstructionsDoc = "<!-- " + afSkillMarker + " -->\n\n" + afUsageReference + "\n"

// opencodeAfConfigDoc is the af-owned opencode config: a minimal, schema-valid
// file whose only job is to point opencode's `instructions` key at
// opencodeInstructionsDoc.
//
// It is .jsonc, not .json, for a verified reason: opencode validates its config
// against a schema and REJECTS unrecognized keys ("Configuration is invalid …
// Unrecognized key: \"//\""), so the af-managed marker cannot ride as a JSON key
// the way it does in every other agent's file. opencode does parse jsonc comments,
// so the marker lives in a leading // comment and writeAfMarkedFile's ownership
// check still works. Both verified against 0.0.0-main-202604230742.
func opencodeAfConfigDoc(instructionsPath string) string {
	// json.Marshal rather than hand-rolled quoting: the af config dir is
	// user-controlled via AGENT_FACTORY_HOME, so the path can carry quotes or
	// backslashes that must be escaped correctly or the config fails to parse.
	quoted, err := json.Marshal(instructionsPath)
	if err != nil {
		// A Go string always marshals; this cannot fail in practice.
		quoted = []byte(`""`)
	}
	return "{\n" +
		"  // " + afSkillMarker + "\n" +
		"  \"$schema\": \"https://opencode.ai/config.json\",\n" +
		"  \"instructions\": [" + string(quoted) + "]\n" +
		"}\n"
}

// opencodeAfDir returns af's own opencode directory, under the af config dir.
//
// Deliberately NOT ~/.config/opencode: that is the USER's global opencode config,
// and af has no business writing there as a side effect of creating a session.
// Anything af wrote there would outlive the session, survive archive/uninstall, and
// still be loaded when the user ran `opencode` by hand tomorrow with no af involved.
// Keeping af's files in af's own directory means they vanish with af and are
// invisible to opencode unless af itself points at them (see injectSystemPrompt's
// OPENCODE_CONFIG). This mirrors the claude (--plugin-dir) and aider (--read)
// seams, which are af-owned for the same reason.
func opencodeAfDir() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "opencode"), nil
}

// ensureOpencodeAfConfig writes af's opencode instructions + the af-owned config
// that points at them, and returns the CONFIG path for OPENCODE_CONFIG. An empty
// path (with nil error) means a file at one of our paths is not af-managed, so the
// caller must skip injection rather than point opencode at a file af does not own.
//
// opencode merges OPENCODE_CONFIG with the user's own global config rather than
// replacing it — verified: with `instructions` set in BOTH, the resolved config
// listed the user's entry AND af's, and the user's other settings survived. That
// merge is what makes this seam safe; a replacing env var would have silently
// discarded the user's model/theme/MCP setup on every af session, which would be a
// worse violation than writing a file.
func ensureOpencodeAfConfig() (string, error) {
	dir, err := opencodeAfDir()
	if err != nil {
		return "", err
	}
	instructionsPath := filepath.Join(dir, "af-skill.md")
	wrote, err := writeAfMarkedFile(instructionsPath, opencodeInstructionsDoc)
	if err != nil {
		return "", err
	}
	if !wrote {
		log.WarningLog.Printf("af skill: %s exists but is not af-managed; not injecting OPENCODE_CONFIG for opencode", instructionsPath)
		return "", nil
	}

	configPath := filepath.Join(dir, "af-config.jsonc")
	wrote, err = writeAfMarkedFile(configPath, opencodeAfConfigDoc(instructionsPath))
	if err != nil {
		return "", err
	}
	if !wrote {
		log.WarningLog.Printf("af skill: %s exists but is not af-managed; not injecting OPENCODE_CONFIG for opencode", configPath)
		return "", nil
	}
	return configPath, nil
}

// opencodeCarriesConfigEnv reports whether a resolved command already sets
// OPENCODE_CONFIG itself — e.g. a program_overrides entry like
// `OPENCODE_CONFIG=~/mine.jsonc opencode` or `env OPENCODE_CONFIG=~/mine.jsonc
// opencode`. Shell semantics give the LAST assignment precedence, so af prepending
// its own would be overridden by the user's anyway and their config would win
// either way; detecting it lets af SAY the guidance did not land rather than
// leaving that silent.
//
// The scan stops at the command word, so an OPENCODE_CONFIG=... appearing later as
// an ARGUMENT (`opencode --prompt 'set OPENCODE_CONFIG=x'`) is correctly not
// treated as an assignment. A leading `env` is stepped over because `env VAR=v
// <agent>` is a shape af already supports elsewhere (#742).
//
// Accepted limitation: this splits on whitespace rather than shell-tokenizing, so a
// QUOTED value containing spaces in an assignment BEFORE an OPENCODE_CONFIG one
// (`FOO='a b' OPENCODE_CONFIG=x opencode`) ends the scan early and misses it. The
// cost is only the warning: the user's assignment still wins at runtime, so their
// config is honored regardless. tmux's splitShellTokens would handle it but is
// package-private, and duplicating a shell tokenizer here to recover a log line is
// not worth the second parser.
func opencodeCarriesConfigEnv(resolved string) bool {
	for _, tok := range strings.Fields(resolved) {
		switch {
		case strings.HasPrefix(tok, "OPENCODE_CONFIG="):
			return true
		case tok == "env" || strings.Contains(tok, "="):
			// Still in the leading env-assignment run; keep scanning.
		default:
			// The command word: anything past here is an argument, not an
			// assignment.
			return false
		}
	}
	return false
}
