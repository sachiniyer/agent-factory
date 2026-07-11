package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// pluginManifest is the .claude-plugin/plugin.json content required by Claude Code.
const pluginManifest = `{
  "name": "agent-factory",
  "description": "Agent Factory (af) CLI usage: sessions, tabs, tasks, daemon",
  "author": {
    "name": "Agent Factory"
  }
}
`

// pluginCommands defines the command files to write into the plugin directory.
// ensurePluginDir writes this map to disk on every session launch and prunes any
// .md file that isn't listed here, so adding/removing/editing a skill is as simple
// as changing this map (the pre-#1043 per-command af-*.md files are pruned this
// way on existing installs).
//
// Since #1043 there is exactly one entry: the "af" skill, whose body is
// afUsageReference (systemprompt.go) — the same text every other agent receives
// via its own skill/context file (agentskill.go) — so no agent's af knowledge can
// drift.
var pluginCommands = map[string]string{
	"af.md": "---\n" +
		"allowed-tools: Bash(af:*)\n" +
		"description: Manage Agent Factory (af) sessions, tabs, scheduled tasks, and the daemon via the af CLI\n" +
		"argument-hint: [request]\n" +
		"---\n" +
		"\n" +
		afUsageReference + "\n" +
		"\n" +
		"User request (may be empty): $ARGUMENTS\n",
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
	if err := config.AtomicWriteFile(manifestPath, []byte(pluginManifest), 0644); err != nil {
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
		if err := config.AtomicWriteFile(path, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	return pluginDir, nil
}
