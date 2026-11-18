package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Option tables and leaf dispatch for the env-injection launcher grammars
// in account_environment_launchers.go — kept in a sibling file for the
// file-length lint. Every table is closed on purpose: an option absent
// from the launcher's table is unprovable rather than assumed benign.

// sudoLongOptions is sudo 1.9's long-option table; true means the option
// requires an operand, which may be the next word. getopt_long resolves any
// unambiguous abbreviation, so the table must be closed: an option missing
// from it refuses rather than being assumed a flag.
var sudoLongOptions = map[string]bool{
	"chdir": true, "chroot": true, "close-from": true,
	"command-timeout": true, "group": true, "host": true,
	"other-user": true, "prompt": true, "role": true,
	"type": true, "user": true,

	"askpass": false, "background": false, "bell": false,
	"edit": false, "help": false, "list": false, "login": false,
	"non-interactive": false, "preserve-env": false,
	"preserve-groups": false, "remove-timestamp": false,
	"reset-timestamp": false, "set-home": false, "shell": false,
	"stdin": false, "validate": false, "version": false,
}

// sudoShortFlags are sudo's no-operand short options; sudoShortValueFlags
// are the ones that take an operand (attached in the cluster or as the next
// word). `-h` is neither: its host operand is optional and attached-only.
const sudoShortFlags = "ABbEHikKlNnPsSVve"
const sudoShortValueFlags = "CDgpRrTtUu"

var systemdRunLongOptions = map[string]bool{
	"description": true, "expand-environment": true, "gid": true,
	"host": true, "machine": true, "nice": true, "on-active": true,
	"on-boot": true, "on-calendar": true, "on-startup": true,
	"on-unit-active": true, "on-unit-inactive": true,
	"path-property": true, "property": true, "service-type": true,
	"setenv": true, "slice": true, "socket-property": true,
	"timer-property": true, "uid": true, "unit": true,
	"working-directory": true,

	"collect": false, "help": false, "no-ask-password": false,
	"no-block": false, "on-clock-change": false,
	"on-timezone-change": false, "pipe": false, "pty": false,
	"quiet":             false,
	"remain-after-exit": false, "same-dir": false, "scope": false,
	"send-sighup": false, "shell": false, "slice-inherit": false,
	"user": false, "version": false, "wait": false,
}

const systemdRunShortFlags = "hdGtPqrS"
const systemdRunShortValueFlags = "HMu"

// dockerOptionSpec is one option region's table: long names to whether they
// take an operand (exact-match — docker's parser is pflag and never
// abbreviates), and the short letters that are flags or take operands.
type dockerOptionSpec struct {
	longs       map[string]bool
	shortFlags  string
	shortValues string
}

// dockerOptionStep consumes the literal dash-word at words[i] under spec.
// The env spellings are judged before the table so `-e`/`--env` variants
// get the denied-name cut even on a leaf whose table does not list them. A
// denied assign spelling is deferred through denied under requireCommand;
// an env file stays an immediate refusal — its contents are unverifiable
// with or without a child.
func dockerOptionStep(words []*syntax.Word, i int, spec dockerOptionSpec, names map[string]struct{}, memo operandTailMemo, requireCommand bool, denied *bool) (int, bool) {
	literal, _ := literalShellWord(words[i])
	if strings.HasPrefix(literal, "--") {
		name, value, attached := strings.Cut(literal[2:], "=")
		switch {
		case dockerEnvFileLong(name):
			return 0, true
		case dockerEnvAssignLong(name):
			if !attached {
				return dockerEnvOperandStep(words, i, names, memo, requireCommand, denied)
			}
			if accountEnvironmentOperandDenied(value, names) {
				if !requireCommand {
					return 0, true
				}
				*denied = true
			}
			return i + 1, false
		}
		requiresArg, known := spec.longs[name]
		if !known {
			// An unknown long option on a launching leaf is unverifiable:
			// docker's pflag never abbreviates, but hidden aliases and
			// version additions are real options whose operand would shift
			// the image boundary (`--net host` mistaken for the image).
			// docker rejects an unknown flag itself, so refusing matches
			// the command's real fate.
			return 0, true
		}
		if !requiresArg || attached {
			return i + 1, false
		}
		return launcherSeparateOperandStep(words, i, names, memo)
	}
	cluster := literal[1:]
	for j := 0; j < len(cluster); j++ {
		c := cluster[j]
		switch {
		case strings.IndexByte(spec.shortFlags, c) >= 0:
			// pflag's `-f=true` bool syntax: an `=` right after a flag
			// letter carries the flag's explicit value and ends the word
			// (`-t=false` consumes like `--tty=false`).
			if j+1 < len(cluster) && cluster[j+1] == '=' {
				return i + 1, false
			}
		case c == 'e' && strings.IndexByte(spec.shortValues, 'e') >= 0:
			// pflag's `-n=1234` short-option syntax: an `=` right after the
			// letter is the separator, not part of the operand, so
			// `-e=CODEX_HOME=/x` must judge `CODEX_HOME=/x`. An emptied
			// operand (`-e=`) falls to the separate-word step — the
			// conservative read of a spelling docker itself rejects.
			operand := strings.TrimPrefix(cluster[j+1:], "=")
			if operand != "" {
				if accountEnvironmentOperandDenied(operand, names) {
					if !requireCommand {
						return 0, true
					}
					*denied = true
				}
				return i + 1, false
			}
			return dockerEnvOperandStep(words, i, names, memo, requireCommand, denied)
		case strings.IndexByte(spec.shortValues, c) >= 0:
			if j+1 < len(cluster) {
				return i + 1, false
			}
			return launcherSeparateOperandStep(words, i, names, memo)
		default:
			return 0, true
		}
	}
	return i + 1, false
}

// dockerEnvSpellingStep is the inert-tail counterpart of dockerOptionStep
// for the subcommands that take no env option of their own: an env spelling
// is still refused, every other word is inert and consumed. Short clusters
// are read with the union of every docker leaf's flag letters so a `-te`
// next to `docker build` still finds the `e`; a letter outside the union
// ends the read — the word is data, and docker rejects it anyway.
func dockerEnvSpellingStep(words []*syntax.Word, i int, names map[string]struct{}, memo operandTailMemo) (int, bool) {
	literal, _ := literalShellWord(words[i])
	if strings.HasPrefix(literal, "--") {
		name, value, attached := strings.Cut(literal[2:], "=")
		switch {
		case dockerEnvFileLong(name):
			return 0, true
		case dockerEnvAssignLong(name):
			if !attached {
				return dockerEnvOperandStep(words, i, names, memo, false, nil)
			}
			if accountEnvironmentOperandDenied(value, names) {
				return 0, true
			}
		}
		return i + 1, false
	}
	if !strings.HasPrefix(literal, "-") || literal == "-" {
		return i + 1, false
	}
	cluster := literal[1:]
	for j := 0; j < len(cluster); j++ {
		c := cluster[j]
		switch {
		case strings.IndexByte(dockerInertLeafSpec.shortFlags, c) >= 0:
			if j+1 < len(cluster) && cluster[j+1] == '=' {
				return i + 1, false
			}
		case c == 'e':
			operand := strings.TrimPrefix(cluster[j+1:], "=")
			if operand != "" {
				if accountEnvironmentOperandDenied(operand, names) {
					return 0, true
				}
				return i + 1, false
			}
			return dockerEnvOperandStep(words, i, names, memo, false, nil)
		default:
			return i + 1, false
		}
	}
	return i + 1, false
}

// dockerEnvOperandStep judges a `-e`/`--env` family's separate-word
// operand: it must be literal to read the name (`$V` spells anything), the
// operand is denied when its `=`-cut name is, and it stays a command-head
// candidate because a shadowed docker can discard it and exec there —
// `docker run -e sudo CODEX_HOME=/x` places the injection one word later.
// A denial defers to the caller's boundary check under requireCommand.
func dockerEnvOperandStep(words []*syntax.Word, i int, names map[string]struct{}, memo operandTailMemo, requireCommand bool, denied *bool) (int, bool) {
	if i+1 >= len(words) {
		return 0, true
	}
	operand, ok := literalShellWord(words[i+1])
	if !ok {
		// A non-literal operand can still name its target when the `NAME=`
		// text is literal (`PORT="$PORT"`): the value being dynamic changes
		// nothing about which variable is assigned — the same rule sudo's
		// assignment region keeps. A word that cannot name its target
		// (`$V` alone) stays unprovable.
		if name, assignment := shellWordAssignmentName(words[i+1]); assignment {
			if dockerEnvNameDenied(name, names, requireCommand, denied) {
				return 0, true
			}
			return i + 2, false
		}
		return 0, true
	}
	if accountEnvironmentOperandDenied(operand, names) {
		if !requireCommand {
			return 0, true
		}
		*denied = true
	}
	if wrapperOperandTailMutates(words[i+1:], names, memo) {
		return 0, true
	}
	return i + 2, false
}

// launcherLiteralPrefix returns the concatenated literal text of a word's
// leading literal parts — `PORT=` in `PORT="$PORT"`, or `--env=PORT=` in
// `--env=PORT="$PORT"` — stopping at the first dynamic part.
func launcherLiteralPrefix(word *syntax.Word) string {
	var b strings.Builder
	for _, part := range word.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			break
		}
		b.WriteString(lit.Value)
	}
	return b.String()
}

// dockerEnvNameDenied applies the denied-name cut plus the requireCommand
// deferral shared by every docker env-assign spelling.
func dockerEnvNameDenied(name string, names map[string]struct{}, requireCommand bool, denied *bool) bool {
	if !accountEnvironmentOperandDenied(name, names) {
		return false
	}
	if !requireCommand {
		return true
	}
	*denied = true
	return false
}

// dockerInertLeafSpec is the short-cluster union dockerEnvSpellingStep
// reads for non-launching leaves, reused by dockerDynamicEnvName so a
// dynamic `-e`-sigil there names the same operand.
var dockerInertLeafSpec = dockerOptionSpec{
	shortFlags:  "diPqtT",
	shortValues: "e",
}

// dockerDynamicEnvName reads a non-literal word whose literal prefix still
// pins it to an env-assign spelling — `--env=PORT="$PORT"`, `-ePORT="$PORT"`,
// or `-te=PORT="$PORT"` — and returns the assigned variable name. The sigil
// before the word's first `=` is a pflag short cluster: an `=` right after
// the `e` letter is pflag's separator, so the operand's name opens `rest`,
// while a nonempty cluster remainder is the operand's NAME itself
// (`-ePORT=`). A dynamic env-file spelling is not recognized here and stays
// unprovable, refusing through the caller's default path like any other
// unreadable word.
func dockerDynamicEnvName(word *syntax.Word, spec dockerOptionSpec) (string, bool) {
	prefix := launcherLiteralPrefix(word)
	eq := strings.Index(prefix, "=")
	if eq < 0 {
		return "", false
	}
	sigil, rest := prefix[:eq], prefix[eq+1:]
	var name string
	switch {
	case strings.HasPrefix(sigil, "--"):
		if !dockerEnvAssignLong(sigil[2:]) {
			return "", false
		}
		name, _, _ = strings.Cut(rest, "=")
	case strings.HasPrefix(sigil, "-"):
		cluster := sigil[1:]
		for j := 0; j < len(cluster); j++ {
			c := cluster[j]
			switch {
			case strings.IndexByte(spec.shortFlags, c) >= 0:
			case c == 'e' && strings.IndexByte(spec.shortValues, 'e') >= 0:
				if operand := cluster[j+1:]; operand != "" {
					name = operand
				} else {
					name, _, _ = strings.Cut(rest, "=")
				}
			default:
				// Another value letter takes the cluster's rest as its
				// own operand, and an unknown letter leaves the word
				// unprovable — either way it is not an env spelling.
				return "", false
			}
			if name != "" {
				break
			}
		}
	default:
		return "", false
	}
	if !validName(name) {
		return "", false
	}
	return name, true
}

// dockerEnvAssignLong names the long options whose operand is a NAME or
// NAME=value docker applies to the child's environment: run/create/exec's
// --env, and compose exec's --env-add/--env-rm (a removal drops a pin the
// same way UnsetEnvironment does). dockerEnvFileLong names the file
// spellings, whose contents the gate cannot prove.
func dockerEnvAssignLong(name string) bool {
	switch name {
	case "env", "env-add", "env-rm":
		return true
	}
	return false
}

func dockerEnvFileLong(name string) bool {
	switch name {
	case "env-file", "env-from-file":
		return true
	}
	return false
}

// dockerManagementGroup names docker's subcommand groups — the word whose
// NEXT word is the real leaf (`docker container exec`, `docker image
// build`). Only the group dispatch needs these: their leaves all take the
// inert-tail scan, so an unlisted group merely anchors the returned tail
// one word earlier without changing the verdict.
func dockerManagementGroup(word string) bool {
	switch word {
	case "builder", "buildx", "checkpoint", "config", "container", "context",
		"image", "manifest", "model", "network", "node", "plugin", "scout",
		"secret", "service", "stack", "swarm", "system", "trust", "volume":
		return true
	}
	return false
}

// dockerPassthroughLeaf returns the option spec for a leaf that places a
// child inside a container: run/create/exec under docker or the container
// group, and run/exec under compose.
func dockerPassthroughLeaf(group, leaf string) (dockerOptionSpec, bool) {
	switch {
	case group == "compose":
		switch leaf {
		case "run":
			return dockerComposeRunOptions, true
		case "exec":
			return dockerComposeExecOptions, true
		}
	case group == "" || group == "container":
		switch leaf {
		case "run", "create":
			return dockerRunCreateOptions, true
		case "exec":
			return dockerExecOptions, true
		}
	}
	return dockerOptionSpec{}, false
}

var dockerGlobalOptions = dockerOptionSpec{
	longs: map[string]bool{
		"config": true, "context": true, "host": true, "log-level": true,
		"tlscacert": true, "tlscert": true, "tlskey": true,

		"debug": false, "help": false, "tls": false, "tlsverify": false,
		"version": false,
	},
	shortFlags:  "Dv",
	shortValues: "cHl",
}

var dockerComposeGlobalOptions = dockerOptionSpec{
	longs: map[string]bool{
		"ansi": true, "file": true, "parallel": true, "profile": true,
		"progress": true, "project-directory": true, "project-name": true,

		"all-resources": false, "compatibility": false, "dry-run": false,
		"help": false,
	},
	shortFlags:  "",
	shortValues: "fp",
}

var dockerRunCreateOptions = dockerOptionSpec{
	longs: map[string]bool{
		"add-host": true, "annotation": true, "attach": true,
		"blkio-weight": true, "blkio-weight-device": true, "cap-add": true,
		"cap-drop": true, "cgroup-parent": true, "cgroupns": true,
		"cidfile": true, "cpu-period": true, "cpu-quota": true,
		"cpu-rt-period": true, "cpu-rt-runtime": true, "cpu-shares": true,
		"cpus": true, "cpuset-cpus": true, "cpuset-mems": true,
		"detach-keys": true, "device": true, "device-cgroup-rule": true,
		"device-read-bps": true, "device-read-iops": true,
		"device-write-bps": true, "device-write-iops": true, "dns": true,
		"dns-option": true, "dns-search": true, "domainname": true,
		"entrypoint": true, "expose": true, "gpus": true, "group-add": true,
		"health-cmd": true, "health-interval": true, "health-retries": true,
		"health-start-interval": true, "health-start-period": true,
		"health-timeout": true, "hostname": true, "ip": true, "ip6": true,
		"ipc": true, "isolation": true, "label": true, "label-file": true,
		"link": true, "link-local-ip": true, "log-driver": true,
		"log-opt": true, "mac-address": true, "memory": true,
		"memory-reservation": true, "memory-swap": true,
		"memory-swappiness": true, "mount": true, "name": true,
		"network": true, "network-alias": true, "oom-score-adj": true,
		// Hidden aliases docker/cli's opts.go registers alongside the
		// recommended spellings (docker 29.4.0): each still consumes an
		// operand, so omitting one shifts the image boundary.
		"net": true, "net-alias": true, "dns-opt": true,
		"pid": true, "pids-limit": true, "platform": true, "publish": true,
		"pull": true, "restart": true, "runtime": true, "security-opt": true,
		"shm-size": true, "stop-signal": true, "stop-timeout": true,
		"storage-opt": true, "sysctl": true, "tmpfs": true, "ulimit": true,
		"user": true, "userns": true, "uts": true, "volume": true,
		"volume-driver": true, "volumes-from": true, "workdir": true,

		"detach": false, "help": false, "init": false, "interactive": false,
		"no-healthcheck": false, "oom-kill-disable": false,
		"privileged": false, "publish-all": false, "quiet": false,
		"read-only": false, "rm": false, "sig-proxy": false, "tty": false,
		"use-api-socket": false,
	},
	shortFlags:  "diPqt",
	shortValues: "acehlmpuvw",
}

var dockerExecOptions = dockerOptionSpec{
	longs: map[string]bool{
		"detach-keys": true, "user": true, "workdir": true,

		"detach": false, "help": false, "interactive": false,
		"privileged": false, "tty": false,
	},
	shortFlags:  "dit",
	shortValues: "euw",
}

var dockerComposeRunOptions = dockerOptionSpec{
	longs: map[string]bool{
		"cap-add": true, "cap-drop": true, "entrypoint": true, "label": true,
		"name": true, "publish": true, "pull": true, "user": true,
		"volume": true, "workdir": true,

		"build": false, "detach": false, "dry-run": false,
		"interactive": false, "no-TTY": false, "no-deps": false,
		"quiet": false, "quiet-build": false, "quiet-pull": false,
		"remove-orphans": false, "rm": false, "service-ports": false,
		"use-aliases": false,
	},
	shortFlags:  "ditTqP",
	shortValues: "elpuvw",
}

var dockerComposeExecOptions = dockerOptionSpec{
	longs: map[string]bool{
		"index": true, "user": true, "workdir": true,

		"detach": false, "dry-run": false, "no-tty": false,
		"privileged": false,
	},
	shortFlags:  "ditT",
	shortValues: "euw",
}
