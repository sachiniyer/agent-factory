package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file models three env-injection launchers — sudo, docker and
// systemd-run — precisely enough that the account-environment guard refuses
// them only when the visible command line assigns, or might assign, a name
// the account scope protects. The guard's property is narrowness: a launcher
// that injects environment into its child is refused on that spelling and
// any other use runs exactly as it did before the launcher was modeled, so
// `sudo apt-get install -y jq`, `docker compose up` and
// `systemd-run --user --scope make` stay admitted while `sudo
// CODEX_HOME=/other codex`, `docker run -e CODEX_HOME=/other busybox codex`
// and `systemd-run --setenv CODEX_HOME=/other codex` refuse.
//
// The hole (#7565): each launcher runs a child command and can place a
// NAME=value into that child's environment, re-rooting the account's
// credential directory. The unrecognized-wrapper tail scan treated a
// positional `NAME=value` as inert data, and the three launcher names were
// not modeled at all, so every spelling passed. Listing them as
// argv-passthrough wrappers (refused wholesale) closed the hole but refused
// every `docker compose up` and `sudo apt-get install`, a user-facing
// regression.
//
// Each unwrapX below walks the launcher's own option region and stops at the
// boundary where child argv begins, the way the util-linux unwraps do:
//
//   - A word carrying the launcher's env-injection spelling is refused when
//     its operand names a denied variable (or names one the operand grammar
//     normalizes to, e.g. `NAME+=`), and refused outright when it hands the
//     launcher a value the command line cannot prove (an env file).
//   - Every other option is consumed by arity from the launcher's real
//     option table; an unknown, ambiguous or unprovable option word fails
//     closed because it can shift the boundary to hide an env operand.
//   - A value-taking option's separate operand stays a command-head
//     candidate for the shadowed reading (wrapperOperandTailMutates), the
//     convention the nice/timeout/ionice unwraps established — a PATH-
//     shadowed or repo-local binary can discard a different count and exec
//     at a different boundary.
//   - The returned tail is the command head plus its argv. The argv gets
//     the ordinary inert tail scan — not the buried-suffix scan — because
//     a `.` or `up` there is the child's own operand (`docker build .`),
//     not a shadowed executable boundary.

// unwrapSudo walks sudo's option and environment-assignment region: getopt
// permutation lets sudo's options and the positional `VAR=value` words
// interleave freely until the command word, and `--` ends both regions at
// once (verified on sudo 1.9.15p5 — `sudo -- FOO=bar env` tries to exec a
// program named "FOO=bar" and fails, so the post-`--` word is the command,
// not an assignment).
//
// sudo applies a pre-command word containing `=` to the child's environment
// under a deliberately loose rule — even `FOO-BAR=x`, which is not a valid
// shell name, lands in the environment verbatim. Every such word is judged
// by accountEnvironmentOperandDenied, which cuts at the first `=` and
// strips a trailing `+`, so `CODEX_HOME+=/other` is refused whether the
// real sudo applies it verbatim or a shadowed sudo reads it as bash's
// append-assignment. An assignment-shaped word whose name is not denied
// (`PORT=3000`) is consumed as sudo's env text exactly as sudo does.
//
// `-E` and `--preserve-env[=LIST]` carry no injection of their own: they
// propagate the caller's environment — which af has already scoped to the
// selected account — so a denied name in the preserve list only hands the
// child the same pinned value it would see anyway. sudo's env_reset is
// sudo's own semantics, not an assignment on the command line, and is out
// of this check's contract.
// requireCommand is envCallMutatesAccountEnvironment's rule applied to the
// launchers' own denied spellings: at command position (the peel arms) a
// denied env spelling refuses on sight, but inside an unrecognized
// wrapper's tail the launcher is only a hypothesis — the words might be
// argv text — so a denied spelling refuses only when the launcher's own
// parse still reaches a command it could inject into. `prog sudo
// CODEX_HOME=/other` admits the same way `prog env CODEX_HOME=/other`
// does: with no command after it, even the real binary only errors.
// Unverifiable words (a dynamic option, an unreadable operand, an env file)
// refuse under either rule, exactly as env's own scan does.
func unwrapSudo(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	run := memo.optionRun("sudo")
	denied := false
	for len(words) > 0 {
		if cached, seen := run.visit(words, requireCommand); seen {
			return run.done(cached.words, cached.unsafe)
		}
		literal, ok := literalShellWord(words[0])
		if !ok {
			// A non-literal word can still name its assignment target when the
			// `NAME=` text is literal (`CODEX_HOME=$V`); only the value is
			// dynamic, so judging the name stays exact.
			if name, assignment := shellWordAssignmentName(words[0]); assignment {
				if accountEnvironmentOperandDenied(name, names) {
					if !requireCommand {
						return run.done(nil, true)
					}
					denied = true
				}
				words = words[1:]
				continue
			}
			// Anything else could be an option, an env assignment, or the
			// command word — and could word-split into several.
			return run.done(nil, true)
		}
		switch {
		case literal == "--":
			return run.done(words[1:], requireCommand && denied && len(words) > 1)
		case strings.HasPrefix(literal, "-") && literal != "-":
			next, unsafe := sudoOptionWordStep(words, names, memo)
			if unsafe {
				return run.done(nil, true)
			}
			words = words[next:]
		case strings.Contains(literal, "="):
			if accountEnvironmentOperandDenied(literal, names) {
				if !requireCommand {
					return run.done(nil, true)
				}
				denied = true
			}
			words = words[1:]
		default:
			return run.done(words, requireCommand && denied)
		}
	}
	return run.done(nil, false)
}

// sudoOptionWordStep consumes one literal dash-word of sudo's option region
// and returns the index it stops at, or unsafe.
func sudoOptionWordStep(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) (int, bool) {
	literal, _ := literalShellWord(words[0])
	if strings.HasPrefix(literal, "--") {
		name, _, attached := strings.Cut(literal[2:], "=")
		resolved, requiresArg, known := resolveLongOption(name, sudoLongOptions)
		if !known {
			return 0, true
		}
		// --preserve-env's list is optional and only ever attached; a
		// separate word after it is the command, never the list.
		if resolved == "preserve-env" || !requiresArg || attached {
			return 1, false
		}
		return launcherSeparateOperandStep(words, 0, names, memo)
	}
	cluster := literal[1:]
	for i := 0; i < len(cluster); i++ {
		switch {
		case strings.IndexByte(sudoShortFlags, cluster[i]) >= 0:
		case cluster[i] == 'h':
			// -h takes its host operand only attached (getopt `h::`); a
			// bare trailing -h is --help, a flag.
			if i+1 < len(cluster) {
				return 1, false
			}
		case strings.IndexByte(sudoShortValueFlags, cluster[i]) >= 0:
			if i+1 < len(cluster) {
				// The operand rides inside this word (-uroot); the
				// boundary cannot move past it.
				return 1, false
			}
			return launcherSeparateOperandStep(words, 0, names, memo)
		default:
			return 0, true
		}
	}
	return 1, false
}

// launcherSeparateOperandStep consumes an option's separate-word operand at
// words[i+1]: provably one argv word (a double-quoted scalar is; an
// unquoted expansion can word-split and shift the child boundary), and
// judged as a possible command head for the shadowed reading, the
// convention unwrapIonice set. It returns the next index to read.
func launcherSeparateOperandStep(words []*syntax.Word, i int, names map[string]struct{}, memo operandTailMemo) (int, bool) {
	if i+1 >= len(words) {
		return 0, true
	}
	if _, literal := literalShellWord(words[i+1]); !literal &&
		!isSimpleQuotedParameterWord(words[i+1]) {
		return 0, true
	}
	if wrapperOperandTailMutates(words[i+1:], names, memo) {
		return 0, true
	}
	return i + 2, false
}

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
const sudoShortFlags = "ABbEHikKNnPsSve"
const sudoShortValueFlags = "CDgpRrTtUu"

// unwrapSystemdRun walks systemd-run's option region to its COMMAND word.
// The env-injection spellings are `-E`/`--setenv` (NAME[=VALUE], attached or
// separate — a bare NAME copies the caller's value, so a denied name there
// is the same re-root) and `-p`/`--property` (NAME=VALUE unit properties,
// among which `Environment=` builds the child's environment wholesale,
// `EnvironmentFile=` reads a file the command line cannot prove, and
// `UnsetEnvironment=` can drop a pin). `PassEnvironment=` is admitted: it
// copies the caller's values, which are the ones af scoped.
//
// A separate-word env or property operand must be literal to judge —
// `$V` can spell anything — while an ordinary option operand only needs to
// be provably one word. Every other word is consumed by the systemd 255
// table; systemd-run's getopt_long resolves unambiguous abbreviations, so
// `--setenv`'s own `-E` spelling exists in long form too (`--set=...`
// resolves to setenv).
func unwrapSystemdRun(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	run := memo.optionRun("systemd-run")
	denied := false
	for len(words) > 0 {
		if cached, seen := run.visit(words, requireCommand); seen {
			return run.done(cached.words, cached.unsafe)
		}
		literal, ok := literalShellWord(words[0])
		if !ok {
			return run.done(nil, true)
		}
		switch {
		case literal == "--":
			return run.done(words[1:], requireCommand && denied && len(words) > 1)
		case strings.HasPrefix(literal, "--"):
			next, unsafe := systemdRunLongOptionStep(words, names, memo, requireCommand, &denied)
			if unsafe {
				return run.done(nil, true)
			}
			words = words[next:]
		case strings.HasPrefix(literal, "-") && literal != "-":
			next, unsafe := systemdRunClusterStep(words, names, memo, requireCommand, &denied)
			if unsafe {
				return run.done(nil, true)
			}
			words = words[next:]
		default:
			return run.done(words, requireCommand && denied)
		}
	}
	return run.done(nil, false)
}

func systemdRunLongOptionStep(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, denied *bool) (int, bool) {
	literal, _ := literalShellWord(words[0])
	name, value, attached := strings.Cut(literal[2:], "=")
	resolved, requiresArg, known := resolveLongOption(name, systemdRunLongOptions)
	if !known {
		return 0, true
	}
	operand := value
	switch {
	case resolved == "setenv":
		if !attached {
			return systemdRunEnvOperandStep(words, names, requireCommand, denied)
		}
		if accountEnvironmentOperandDenied(operand, names) {
			if !requireCommand {
				return 0, true
			}
			*denied = true
		}
		return 1, false
	case strings.HasSuffix(resolved, "property"):
		if !attached {
			if len(words) < 2 {
				return 0, true
			}
			var ok bool
			operand, ok = literalShellWord(words[1])
			if !ok {
				return 0, true
			}
			if systemdRunPropertyOperandDenied(operand, names) {
				if !requireCommand {
					return 0, true
				}
				*denied = true
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return 0, true
			}
			return 2, false
		}
		if systemdRunPropertyOperandDenied(operand, names) {
			if !requireCommand {
				return 0, true
			}
			*denied = true
		}
		return 1, false
	case requiresArg:
		if attached {
			return 1, false
		}
		return launcherSeparateOperandStep(words, 0, names, memo)
	default:
		return 1, false
	}
}

func systemdRunClusterStep(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, denied *bool) (int, bool) {
	literal, _ := literalShellWord(words[0])
	cluster := literal[1:]
	for i := 0; i < len(cluster); i++ {
		c := cluster[i]
		switch {
		case strings.IndexByte(systemdRunShortFlags, c) >= 0:
		case c == 'E' || c == 'p':
			operand := cluster[i+1:]
			if operand == "" {
				if c == 'E' {
					return systemdRunEnvOperandStep(words, names, requireCommand, denied)
				}
				if len(words) < 2 {
					return 0, true
				}
				var ok bool
				operand, ok = literalShellWord(words[1])
				if !ok {
					return 0, true
				}
				if systemdRunPropertyOperandDenied(operand, names) {
					if !requireCommand {
						return 0, true
					}
					*denied = true
				}
				if wrapperOperandTailMutates(words[1:], names, memo) {
					return 0, true
				}
				return 2, false
			}
			if c == 'E' {
				if accountEnvironmentOperandDenied(operand, names) {
					if !requireCommand {
						return 0, true
					}
					*denied = true
				}
			} else if systemdRunPropertyOperandDenied(operand, names) {
				if !requireCommand {
					return 0, true
				}
				*denied = true
			}
			return 1, false
		case strings.IndexByte(systemdRunShortValueFlags, c) >= 0:
			if i+1 < len(cluster) {
				return 1, false
			}
			return launcherSeparateOperandStep(words, 0, names, memo)
		default:
			return 0, true
		}
	}
	return 1, false
}

// systemdRunEnvOperandStep judges -E's or --setenv's separate-word operand:
// `NAME` (copy the caller's value) and `NAME=value` both run through the
// shared denied-name cut. Under requireCommand a denial is deferred to the
// caller's command-boundary check rather than refusing here.
func systemdRunEnvOperandStep(words []*syntax.Word, names map[string]struct{}, requireCommand bool, denied *bool) (int, bool) {
	if len(words) < 2 {
		return 0, true
	}
	operand, ok := literalShellWord(words[1])
	if !ok {
		return 0, true
	}
	if accountEnvironmentOperandDenied(operand, names) {
		if !requireCommand {
			return 0, true
		}
		*denied = true
	}
	return 2, false
}

// systemdRunPropertyOperandDenied judges a `-p`/`--property` NAME=VALUE
// operand by the property it sets. `Environment=` composes the child's
// whole environment from space-separated (possibly quoted) NAME=value
// entries — every fragment must be an assignment to an allowed name.
// `EnvironmentFile=` reads a file this command line cannot prove, so any
// use is unverifiable. `UnsetEnvironment=` removes names — a denied name
// there drops the pin. `PassEnvironment=` copies the caller's values, which
// are the ones af already scoped, so it is admitted. systemd matches unit
// property names case-insensitively, so the property is matched by
// EqualFold.
func systemdRunPropertyOperandDenied(operand string, names map[string]struct{}) bool {
	property, value, ok := strings.Cut(operand, "=")
	if !ok {
		return true
	}
	switch {
	case strings.EqualFold(property, "Environment"):
		fragments := strings.FieldsFunc(value, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '"' || r == '\''
		})
		if len(fragments) == 0 {
			// `Environment=` empties the unit's environment — the pins go
			// with it.
			return true
		}
		for _, fragment := range fragments {
			name, _, assignment := strings.Cut(fragment, "=")
			if !assignment || accountEnvironmentOperandDenied(name, names) {
				return true
			}
		}
		return false
	case strings.EqualFold(property, "EnvironmentFile"):
		return true
	case strings.EqualFold(property, "UnsetEnvironment"):
		for _, name := range strings.Fields(value) {
			if accountEnvironmentOperandDenied(name, names) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

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
	"on-timezone-change": false, "pty": false, "quiet": false,
	"remain-after-exit": false, "same-dir": false, "scope": false,
	"send-sighup": false, "shell": false, "slice-inherit": false,
	"user": false, "version": false, "wait": false,
}

const systemdRunShortFlags = "hdGtPqrS"
const systemdRunShortValueFlags = "HMu"

// unwrapDocker walks docker's command line to the point where child argv
// begins. docker's layout is `docker [globals] [group] LEAF [leaf options]
// [boundary] [child argv]`:
//
//   - Env injection lives on the leaf commands that place a child inside a
//     container: run/create/exec carry `-e`/`--env` (attached `-eNAME=v` or
//     `--env=NAME=v`, or a separate word — a bare `-e NAME` copies the
//     caller's value, so a denied name there is the same re-root) and
//     `--env-file PATH` (compose run's `--env-from-file`, and the compose
//     `--env-add`/`--env-rm` spellings, are the same class). An env file is
//     a value the command line cannot prove, so any env-file spelling
//     refuses — the gate cannot read the file to know whether it assigns a
//     protected name.
//   - `docker compose` is special-cased before generic groups because its
//     own global `--env-file` (an interpolation source) sits between
//     `compose` and the subcommand.
//   - For run/create/exec — and compose run/exec — the leaf's option region
//     is walked with the leaf's real table (docker's parser is pflag: no
//     abbreviations), so a `-e` inside `--name`'s operand is never
//     misjudged and the first non-option word (image, container or
//     service) is the boundary where child argv begins.
//   - Every other subcommand keeps its arguments inert — `docker compose
//     up`, `docker build .`, `docker ps` — except that the same env
//     spellings are refused wherever they appear before `--`, because a
//     build, service create or compose run spelling that reaches a docker
//     new enough to honor it would still inject. A `-e` in such a tail
//     that is not denied changes nothing real (the leaf rejects it), so it
//     is consumed silently.
func unwrapDocker(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	run := memo.optionRun("docker")
	denied := false
	i := 0
	for i < len(words) {
		if cached, seen := run.visit(words[i:], requireCommand); seen {
			return run.done(cached.words, cached.unsafe)
		}
		literal, ok := literalShellWord(words[i])
		if !ok {
			return run.done(nil, true)
		}
		if literal == "--" {
			i++
			break
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			break
		}
		next, unsafe := dockerOptionStep(words, i, dockerGlobalOptions, names, memo, requireCommand, &denied)
		if unsafe {
			return run.done(nil, true)
		}
		i = next
	}
	group := ""
	if i < len(words) {
		literal, ok := literalShellWord(words[i])
		if !ok {
			return run.done(nil, true)
		}
		switch {
		case literal == "compose":
			group = "compose"
			i++
			for i < len(words) {
				literal, ok := literalShellWord(words[i])
				if !ok {
					return run.done(nil, true)
				}
				if literal == "--" {
					i++
					break
				}
				if !strings.HasPrefix(literal, "-") || literal == "-" {
					break
				}
				next, unsafe := dockerOptionStep(words, i, dockerComposeGlobalOptions, names, memo, requireCommand, &denied)
				if unsafe {
					return run.done(nil, true)
				}
				i = next
			}
		case dockerManagementGroup(literal):
			group = literal
			i++
		}
	}
	if i >= len(words) {
		return run.done(nil, false)
	}
	leaf, ok := literalShellWord(words[i])
	if !ok {
		return run.done(nil, true)
	}
	i++
	if spec, passthrough := dockerPassthroughLeaf(group, leaf); passthrough {
		for i < len(words) {
			if cached, seen := run.visit(words[i:], requireCommand); seen {
				return run.done(cached.words, cached.unsafe)
			}
			literal, ok := literalShellWord(words[i])
			if !ok {
				return run.done(nil, true)
			}
			if literal == "--" {
				return run.done(words[i+1:], requireCommand && denied && len(words[i+1:]) > 0)
			}
			if !strings.HasPrefix(literal, "-") || literal == "-" {
				// The image, container or service word: docker's parser
				// hands every later word to the child, so this is the
				// boundary where the returned tail begins.
				return run.done(words[i:], requireCommand && denied)
			}
			next, unsafe := dockerOptionStep(words, i, spec, names, memo, requireCommand, &denied)
			if unsafe {
				return run.done(nil, true)
			}
			i = next
		}
		return run.done(nil, false)
	}
	// Every other subcommand: the leaf itself is the command the launcher
	// runs, so an env spelling refuses on sight — requireCommand is already
	// satisfied and only the denied-name cut applies. The leaf stays docker
	// grammar and is never promoted into shell-command dispatch: `docker
	// history alpine` or `docker kill c1` would otherwise re-enter the walk
	// as the history/kill builtins and refuse ordinary docker verbs.
	for i < len(words) {
		if cached, seen := run.visit(words[i:], requireCommand); seen {
			return run.done(cached.words, cached.unsafe)
		}
		literal, ok := literalShellWord(words[i])
		if !ok {
			return run.done(nil, true)
		}
		if literal == "--" {
			break
		}
		next, unsafe := dockerEnvSpellingStep(words, i, names, memo)
		if unsafe {
			return run.done(nil, true)
		}
		i = next
	}
	return run.done(nil, false)
}

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
			// An unknown long option is inert: docker's parser is pflag,
			// which never abbreviates and never reads the next word as an
			// operand of a name it does not know — it errors on the word
			// itself. Consuming it as one word keeps option sets on other
			// docker versions from regressing the command.
			return i + 1, false
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
		case c == 'e' && strings.IndexByte(spec.shortValues, 'e') >= 0:
			operand := cluster[j+1:]
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
		case strings.IndexByte("diPqtT", c) >= 0:
		case c == 'e':
			operand := cluster[j+1:]
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
	shortFlags:  "diTqP",
	shortValues: "elpuvw",
}

var dockerComposeExecOptions = dockerOptionSpec{
	longs: map[string]bool{
		"index": true, "user": true, "workdir": true,

		"detach": false, "dry-run": false, "no-tty": false,
		"privileged": false,
	},
	shortFlags:  "dT",
	shortValues: "euw",
}

// launcherSuffixMutates judges a launcher word found in an unrecognized
// wrapper's tail — `prog sudo CODEX_HOME=/other` — as if the words after it
// were the launcher's command line, under the hypothesis that a shadowed
// `prog` may discard its own operands and exec there. It is the same scan
// the env word already gets in this position (requireCommand), so the
// launcher only refuses here when its denied spelling still reaches a
// command word; a bare `prog sudo CODEX_HOME=/other` — which the real
// sudo rejects with no command to run — stays admitted, matching `prog
// env CODEX_HOME=/other`.
func launcherSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	*memo.childJudgements++
	if *memo.childJudgements > shadowedChildJudgementLimit {
		return true
	}
	var tail []*syntax.Word
	var unsafe bool
	switch {
	case isAccountCommandName(words[0], "sudo"):
		tail, unsafe = unwrapSudo(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "docker"):
		tail, unsafe = unwrapDocker(words[1:], names, memo, true)
	default:
		tail, unsafe = unwrapSystemdRun(words[1:], names, memo, true)
	}
	if unsafe || len(tail) == 0 {
		return unsafe
	}
	return wrapperOperandTailMutates(tail, names, memo)
}
