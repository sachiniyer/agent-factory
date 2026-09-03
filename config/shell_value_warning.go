package config

import (
	"fmt"
	"sort"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// Operator-authored config values that af hands to `/bin/sh -c`, warned about at
// load when they begin with `exec --` (#3566).
//
// #3563 fixed the account boundary, which refuses that shape because dash — the
// /bin/sh of Debian and Ubuntu — gives its exec builtin no options and so takes
// `--` as the command NAME, exiting 127. An UNSCOPED session never reaches that
// boundary, and neither do the hook values, so the same string dies with no
// explanation at all. This says something about it once, where the value is
// read.
//
// A WARNING, never a refusal. Which /bin/sh runs the value is not knowable here:
// it is dash on this host, bash on macOS, busybox ash in a container, and for the
// docker/ssh backends a shell on another machine entirely — all of which accept
// the separator. Refusing would break configurations that are correct as written.
// af also does not rewrite the value: the operator wrote it, and #3563 records
// why silently editing it is the wrong answer.
//
// The set of values is derived from the CONSUMERS — every site that hands an
// operator string to a shell's `-c` — not from a list of key names that looks
// plausible. TestShellSites_EveryShellSiteIsClassified holds the tree to that
// derivation, and TestWarnedShellValueKeys_EachOneActuallyWarns holds the keys
// below to the loaders.

// warnedShellValueKeys names every config key whose value reaches a shell's `-c`
// and is inspected by warnExecSeparator below.
//
// It is the drift gate's vocabulary, not an independent policy list: a gate entry
// may only claim a config key that appears here, and each key here must be proven
// to warn from a real loader. `default_program` is deliberately absent — it is
// enum-validated to a bare agent name, so it cannot carry a command.
var warnedShellValueKeys = []string{
	"on_archive_command",
	"post_worktree_commands",
	"program_overrides",
	"root_agent.program",
	"root_agents",
	"sandbox.ssh",
}

// detectedClaudeNote is appended when a value still matches the claude command af
// probes off the operator's shell. DefaultConfig overlays that probe as
// program_overrides.claude BEFORE any file is decoded, so the key can be absent
// from the file the warning names — the `--` is in their ~/.zshrc — and an
// operator sent only to the config file would find nothing to edit.
//
// It is a NOTE rather than a replacement for the path, because af cannot tell the
// two apart here: `source.builtIn` says the value equals the default, not whether
// the file also sets it, and materializeDefaultConfig WRITES the probed value
// into config.toml, so on every later load the common case is that it is in both
// places at once. `validateConfig` has no access to the file's shape
// (metadataForSource is computed in the parsers and never reaches it), so a
// message claiming the key is absent would be false exactly as often as it was
// true. Naming both places is the honest form, and both are real fixes: the file
// value wins for this install, and the alias is what regenerates it.
const detectedClaudeNote = " af also detects this command from your shell (an alias, or PATH), and the value here " +
	"still matches what it detected — so check that alias too: while it carries the `--`, the next config af " +
	"materializes will carry it again."

// shellValue is one operator-authored value bound for `/bin/sh -c`, with the
// config key that names it and, when the value is also af's own probe result, a
// note sending the operator to the shell alias behind it.
type shellValue struct {
	key   string
	value string
	note  string
}

// shellValueSet collects the values one config source hands to a shell. Each of
// the config sources builds its own set, because each admits a different subset
// of the keys and each has its own path to name in the message.
type shellValueSet []shellValue

// add records one scalar value from the file being loaded. Empty values are
// skipped rather than parsed: an unset key is not an operator's authored string.
func (s *shellValueSet) add(key, value string) {
	s.addNoted(key, value, "")
}

// addNoted records a value that carries an extra clause about where else it may
// have come from.
func (s *shellValueSet) addNoted(key, value, note string) {
	if value == "" {
		return
	}
	*s = append(*s, shellValue{key: key, value: value, note: note})
}

// addMap records a `section.<leaf>` map such as program_overrides. builtIn, when
// non-nil, is the defaults snapshot the file was decoded onto: a leaf still equal
// to it is also af's own probe result, which earns the note.
func (s *shellValueSet) addMap(section string, values, builtIn map[string]string, note string) {
	for leaf, value := range values {
		if builtIn != nil && builtIn[leaf] == value {
			s.addNoted(section+"."+leaf, value, note)
			continue
		}
		s.add(section+"."+leaf, value)
	}
}

// addList records an ordered list such as post_worktree_commands, naming each
// element by index so the warning points at the entry the operator must edit.
func (s *shellValueSet) addList(key string, values []string) {
	for i, value := range values {
		s.add(fmt.Sprintf("%s[%d]", key, i), value)
	}
}

// addRootAgents records the legacy path-keyed root_agents map, whose per-repo
// program is a full command string that the root session runs — through the same
// pane shell as any other program.
func (s *shellValueSet) addRootAgents(values map[string]RootAgentConfig) {
	for path, entry := range values {
		s.add(fmt.Sprintf("root_agents[%q].program", path), entry.Program)
	}
}

// warnExecSeparator logs one line for each collected value whose exec builtin is
// followed by `--`. Keys are visited in sorted order so a file with several
// affected values logs the same way on every load.
//
// The predicate is sessionenv.CommandUsesExecSeparator — the same one the account
// boundary refuses on — so the warning and the refusal can never come to
// different conclusions about the same string.
func (s shellValueSet) warnExecSeparator(prettyPath string) {
	affected := make([]shellValue, 0, len(s))
	for _, value := range s {
		if sessionenv.CommandUsesExecSeparator(value.value) {
			affected = append(affected, value)
		}
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i].key < affected[j].key })
	for _, value := range affected {
		lead := fmt.Sprintf("Config issue in %s: %s", prettyPath, value.key)
		// Once per (source, key, value). A config load is not a rare event — the
		// daemon issues ~10 per session-create, and `af config set` re-parses the
		// file twice around its own write — and #2496 already paid for the
		// version of this that said the same thing on every one of them. Keying
		// on the value keeps a LATER edit that reintroduces the shape audible.
		if _, seen := shellValueWarned.LoadOrStore(lead+"\x00"+value.value, struct{}{}); seen {
			continue
		}
		log.WarningLog.Printf(
			"%s begins with `exec --`, and af runs that value through /bin/sh, where the separator is not "+
				"portable; dash — /bin/sh on Debian and Ubuntu — gives its exec builtin no options, so it takes "+
				"`--` as the command name and the command exits 127 with `exec: --: not found`. Remove the "+
				"`--`: af runs the same command written `exec <program> …`. This is a warning, not an error — "+
				"bash (/bin/sh on macOS), busybox ash and zsh in sh mode all accept the separator, so the value "+
				"is correct as written on those shells, and a docker or ssh backend runs it on another machine's "+
				"shell entirely.%s",
			lead, value.note)
	}
}

// warnGlobalShellValues is the global file's set. It is a named function rather
// than an inline block in validateConfig because materializeDefaultConfig has to
// run the same inspection over a config it never validates (#3566 review): that
// path writes the probed default and returns it, so without this the very first
// load after a first run is the one load that says nothing.
func warnGlobalShellValues(config *Config, prettyConfigPath string) {
	var overrideDefaults map[string]string
	if config.source.builtIn != nil {
		overrideDefaults = config.source.builtIn.ProgramOverrides
	}
	values := shellValueSet{}
	values.addMap("program_overrides", config.ProgramOverrides, overrideDefaults, detectedClaudeNote)
	values.add("on_archive_command", config.OnArchiveCommand)
	values.add("sandbox.ssh", config.SandboxSSH)
	values.add("root_agent.program", config.RootAgent.Program)
	values.addRootAgents(config.RootAgents)
	values.warnExecSeparator(prettyConfigPath)
}

// shellValueWarned memoizes the (source, key, value) triples already warned
// about, on the #2496 precedent: a genuinely misconfigured value deserves one
// notice per source per af process, not one per config load.
var shellValueWarned sync.Map

// resetShellValueWarnings clears that memo. captureLog calls it so a test
// asserting the warning is not silenced by an earlier test in the same process
// that happened to use the same path, key and value.
func resetShellValueWarnings() {
	shellValueWarned.Clear()
}
