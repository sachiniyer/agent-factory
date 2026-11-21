package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// account_environment_strace_test.go pins the multiword family of dynamic
// strace option words the wholly-dynamic terminal #7690 added for `"$OPT"`
// left on the suspect path. A word in a strace option run — in the option
// name, an attached operand, or a separate operand word — that can expand
// to more than one argv word can supply more options and the traced
// command, so it refuses terminally:
//
//   - a literal dash prefix or operand glued to an unquoted parameter
//     expansion (`--$OPT`, `-$OPT`, `-f$OPT`, `--en$OPT`, `-E$V`,
//     `--env=$V`, `-EFOO=$X`, `--output=$F`, `-o$LOG`) yields a non-empty
//     literal prefix with `complete == false`; at runtime the shell
//     word-splits the unquoted expansion, so both the option AND the traced
//     command can come from inside the variable even though no literal argv
//     word follows;
//   - a separate operand word that can expand to more than one argv word
//     (`-o $F`, `--output $F`, `-e $F`, `-p $P`, `-s $N`, `-P $F`,
//     `-o "$@"`) splits into the operand plus further options and the
//     command, so the run refuses before consuming it;
//   - a quoted positional-@ or whole-array expansion (`"$@"`, `"${a[@]}"`,
//     `"${!a[@]}"`, `"${!pre@}"`, `"${x:-$@}"`) yields one argv field per
//     element even inside double quotes — measured on bash — so it refuses
//     exactly like the unquoted forms;
//   - an unquoted, unescaped brace expansion (`{a,b}`, `{x..y}`) or glob
//     metacharacter (`*`, `?`, a `[` that can open a class) delivers
//     INDEPENDENT argv words — the parser keeps them inside one Lit, but
//     the shell spells several — so they refuse in every position,
//     including the first traced-command word (`-o /dev/null
//     {-ECODEX_HOME=/o,codex}` puts the -E in front of the command). A
//     glued field split (`A=$x`) is different: its literal prefix stays
//     on the first produced field, which is still the command.
//
// Only a word provably expanding to exactly one argv word is judged as a
// single word: a literal, a quoted `"$x"`/`"${x}"`/`"$*"`/`"${a[*]}"`, an
// arithmetic expansion, or a process substitution. Each suspect the fix
// removes returned a non-terminal suspect that the run fold dropped when
// the option was the last argv word — the verdict now comes from a single
// terminal check ahead of every option path, symmetric with the
// wholly-dynamic `text == ""` case and with the head-level refusal of
// `strace "$OPT" codex`.
//
// The shapes are runtime-equivalent to the literal twins the gate already
// refuses (`strace --env=CODEX_HOME=/other codex`) and to the pinned dynamic
// twins (`firejail strace "$OPT" codex`), but were ADMITTED before the fix
// because the suspect fold drops the suspect when no literal command word
// follows the option in the original shell AST. Verified through both
// `commandMutatesAccountEnvironment` and the end-to-end
// `ValidateAccountEnvironmentCommand`.

func dashGluedNames() map[string]struct{} {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	return names
}

// TestNestedStraceDashGluedDynamicOptionWordRefused pins the four reported
// exploit shapes and their siblings: a dash-glued unquoted expansion in a
// nested strace's option position under any unmodeled wrapper
// (firejail/eatmydata/a single arbitrary word/another strace) must refuse
// through both the predicate and the end-to-end validator. The fix changes
// ONLY the "no command follows the option" case — the with-command case was
// already refused via the existing suspect+command-follows fold — so the
// command-follows controls below stay refused exactly as before.
func TestNestedStraceDashGluedDynamicOptionWordRefused(t *testing.T) {
	names := dashGluedNames()

	// Exploit shapes: a dash-glued unquoted expansion that word-splits into a
	// -E/--env option operand plus a traced command from inside the variable,
	// with NO literal command word following in the original shell AST. These
	// were ADMITTED before the fix; the bug is precisely this admission.
	refused := []string{
		// Long-option `--` prefix glued to an unquoted expansion — the
		// payload runs `--env=CODEX_HOME=/other codex` after the shell
		// word-splits the unquoted $OPT. (#5033's `!hasEq && !complete`
		// long-option path lands here.)
		`OPT='env=CODEX_HOME=/other codex'; firejail strace --$OPT`,
		// The same `-E`/`--env` payload under other unmodeled wrappers and as
		// the unquoted ${OPT} spelling (word-splits identically to $OPT).
		`OPT='env=CODEX_HOME=/other codex'; eatmydata strace --$OPT`,
		`OPT='env=CODEX_HOME=/other codex'; x strace --$OPT`,
		`OPT='env=CODEX_HOME=/other codex'; firejail strace --${OPT}`,
		// A nested strace behind another strace, behind a strace option, and
		// behind a strace flag — the inner strace option run reaches the fix
		// the same way the outermost one does.
		`OPT='env=CODEX_HOME=/other codex'; firejail strace strace --$OPT`,
		`OPT='env=CODEX_HOME=/other codex'; firejail strace -o x strace --$OPT`,
		`OPT='env=CODEX_HOME=/other codex'; firejail strace -c strace --$OPT`,
		// Multi-statement set of $OPT before the strace line; the explicit
		// assignment makes the variable's content unprovable to the static
		// walk, so the dash-glued option word it lands in is unprovable too.
		`echo hi; OPT='env=CODEX_HOME=/other codex'; firejail strace --$OPT`,
		// An abbreviation of --env glued the same way (`--en$OPT`); getopt_long
		// completes `--en` to `--env`, so the split argv spells
		// `--env=CODEX_HOME=/other codex`.
		`OPT='v=CODEX_HOME=/other codex'; firejail strace --en$OPT`,
		// An unknown long-option name glued to an unquoted expansion carrying
		// the env payload — the unknown-name path with !complete is the same
		// word-splitting risk as the unknown-abbreviation shape.
		`OPT='CODEX_HOME=/other codex'; firejail strace --unknown=$OPT`,
		// Short cluster glued to an unquoted expansion. Empty cluster
		// (`-$OPT`) and known-flag-only cluster (`-f$OPT`) both fall through
		// the cluster loop into the fall-through suspect, and the runtime
		// splits the variable into the option operand plus the command.
		`OPT='E=CODEX_HOME=/other codex'; firejail strace -$OPT`,
		`OPT='ECODEX_HOME=/other codex'; firejail strace -f$OPT`,
		`OPT='ECODEX_HOME=/other codex'; firejail strace -ff$OPT`,
		// A benign-looking payload still refuses: the gate can see only the
		// unquoted expansion's name ($OPT), and word splitting can promote
		// anything inside $OPT to either an option or the traced command, so
		// the option identity is unprovable. Pin this so a future change that
		// tried to "un-refuse" a benign-looking payload cannot re-open the
		// bug: the conservative reading is the sound one.
		`OPT='f -o /tmp/x codex'; firejail strace -$OPT`,
		// An unknown short letter glued to an unquoted expansion — the loop
		// breaks at the unplaced letter and the cluster fall-through's
		// !complete branch refuses.
		`OPT='CODEX_HOME=/other codex'; firejail strace -Q$OPT`,
		// A benign-looking -Q payload still refuses for the same reason.
		`OPT='f -o /tmp/x codex'; firejail strace -Q$OPT`,
		// A KNOWN non-env long option with an unquoted (field-splittable)
		// attached operand (`--output=$F`). The operand word-splits: with
		// `F='trace -E CODEX_HOME=/other codex'` the argv becomes
		// `--output=trace`, `-E`, `CODEX_HOME=/other`, `codex`, and strace
		// processes the `-E` before launching the literal command. Only the
		// quoted `--output="$F"` form is non-splittable and stays admitted.
		`F='trace -E CODEX_HOME=/other codex'; firejail strace --output=$F codex`,
		`F='trace -E CODEX_HOME=/other codex'; firejail strace --output=$F`,
		// A KNOWN non-env short option with an unquoted (field-splittable)
		// attached operand (`-o$LOG`) — the short twin of `--output=$F`:
		// `-o$LOG` with `LOG='x -E CODEX_HOME=/other codex'` splits into
		// `-ox`, `-E`, `CODEX_HOME=/other`, `codex`.
		`LOG='x -E CODEX_HOME=/other codex'; firejail strace -o$LOG codex`,
		`LOG='x -E CODEX_HOME=/other codex'; firejail strace -o$LOG`,
		// The -E/--env attached operand, same class: `-E$V` and `--env=$V`
		// returned the env-operand suspect, which the run fold dropped the
		// same way. With `V='CODEX_HOME=/o codex'` the split argv is
		// `-ECODEX_HOME=/o` plus the traced command.
		`V='CODEX_HOME=/o codex'; firejail strace -E$V`,
		`V='CODEX_HOME=/o codex'; firejail strace --env=$V`,
		`V='CODEX_HOME=/o codex'; firejail strace -fE$V`,
		// A benign-looking literal env assignment prefix does not help:
		// `--env=FOO=$X` splits into `--env=FOO=ok` plus `-E`,
		// `CODEX_HOME=/other` and the traced command — the same bypass the
		// second Codex P1 reports.
		`X='ok -E CODEX_HOME=/other codex'; firejail strace --env=FOO=$X`,
		`X='ok -E CODEX_HOME=/other codex'; firejail strace -EFOO=$X`,
		// A SEPARATE operand word that can expand to more than one argv word
		// (`-o $F`, `--output $F`, `-e $F`, `-p $P`, `-s $N`, `-P $F`): the
		// option consumes the first field as its operand and strace parses
		// the rest — including `-E`/`--env` and the traced command — as its
		// own argv. The run refused none of these before the fix because the
		// operand word was skipped by arity rather than judged.
		`F='x -E CODEX_HOME=/o codex'; firejail strace -o $F`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace -o $F codex`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace --output $F`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace -e $F`,
		`P='1234 -E CODEX_HOME=/o codex'; firejail strace -p $P`,
		`N='8 -E CODEX_HOME=/o codex'; firejail strace -s $N`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace -P $F`,
		// A QUOTED multiword expansion in the option name — `"$@"` and the
		// whole-array forms yield one argv field per element even inside
		// double quotes, so `--"$@"` expands to `--env=CODEX_HOME=/o` plus
		// `codex` exactly like the unquoted `--$OPT`.
		`set -- env=CODEX_HOME=/o codex; firejail strace --"$@"`,
		`firejail strace --"$@"`,
		`firejail strace -"$@"`,
		// Attached operands carrying a quoted multiword expansion refuse the
		// same way: `-E"$@"` and `--env="${a[@]}"` inject the same
		// `-E`/`--env` argv words as their unquoted twins.
		`firejail strace -E"$@"`,
		`firejail strace --env="${a[@]}"`,
		`firejail strace -o"$@"`,
		// The separate-operand twin: `-o "$@"` consumes the first field as
		// the output name and parses the rest as options plus the command.
		`firejail strace -o "$@"`,
		// A value-position nested multiword expansion — `"${x:-$@}"` expands
		// to one field per positional (measured on bash), so it refuses the
		// same way. Pattern-position sub-words (`"${y#$@}"`) produce one
		// field and stay admitted below.
		`firejail strace --"${x:-$@}"`,
		// The operand check fires before the `--` separator is reached: the
		// split fields land between `-o` and the separator, so the run
		// refuses at the operand word rather than reading `--` as the end
		// of the options.
		`F='x -E CODEX_HOME=/o codex'; firejail strace -o $F -- codex`,
		// An incomplete short cluster keeps the non-terminal suspect: a
		// quoted scalar glued after known short flags is provably ONE argv
		// word, but its dynamic tail is parsed as more option letters
		// inside that word — `-f"$OPT"` with OPT=ECODEX_HOME=/o expands to
		// `-fECODEX_HOME=/o`, a single word carrying the env operand — so a
		// command word after it still refuses.
		`OPT='ECODEX_HOME=/o'; firejail strace -f"$OPT" codex`,
		`OPT='ECODEX_HOME=/o'; firejail strace -ff"$OPT" codex`,
		`OPT='ECODEX_HOME=/o'; firejail strace -"$OPT" codex`,
		// An UNQUOTED UNESCAPED BRACE EXPANSION delivers several argv words
		// from one Lit: `-o {/dev/null,-ECODEX_HOME=/o,codex}` expands to
		// `-o /dev/null -ECODEX_HOME=/o codex` — the env operand and the
		// traced command both come from the braces. The same hole exists in
		// the option name (`-{E,o}`), an attached operand
		// (`-E{CODEX_HOME=/o,x}`), a separate operand, and the first
		// traced-command word (`-o /dev/null {-E…,codex}`) — real strace
		// proves `strace -o {/dev/null,-ECODEX_HOME=/pwned,printenv}
		// CODEX_HOME` prints /pwned.
		`firejail strace -o {/dev/null,-ECODEX_HOME=/o,codex}`,
		`firejail strace -e {/dev/null,-ECODEX_HOME=/o,codex}`,
		`firejail strace -s {/dev/null,-ECODEX_HOME=/o,codex}`,
		`firejail strace --output {/dev/null,-ECODEX_HOME=/o,codex}`,
		`firejail strace -o /dev/null {-ECODEX_HOME=/o,codex}`,
		`firejail strace -e /dev/null {-ECODEX_HOME=/o,codex}`,
		`firejail strace strace -o {/dev/null,-ECODEX_HOME=/o,codex}`,
		`firejail strace strace -o /dev/null {-ECODEX_HOME=/o,codex}`,
		`firejail strace {-E,CODEX_HOME=/o,codex}`,
		`firejail strace -{E,o}`,
		`firejail strace -E{CODEX_HOME=/o,x}`,
		`firejail strace --env={CODEX_HOME=/o,x}`,
		`firejail strace --{env,output}={CODEX_HOME=/o,/tmp/x}`,
		`firejail strace -o {a..z}`,
		`firejail strace -o x{,/tmp/a}`,
		// An UNQUOTED GLOB metacharacter lets pathname expansion deliver the
		// options and the command: a worktree file named `-ECODEX_HOME=…`
		// satisfies `-*`, and `*` hands strace whatever the directory
		// holds. Quoted or escaped copies stay literal.
		`firejail strace -o -*`,
		`firejail strace -o *`,
		`firejail strace -o ?`,
		`firejail strace -o [a-z]*`,
		`firejail strace -o*.log`,
		`firejail strace -o -* codex`,
		`firejail strace *`,
		`firejail strace -o /dev/null -*`,
	}
	for _, command := range refused {
		require.True(t, commandMutatesAccountEnvironment(command, names),
			"command %q hides a -E/--env mutation in a dash-glued word and must be refused", command)
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must be refused by the account-environment guard", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}

	// Controls: the gate already refused these literal / pinned-dynamic
	// shapes — with a command word following the option, the suspect+command
	// fold refused the dash-glued variant too, and the wholly-dynamic strict
	// terminal refused the quoted twin. The fix does NOT move these; they
	// stay refused.
	alreadyRefused := []string{
		// Literal twins the gate always refused.
		`firejail strace -E CODEX_HOME=/other codex`,
		`firejail strace -E CODEX_HOME codex`,
		`firejail strace --env=CODEX_HOME=/other codex`,
		`firejail strace --env CODEX_HOME=/other codex`,
		`firejail strace "-ECODEX_HOME=/other" codex`,
		`firejail strace "--env=CODEX_HOME=/other" codex`,
		// Wholly-dynamic quoted twins #7690 already refused.
		`firejail strace "$OPT" codex`,
		`firejail strace "$OPT"`,
		`firejail strace ${OPT} codex`,
		`firejail strace -f "$OPT" codex`,
		`firejail strace -c strace "$OPT" codex`,
		`firejail strace $(O) codex`,
		// Dash-glued WITH codex following — already refused via the
		// suspect+command-follows fold; my fix preserves this. The verdict
		// moves from non-terminal-suspect to terminal at the dash-glued word,
		// so the same REFUSE verdict is reached earlier in the run.
		`OPT='env=CODEX_HOME=/other codex'; firejail strace --$OPT codex`,
		`OPT='E=CODEX_HOME=/other codex'; firejail strace -$OPT codex`,
		`OPT='ECODEX_HOME=/other codex'; firejail strace -f$OPT codex`,
		`OPT='v=CODEX_HOME=/other codex'; firejail strace --en$OPT codex`,
		// Head-level strace is refused wholesale via
		// isUnmodeledArgvPassthroughWrapper before any option walk; the
		// dash-glued shape is unreachable, but the verdict stays REFUSE.
		`OPT='env=CODEX_HOME=/other codex'; strace --$OPT`,
		`nice strace --env=CODEX_HOME=/other codex`,
		// More multiword shapes the review enumerates — refused on the
		// previous head through the word-level check, kept refused here:
		// `-fo$F`, `-e$F` and `--trace=$T` carry an unquoted attached
		// operand the same way `-o$LOG` does, and the nested strace judges
		// its own option run.
		`F='x -E CODEX_HOME=/o codex'; firejail strace -fo$F`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace -e$F`,
		`T='read -E CODEX_HOME=/o codex'; firejail strace --trace=$T`,
		`F='x -E CODEX_HOME=/o codex'; firejail strace strace -o$F`,
	}
	for _, command := range alreadyRefused {
		require.True(t, commandMutatesAccountEnvironment(command, names),
			"control command %q must stay refused", command)
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "control command %q must stay refused", command)
	}

	// Benign shapes: no dash-glued UNQUOTED expansion in a nested strace's
	// option position, so they stay ADMITTED — the fix introduces no false
	// positive. A quoted (non-splittable) operand of a KNOWN non-env option
	// expands to a single argv word, so it keeps its admission the way #7690
	// already promised (`-o"$LOG"`, `--output="$F"`); an unquoted
	// (field-splittable) operand refuses (see the refused list above).
	admitted := []string{
		// No options at all and no dynamic content.
		`firejail strace codex`,
		`firejail strace codex extra-arg`,
		// A literal strace option run with no dash-glued dynamic content.
		`firejail strace -o x codex`,
		`firejail strace -o "$FILE" codex`,
		`firejail strace -o"$FILE" codex`,
		`firejail strace -ff -o x codex`,
		`firejail strace -p 1234 codex`,
		`firejail strace -f codex`,
		`firejail strace -ff codex`,
		`firejail strace -c codex`,
		`firejail strace -- codex`,
		`firejail strace -o ./out.trace --trace=openat codex`,
		// A KNOWN non-env long option with a QUOTED (non-splittable) operand
		// — the bug report's `--output="$F"` form. A quoted expansion does
		// not field-split, so the operand expands to a single argv word and
		// the known non-env option cannot place a variable; it stays
		// admitted. (The unquoted `--output=$F` twin refuses — see above.)
		`firejail strace --output="$F" codex`,
		`firejail strace --output="$F"`,
		// A KNOWN non-env short option with a QUOTED (non-splittable)
		// operand — the bug report's `-o"$LOG"` form, which stays admitted
		// because the quoted expansion does not field-split.
		`firejail strace -o"$LOG" codex`,
		// A separate operand word provably expanding to exactly one argv
		// word — the quoted `"$x"`, `"$FILE"` and `"$*"` forms — stays
		// admitted. ("$*" joins the positionals into ONE word; "$@" is the
		// multiword twin in the refused list above.)
		`firejail strace -o "$*" codex`,
		`firejail strace --output="$*" codex`,
		// An arithmetic expansion expands to exactly one argv word — a
		// number — so a proven-constant arithmetic operand stays admitted.
		// (A non-constant one is refused file-wide by the inverted
		// arithmetic guard, before the option run sees it.)
		`firejail strace --output=$((1+1)) codex`,
		// A positional-count expansion is one decimal word, not one field
		// per positional — `"${#@}"` is the count form of "$@", so it keeps
		// the single-word verdict.
		`firejail strace --output="${#@}" codex`,
		// The joined-value substitution operators yield one word even when
		// their value word contains "$@": `${x:=$@}` assigns and yields the
		// JOINED value, and `${x:?$@}` errors or yields the set value —
		// only `+`/`:+`/`-`/`:-` keep per-element fields (the refused
		// `"${x:-$@}"` row above).
		`firejail strace -o "${x:=$@}" codex`,
		`firejail strace -o "${x:?$@}" codex`,
		// A quoted or escaped brace/glob never expands — `'{a,b}'`,
		// `"{a,b}"`, `\{a,b}` and `'*'` are each one literal argv word.
		`firejail strace -o '{/dev/null,-ECODEX_HOME=/o,codex}'`,
		`firejail strace -o "{/dev/null,-ECODEX_HOME=/o,codex}"`,
		`firejail strace -o \* codex`,
		`firejail strace -o '*' codex`,
		`firejail strace -o '"'"'*'"'"' codex`,
		// The incomplete-cluster suspect is non-terminal: with no command
		// following, `-f"$OPT"` runs nothing and cannot inject env, so it
		// stays admitted — the same single-word-no-command shape as
		// `-"$OPT"` below.
		`OPT='ECODEX_HOME=/o'; firejail strace -f"$OPT"`,
		// A QUOTED dash-glued expansion (`--"$OPT"`, `-"$OPT"`,
		// `--unknown="$OPT"`) is incomplete (complete=false) but does NOT
		// field-split — it expands to a single argv word, so with no command
		// following strace runs nothing and cannot inject env. It keeps the
		// original non-terminal suspect, so a command word after it still
		// refuses the way `strace --env=... codex` does.
		`OPT=tips; firejail strace --"$OPT"`,
		`OPT=tips; firejail strace -"$OPT"`,
		`OPT=tips; firejail strace --unknown="$OPT"`,
		// A dynamic word AFTER the traced command is an argument, not a
		// strace option — the option run ends at the command word.
		`firejail strace codex "$OPT"`,
		`firejail strace codex --$OPT`,
		// A dash-glued word AFTER the `--` separator is the traced command's
		// argv, not a strace option. With $OPT split as `--env=CODEX_HOME=/o
		// codex`, the traced command is `--env=CODEX_HOME=/o codex` — that
		// runs a file with that exact name (which does not exist) and cannot
		// inject env the way `-E`/`--env` does, so it stays admitted.
		`firejail strace -- --$OPT codex`,
		// A nested strace whose option run is clean.
		`firejail strace strace -f codex`,
		`firejail strace strace -- codex`,
		`firejail strace -f strace -o x codex`,
		// A literal unknown short flag with a command following — complete
		// is true, so the suspect+command-follows fold refuses via a
		// non-terminal suspect; my fix leaves that case unchanged (the fix
		// is scoped to the !complete fall-through case). Verify the negative
		// here: a literal unknown cluster with NO command following stays
		// admitted exactly as before (strace with an unknown option and no
		// command runs nothing, so no env injection).
		`firejail strace -Q`,
		`firejail strace --unknown`,
		// A literal unknown long option with a known-equals sign and no
		// command also stays admitted; complete=true, so the suspect path
		// is the original one.
		`firejail strace --unknown=foo`,
	}
	for _, command := range admitted {
		require.False(t, commandMutatesAccountEnvironment(command, names),
			"benign command %q must stay admitted", command)
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.NoError(t, err, "benign command %q must stay admitted", command)
	}
}

// TestStraceOptionStepDashGluedTerminalRefusal drives straceOptionStep
// directly through the wrapperTailWordHidesAccountAssignment strace-branch
// entry, the way production code reaches it. The dash-glued shapes the bug
// admits all share a non-empty literal dash prefix and an unquoted
// (field-splittable) expansion that makes the literal prefix incomplete
// (complete=false); each must reach straceOptionRunRefused's strict terminal
// there rather than the non-terminal suspect the run fold drops. Pinned
// through the gate (commandMutatesAccountEnvironment) instead of an
// internal-symbol probe so the test tracks the contract callers actually
// rely on.
func TestStraceOptionStepDashGluedTerminalRefusal(t *testing.T) {
	names := dashGluedNames()
	// Each refused row's dash-glued word is the LAST word of `firejail strace
	// …` — exactly the shape the bug admits, because the suspect fold drops
	// it. The fix turns the suspect into a terminal so this row refuses.
	for _, command := range []string{
		// Long-option `--` / `--en` dashes glued to an unquoted expansion.
		`OPT='CODEX_HOME=/o codex'; firejail strace --$OPT`,
		`OPT='CODEX_HOME=/o codex'; firejail strace --en$OPT`,
		// Short-option `-` / `-f` / `-ff` glued to an unquoted expansion.
		`OPT='CODEX_HOME=/o codex'; firejail strace -$OPT`,
		`OPT='CODEX_HOME=/o codex'; firejail strace -f$OPT`,
		`OPT='CODEX_HOME=/o codex'; firejail strace -ff$OPT`,
		// Unknown dash-glued long and short option words.
		`OPT='CODEX_HOME=/o codex'; firejail strace --unknown=$OPT`,
		`OPT='CODEX_HOME=/o codex'; firejail strace -Q$OPT`,
		// A KNOWN non-env option with an unquoted (field-splittable) attached
		// operand (`--output=$F`, `-o$LOG`): the operand word-splits into the
		// option value plus further argv words including `-E` and the command.
		`F='trace -E CODEX_HOME=/o codex'; firejail strace --output=$F`,
		`LOG='x -E CODEX_HOME=/o codex'; firejail strace -o$LOG`,
		// A KNOWN env option with an unquoted (field-splittable) attached
		// operand — the same bypass the second Codex P1 reports through the
		// env paths' early return.
		`V='CODEX_HOME=/o codex'; firejail strace -E$V`,
		`V='CODEX_HOME=/o codex'; firejail strace --env=$V`,
		`X='ok -E CODEX_HOME=/o codex'; firejail strace --env=FOO=$X`,
		// A SEPARATE operand word that can expand to more than one argv word
		// (`-o $F`, `-o "$@"`) — the option consumes the first field and the
		// rest become options plus the traced command.
		`F='x -E CODEX_HOME=/o codex'; firejail strace -o $F`,
		`firejail strace -o "$@"`,
		// A QUOTED multiword expansion in the option name (`--"$@"`) — one
		// argv field per positional, exactly like the unquoted `--$OPT`.
		`firejail strace --"$@"`,
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names),
			"command %q lands on the terminal refusal at its dash-glued option word", command)
	}
}
