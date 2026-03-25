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
var pluginCommands = map[string]string{
	"af-sessions.md": "---\n" +
		"allowed-tools: Bash(af api sessions list:*)\n" +
		"description: List all Agent Factory sessions\n" +
		"---\n" +
		"\n" +
		"List all running Agent Factory sessions.\n" +
		"\n" +
		"## Context\n" +
		"\n" +
		"- Current sessions: !`af api sessions list`\n",

	"af-kill.md": "---\n" +
		"allowed-tools: Bash(af api sessions kill:*)\n" +
		"description: Kill an Agent Factory session by title\n" +
		"argument-hint: <session-title>\n" +
		"---\n" +
		"\n" +
		"Kill the specified Agent Factory session.\n" +
		"\n" +
		"The user wants to kill the session: $ARGUMENTS\n" +
		"\n" +
		"Run `af api sessions kill $ARGUMENTS` to kill it.\n",

	"af-send.md": "---\n" +
		"allowed-tools: Bash(af api sessions send-prompt:*)\n" +
		"description: Send a prompt to another Agent Factory session\n" +
		"argument-hint: <session-title> <prompt>\n" +
		"---\n" +
		"\n" +
		"Send a prompt to another running Agent Factory session.\n" +
		"\n" +
		"The user provided: $ARGUMENTS\n" +
		"\n" +
		"Parse the session title (first argument) and prompt (remaining arguments), " +
		"then run `af api sessions send-prompt <title> <prompt>`.\n",

	"af-preview.md": "---\n" +
		"allowed-tools: Bash(af api sessions preview:*)\n" +
		"description: Preview another Agent Factory session's terminal output\n" +
		"argument-hint: <session-title>\n" +
		"---\n" +
		"\n" +
		"View the terminal output of another running Agent Factory session.\n" +
		"\n" +
		"The user wants to preview the session: $ARGUMENTS\n" +
		"\n" +
		"Run `af api sessions preview $ARGUMENTS` to view it.\n",

	"af-whoami.md": "---\n" +
		"allowed-tools: Bash(af api sessions whoami:*)\n" +
		"description: Identify the current Agent Factory session\n" +
		"---\n" +
		"\n" +
		"Identify which Agent Factory session you are running in.\n" +
		"\n" +
		"## Context\n" +
		"\n" +
		"- Current session: !`af api sessions whoami`\n",
}

// ensurePluginDir creates the plugin directory with manifest and slash command
// files and returns its path. The directory is located at <config-dir>/plugin/.
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

	// Write command files
	for name, content := range pluginCommands {
		path := filepath.Join(commandsDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	return pluginDir, nil
}
