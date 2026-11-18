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
			if envSpelling, deniedEnv := systemdRunDynamicEnvName(words[0], names); envSpelling {
				if deniedEnv {
					if !requireCommand {
						return run.done(nil, true)
					}
					denied = true
				}
				words = words[1:]
				continue
			}
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
			// An `=` right after the letter is the option's separator on
			// any parser that honors it (getopt hands systemd-run `=N=v`,
			// which it rejects — but a shadowed or pflag-styled run would
			// read `N=v`), so judge the stripped operand; an emptied
			// operand falls to the separate-word step.
			operand := strings.TrimPrefix(cluster[i+1:], "=")
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
		// Same rule as sudo's region and docker's env operand: a literal
		// `NAME=` prefix pins the target (`PORT="$PORT"`), so only a word
		// that cannot name its variable stays unprovable.
		if name, assignment := shellWordAssignmentName(words[1]); assignment {
			if accountEnvironmentOperandDenied(name, names) {
				if !requireCommand {
					return 0, true
				}
				*denied = true
			}
			return 2, false
		}
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

// systemdRunDynamicEnvName reads a non-literal systemd-run word whose
// literal prefix still pins it to an env option — `--setenv=PORT="$PORT"`,
// `-EPORT="$PORT"`, `-tE=PORT="$PORT"`, `--property=Environment=PORT="$V"`.
// envSpelling is true when the prefix proves such an option, and denied
// reports whether its visible name already runs afoul of the denied-name
// cut or its operand can no longer be proven (an env-family property with a
// dynamic value expands into unprovable fragments), so the caller refuses
// rather than consumes an unreadable tail.
func systemdRunDynamicEnvName(word *syntax.Word, names map[string]struct{}) (envSpelling, denied bool) {
	prefix := launcherLiteralPrefix(word)
	eq := strings.Index(prefix, "=")
	if eq < 0 {
		return false, false
	}
	sigil, rest := prefix[:eq], prefix[eq+1:]
	switch {
	case strings.HasPrefix(sigil, "--"):
		resolved, _, known := resolveLongOption(sigil[2:], systemdRunLongOptions)
		if !known {
			return false, false
		}
		switch {
		case resolved == "setenv":
			name, _, _ := strings.Cut(rest, "=")
			if !validName(name) {
				return false, false
			}
			return true, accountEnvironmentOperandDenied(name, names)
		case strings.HasSuffix(resolved, "property"):
			return true, systemdRunDynamicPropertyDenied(rest)
		}
		return false, false
	case strings.HasPrefix(sigil, "-"):
		cluster := sigil[1:]
		for i := 0; i < len(cluster); i++ {
			c := cluster[i]
			switch {
			case strings.IndexByte(systemdRunShortFlags, c) >= 0:
			case c == 'E' || c == 'p':
				operand := cluster[i+1:]
				if operand == "" {
					// `-E=`/`-p=`: the cut `=` was the separator after
					// the letter, so the operand opens `rest`.
					operand = rest
				} else {
					// The cut `=` was inside the operand (`-EN=v`,
					// `-pName=v`): reattach it so the operand reads as
					// the launcher sees it.
					operand += "=" + rest
				}
				operand = strings.TrimPrefix(operand, "=")
				if c == 'E' {
					name, _, _ := strings.Cut(operand, "=")
					if !validName(name) {
						return false, false
					}
					return true, accountEnvironmentOperandDenied(name, names)
				}
				return true, systemdRunDynamicPropertyDenied(operand)
			default:
				// Another value letter takes the cluster's rest as its
				// own operand; an unknown letter leaves the word
				// unprovable. Either way it is not an env spelling.
				return false, false
			}
		}
	}
	return false, false
}

// systemdRunDynamicPropertyDenied judges a `-p`/`--property` operand whose
// value is only partly literal. Its property name is always literal — it
// sits before the operand's own first `=`, which the caller's prefix cut
// guarantees — but a dynamic Environment= value can expand into extra
// fragments the word does not show, and a dynamic EnvironmentFile= or
// UnsetEnvironment= operand is unprovable the same way, so the env-family
// properties refuse while any other property is inert to the child's
// environment.
func systemdRunDynamicPropertyDenied(operand string) bool {
	property, _, hasValue := strings.Cut(operand, "=")
	switch {
	case !hasValue:
		// A bare property name with a dynamic tail cannot be read.
		return true
	case strings.EqualFold(property, "Environment"),
		strings.EqualFold(property, "EnvironmentFile"),
		strings.EqualFold(property, "UnsetEnvironment"):
		return true
	}
	return false
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
		fragments, closed := systemdEnvironmentFragments(value)
		if !closed || len(fragments) == 0 {
			// An unclosed quote leaves hidden text unprovable; an empty
			// `Environment=` clears the unit's environment — the pins go
			// with it either way.
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
		names2, closed := systemdEnvironmentFragments(value)
		if !closed {
			return true
		}
		for _, name := range names2 {
			if accountEnvironmentOperandDenied(name, names) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// systemdEnvironmentFragments splits a systemd Environment= value into
// NAME=value fragments the way systemd does: whitespace separates
// assignments, but double or single quotes protect whitespace inside one
// assignment (`Environment="FOO=hello world"` is one fragment) and a
// backslash escapes the next rune. The quote characters themselves are
// stripped, matching the value the unit file would store.
func systemdEnvironmentFragments(value string) ([]string, bool) {
	var fragments []string
	var cur strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			fragments = append(fragments, cur.String())
			cur.Reset()
		}
	}
	for _, r := range value {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return fragments, quote == 0 && !escaped
}

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
			if name, envSpelling := dockerDynamicEnvName(words[i], dockerGlobalOptions); envSpelling {
				if dockerEnvNameDenied(name, names, requireCommand, &denied) {
					return run.done(nil, true)
				}
				i++
				continue
			}
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
					if name, envSpelling := dockerDynamicEnvName(words[i], dockerComposeGlobalOptions); envSpelling {
						if dockerEnvNameDenied(name, names, requireCommand, &denied) {
							return run.done(nil, true)
						}
						i++
						continue
					}
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
				if name, envSpelling := dockerDynamicEnvName(words[i], spec); envSpelling {
					if dockerEnvNameDenied(name, names, requireCommand, &denied) {
						return run.done(nil, true)
					}
					i++
					continue
				}
				return run.done(nil, true)
			}
			if literal == "--" {
				if i+1 < len(words) {
					// After `--` the first word is still the image name —
					// consume it as well; child argv begins one word later.
					return run.done(words[i+2:], requireCommand && denied)
				}
				return run.done(nil, false)
			}
			if !strings.HasPrefix(literal, "-") || literal == "-" {
				// The image, container or service word is docker config,
				// not the child executable — consume it inside docker's
				// grammar so a `docker run --rm bash` image never re-enters
				// the walk as an unproven shell. Only the words after it
				// (the container's COMMAND ARG…) form the returned tail.
				return run.done(words[i+1:], requireCommand && denied)
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
			// A dynamic env spelling still names its target from the
			// literal prefix — `--env=CODEX_HOME=$V` on a service create
			// injects the same way the literal form does — while any other
			// non-literal operand of a non-launching leaf is inert data,
			// skipped exactly like the unrecognized tail scan (`docker
			// logs "$CONTAINER"` admits). The words after it may still
			// open an env invocation.
			if name, envSpelling := dockerDynamicEnvName(words[i], dockerInertLeafSpec); envSpelling {
				if dockerEnvNameDenied(name, names, false, nil) {
					return run.done(nil, true)
				}
				i++
				continue
			}
			if memo.xargsItemFollows(words[i+1:]) ||
				envCallMutatesAccountEnvironment(words[i+1:], names, true, memo) {
				return run.done(nil, true)
			}
			i++
			continue
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
