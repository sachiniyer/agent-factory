package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// pluginManifest is the .claude-plugin/plugin.json content required by Claude Code.
const pluginManifest = `{
  "name": "agent-factory",
  "description": "Agent Factory (af) session management commands",
  "author": {
    "name": "Agent Factory"
  }
}
`

// pluginCommands defines the slash command files to write into the plugin directory.
// ensurePluginDir writes this map to disk on every session launch and prunes any
// .md file that isn't listed here, so adding/removing/editing a skill is as simple
// as changing this map.
var pluginCommands = map[string]string{
	"af-sessions.md": "---\n" +
		"allowed-tools: Bash(af sessions list:*)\n" +
		"description: List all Agent Factory sessions\n" +
		"---\n" +
		"\n" +
		"List all running Agent Factory sessions.\n" +
		"\n" +
		"## Context\n" +
		"\n" +
		"- Current sessions: !`af sessions list`\n",

	"af-create.md": "---\n" +
		"allowed-tools: Bash(af sessions create:*)\n" +
		"description: Create a new Agent Factory session\n" +
		"argument-hint: <session-name> [initial-prompt]\n" +
		"---\n" +
		"\n" +
		"Create a new Agent Factory session (a new agent instance in an isolated git worktree).\n" +
		"\n" +
		"The user provided: $ARGUMENTS\n" +
		"\n" +
		"Parse the session name (first argument) and optional initial prompt (remaining arguments), " +
		"then run `af sessions create --name <name> --prompt <prompt>`. " +
		"If no prompt is given, omit the --prompt flag. " +
		"The name should be a short kebab-case identifier the user will recognize in the session list.\n",

	"af-kill.md": "---\n" +
		"allowed-tools: Bash(af sessions kill:*)\n" +
		"description: Kill an Agent Factory session by title\n" +
		"argument-hint: <session-title>\n" +
		"---\n" +
		"\n" +
		"Kill the specified Agent Factory session.\n" +
		"\n" +
		"The user wants to kill the session: $ARGUMENTS\n" +
		"\n" +
		"Run `af sessions kill $ARGUMENTS` to kill it.\n",

	"af-send.md": "---\n" +
		"allowed-tools: Bash(af sessions send-prompt:*)\n" +
		"description: Send a prompt to another Agent Factory session\n" +
		"argument-hint: <session-title> <prompt>\n" +
		"---\n" +
		"\n" +
		"Send a prompt to another running Agent Factory session.\n" +
		"\n" +
		"The user provided: $ARGUMENTS\n" +
		"\n" +
		"Parse the session title (first argument) and prompt (remaining arguments), " +
		"then run `af sessions send-prompt <title> <prompt>`.\n",

	"af-preview.md": "---\n" +
		"allowed-tools: Bash(af sessions preview:*)\n" +
		"description: Preview another Agent Factory session's terminal output\n" +
		"argument-hint: <session-title>\n" +
		"---\n" +
		"\n" +
		"View the terminal output of another running Agent Factory session.\n" +
		"\n" +
		"The user wants to preview the session: $ARGUMENTS\n" +
		"\n" +
		"Run `af sessions preview $ARGUMENTS` to view it.\n",

	"af-whoami.md": "---\n" +
		"allowed-tools: Bash(af sessions whoami:*)\n" +
		"description: Identify the current Agent Factory session\n" +
		"---\n" +
		"\n" +
		"Identify which Agent Factory session you are running in.\n" +
		"\n" +
		"## Context\n" +
		"\n" +
		"- Current session: !`af sessions whoami`\n",
}

// ensurePluginDir creates the plugin directory with manifest and slash command
// files and returns its path. The directory is located at <config-dir>/plugin/.
//
// This is called on every claude-based session launch (see injectSystemPrompt),
// and rewrites the manifest, writes every file in pluginCommands, and prunes any
// stray .md in commands/ that isn't in the map — so inserts, edits, and removes
// in pluginCommands all propagate on the next session start.
func ensurePluginDir() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}

	pluginDir := filepath.Join(configDir, "plugin")
	commandsDir := filepath.Join(pluginDir, "commands")
	manifestDir := filepath.Join(pluginDir, ".claude-plugin")

	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return "", err
	}

	// Write plugin manifest
	manifestPath := filepath.Join(manifestDir, "plugin.json")
	if err := os.WriteFile(manifestPath, []byte(pluginManifest), 0644); err != nil {
		return "", err
	}

	// Prune stale command files no longer declared in pluginCommands so
	// removed/renamed skills don't linger as orphan slash commands.
	entries, err := os.ReadDir(commandsDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if _, ok := pluginCommands[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(commandsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}

	// Write command files
	for name, content := range pluginCommands {
		path := filepath.Join(commandsDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	return pluginDir, nil
}
