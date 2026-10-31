package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// The plugin's identity — the fields any agent's plugin manifest carries. They
// are constants rather than literals inside the manifest below because the
// SAME identity is emitted by the generated, installable per-agent plugins
// (commands/plugins_gen.go, #2172): the manifest af writes at runtime for the
// session it is launching, and the manifest a user installs from the repo
// marketplace, must describe the same plugin.
const (
	// AfPluginDescription is the plugin's one-line summary. Unlike
	// AfSkillDescription (which tells an agent when to activate the skill),
	// this is what a human reads in a marketplace listing.
	AfPluginDescription = "Run and schedule AI coding agents in isolated git worktrees with the Agent Factory (af) CLI"
	// AfPluginAuthorName is the publisher name every manifest carries.
	AfPluginAuthorName = "Agent Factory"
	// AfPluginHomepage is the project URL every manifest carries.
	AfPluginHomepage = "https://github.com/sachiniyer/agent-factory"
)

// pluginManifest is the .claude-plugin/plugin.json content required by Claude Code.
const pluginManifest = `{
  "name": "` + AfSkillName + `",
  "description": "` + AfPluginDescription + `",
  "author": {
    "name": "` + AfPluginAuthorName + `"
  }
}
`

// legacyGuardNoop keeps already-running pre-#2563 Claude sessions usable after
// an af upgrade. Those sessions retain their loaded PreToolUse wrapper, which
// still executes this path even after hooks.json is removed. New sessions never
// load the tombstone because ensurePluginDir removes hooks.json.
const legacyGuardNoop = "#!/bin/sh\nexit 0\n"

// pluginCommands defines the command files to write into the plugin directory.
// ensurePluginDir writes this map to disk on every session launch and prunes any
// .md file that isn't listed here, so adding/removing/editing a skill is as simple
// as changing this map (the pre-#1043 per-command af-*.md files are pruned this
// way on existing installs).
//
// Since #1043 there is exactly one entry: the "af" skill, whose body is
// afUsageReference (systemprompt.go) — the same text every other agent receives
// via its own skill/context file (agentskill.go) — so no agent's af knowledge can
// drift. A callbackless session gets afUsageReferenceCallbackless instead: the
// file lives under this host's af config dir, and a host whose sessions cannot
// reach the daemon must not be taught af verbs that all fail (#5571 W3.1).
func pluginCommands(caps guidanceCaps) map[string]string {
	body := caps.Reference()
	return map[string]string{
		"af.md": "---\n" +
			"allowed-tools: Bash(af:*)\n" +
			"description: " + afSkillDescriptionInside + "\n" +
			"argument-hint: [request]\n" +
			"---\n" +
			"\n" +
			body + "\n" +
			"\n" +
			"User request (may be empty): $ARGUMENTS\n",
	}
}

// afProposalSkillName names the model-invoked proposal skill. Claude loads a
// slash command body only when the request matches its description, so the
// proposal entry point needs its own skill with a proposal-shaped trigger.
const afProposalSkillName = "proposal"

// pluginSkills maps a skill directory under skills/ to its SKILL.md. Like
// pluginCommands, ensurePluginDir writes every entry and prunes undeclared ones.
// A callbackless session gets none: every skill here teaches af verbs, which is
// exactly the content a session that cannot reach the daemon must not see
// (#5571 W3.1). The one skill is the proposal entry point, so a session whose
// credential cannot reach the proposal store gets none either.
func pluginSkills(caps guidanceCaps) map[string]string {
	if !caps.Callback || !caps.HostStores {
		return nil
	}
	return map[string]string{
		afProposalSkillName: "---\n" +
			"name: " + afProposalSkillName + "\n" +
			"description: " + afProposalTrigger + ". Starts or attaches an Agent Factory (af) proposal instead of writing docs into the repository.\n" +
			"allowed-tools: Bash(af:*)\n" +
			"---\n" +
			"\n" +
			afUsageProposalsInside + "\n",
	}
}

// ensurePluginDir creates the plugin directory with its manifest and slash
// command, then returns its path. The directory is located at <config-dir>/plugin/.
//
// This is called on every claude-based session launch (see injectSystemPrompt),
// and rewrites the manifest, writes every file in pluginCommands, prunes any
// stray .md in commands/ that isn't in the map, and disarms the stale hook an
// older af wrote — so inserts, edits, and removes all propagate on the next
// session start.
func ensurePluginDir(caps guidanceCaps) (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}

	// The callbackless variant gets its own directory for the same reason the
	// aider seam gets its own filename: sessions on a shared remote user share
	// this config dir, and two variants at one path flap — the later launch
	// rewrites the plugin the earlier session is about to read (#5571 W3.1,
	// PR #5577 review).
	pluginName := "plugin"
	if key := caps.key(); key != "" {
		pluginName = "plugin-" + key
	}
	pluginDir := filepath.Join(configDir, pluginName)
	commandsDir := filepath.Join(pluginDir, "commands")
	hooksDir := filepath.Join(pluginDir, "hooks")
	manifestDir := filepath.Join(pluginDir, ".claude-plugin")

	// MkdirAllUnderAFHome, not os.MkdirAll: this is <config-dir>/plugin, so
	// MkdirAll would re-create the whole AF home as an ancestor — on EVERY claude
	// launch, and before any write the #3845 latch could refuse (#3850).
	if err := config.MkdirAllUnderAFHome(commandsDir, 0755); err != nil {
		return "", err
	}
	if err := config.MkdirAllUnderAFHome(manifestDir, 0755); err != nil {
		return "", err
	}

	// A prior af wrote hooks.json + guard-tmux.sh. Remove only the registration
	// so new sessions load no PreToolUse guard, then replace its executable with
	// a no-op for already-running Claude sessions that cached the old wrapper at
	// startup (#2608). Deleting the whole directory leaves those live wrappers
	// failing closed on every Bash call.
	if err := os.Remove(filepath.Join(hooksDir, "hooks.json")); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	// Every write below refuses a symlinked destination (#3672). <config-dir>/plugin
	// is regenerated wholesale on each claude session launch — af writes the
	// manifest, rewrites the commands, and prunes what it no longer declares — so
	// a link inside it is a path af would otherwise clobber on the next launch.
	if err := config.AtomicWriteFileRefusingLink(filepath.Join(hooksDir, "guard-tmux.sh"), []byte(legacyGuardNoop), 0755); err != nil {
		return "", err
	}

	// Write plugin manifest
	manifestPath := filepath.Join(manifestDir, "plugin.json")
	if err := config.AtomicWriteFileRefusingLink(manifestPath, []byte(pluginManifest), 0644); err != nil {
		return "", err
	}

	commands := pluginCommands(caps)

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
		if _, ok := commands[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(commandsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}

	// Write command files
	for name, content := range commands {
		path := filepath.Join(commandsDir, name)
		if err := config.AtomicWriteFileRefusingLink(path, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	if err := syncPluginSkills(filepath.Join(pluginDir, "skills"), pluginSkills(caps)); err != nil {
		return "", err
	}

	return pluginDir, nil
}

// syncPluginSkills writes every entry of skills under skillsDir and prunes
// skill directories it does not declare. A pruned directory loses only its
// SKILL.md and is then removed only if empty, so nothing else in it is deleted.
func syncPluginSkills(skillsDir string, skills map[string]string) error {
	if err := config.MkdirAllUnderAFHome(skillsDir, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := skills[entry.Name()]; ok {
			continue
		}
		dir := filepath.Join(skillsDir, entry.Name())
		if err := config.RemoveFileRefusingLink(filepath.Join(dir, "SKILL.md")); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(dir)
	}
	for name, content := range skills {
		dir := filepath.Join(skillsDir, name)
		if err := config.MkdirAllUnderAFHome(dir, 0755); err != nil {
			return err
		}
		if err := config.AtomicWriteFileRefusingLink(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}
