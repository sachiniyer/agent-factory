package config

import (
	"fmt"
	"sort"

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
// enum-validated to a bare agent name, so it cannot carry a command at all.
var warnedShellValueKeys = []string{
	"on_archive_command",
	"post_worktree_commands",
	"program_overrides",
	"sandbox.ssh",
}

// shellValueSet collects the operator-authored values one config source hands to
// `/bin/sh -c`, keyed by the config key that names each one
// ("program_overrides.claude", "post_worktree_commands[1]"). Each of the three
// config sources builds its own set, because each admits a different subset of
// the keys and each has its own path to name in the message.
type shellValueSet map[string]string

// add records one scalar value. Empty values are skipped rather than parsed: an
// unset key is not an operator's authored string.
func (s shellValueSet) add(key, value string) {
	if value == "" {
		return
	}
	s[key] = value
}

// addMap records a `section.<leaf>` map such as program_overrides.
func (s shellValueSet) addMap(section string, values map[string]string) {
	for leaf, value := range values {
		s.add(section+"."+leaf, value)
	}
}

// addList records an ordered list such as post_worktree_commands, naming each
// element by index so the warning points at the entry the operator must edit.
func (s shellValueSet) addList(key string, values []string) {
	for i, value := range values {
		s.add(fmt.Sprintf("%s[%d]", key, i), value)
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
	affected := make([]string, 0, len(s))
	for key, value := range s {
		if sessionenv.CommandUsesExecSeparator(value) {
			affected = append(affected, key)
		}
	}
	sort.Strings(affected)
	for _, key := range affected {
		log.WarningLog.Printf(
			"Config issue in %s: %s begins with `exec --`, and af runs that value through /bin/sh, where the "+
				"separator is not portable; dash — /bin/sh on Debian and Ubuntu — gives its exec builtin no "+
				"options, so it takes `--` as the command name and the command exits 127 with "+
				"`exec: --: not found`. Remove the `--`: af runs the same command written `exec <program> …`. "+
				"This is a warning, not an error — bash (/bin/sh on macOS), busybox ash and zsh in sh mode all "+
				"accept the separator, so the value is correct as written on those shells, and a docker or ssh "+
				"backend runs it on another machine's shell entirely",
			prettyPath, key)
	}
}
