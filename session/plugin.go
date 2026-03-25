package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// pluginCommands defines the slash command files to write into the plugin directory.
var pluginCommands = map[string]string{
	"af-sessions.md": `---
allowed-tools: Bash(af api sessions list:*)
description: List all Agent Factory sessions
---

List all running Agent Factory sessions.

` + "!" + `af api sessions list
`,

	"af-kill.md": `---
allowed-tools: Bash(af api sessions kill:*)
description: Kill an Agent Factory session by title
---

Kill the specified Agent Factory session. The user must provide the session title as an argument.

## Usage
/af-kill <session-title>
`,

	"af-send.md": `---
allowed-tools: Bash(af api sessions send-prompt:*)
description: Send a prompt to another Agent Factory session
---

Send a prompt to another running Agent Factory session. The user must provide the session title and the prompt text.

## Usage
/af-send <session-title> <prompt>
`,

	"af-preview.md": `---
allowed-tools: Bash(af api sessions preview:*)
description: Preview another Agent Factory session's terminal output
---

View the terminal output of another running Agent Factory session. The user must provide the session title.

## Usage
/af-preview <session-title>
`,
}

// ensurePluginDir creates the plugin directory with slash command files and returns its path.
// The directory is located at <config-dir>/plugin/commands/.
func ensurePluginDir() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}

	commandsDir := filepath.Join(configDir, "plugin", "commands")
	if err := os.MkdirAll(commandsDir, 0755); err != nil {
		return "", err
	}

	for name, content := range pluginCommands {
		path := filepath.Join(commandsDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	return filepath.Join(configDir, "plugin"), nil
}
