package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An arithmetic ARRAY SUBSCRIPT is itself an arithmetic context, so an
// assignment written inside one runs when the surrounding expression is merely
// READ. `(( arr[CODEX_HOME=42] ))` assigns nothing at the top level — the walk
// sees an array-reference word, not an assignment node — yet bash 5.2.21
// evaluates the subscript and sets the exported CODEX_HOME to 42, even when
// `arr` is unset. Verified against the installed bash before this test.
func TestValidateAccountEnvironmentCommand_RefusesArithmeticSubscriptAssignment(t *testing.T) {
	for _, command := range []string{
		"(( arr[CODEX_HOME=42] )); codex",
		"(( arr[CODEX_HOME++] )); codex",
		"(( arr[OPENAI_API_KEY=1] )); codex",
		// The read can hide anywhere an arithmetic context is opened.
		"let 'arr[CODEX_HOME=42]'; codex",
		"echo $(( arr[CODEX_HOME=42] )); codex",
		"(( x = arr[CODEX_HOME=42] )); codex",
		// Nested one level deeper.
		"(( arr[other[CODEX_HOME=42]] )); codex",
		// The INVERSE of the reported case, and the one that actually escaped:
		// the denied name is the ARRAY, not the subscript. bash applies
		// CODEX_HOME[0]=1 to the exported SCALAR and converts it to an indexed
		// array, after which a child process reads CODEX_HOME as EMPTY — the
		// selected account root is not merely replaced, it is destroyed.
		"(( CODEX_HOME[0]=1 )); codex",
		"(( CODEX_HOME[0]++ )); codex",
		"(( OPENAI_API_KEY[0]=1 )); codex",
		// A dynamic assignment target is unprovable for the same reason.
		"(( ${target}=1 )); codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q assigns the selected root inside an arithmetic subscript", command)
	}
}

// ionice and taskset run an arbitrary command, exactly like nice and env do.
// This repository already models them as executable wrappers in
// session/tmux/resume.go, so a scoped command must not treat them as opaque
// leaf programs whose arguments are inert.
func TestValidateAccountEnvironmentCommand_RefusesUnprovableExecutableWrappers(t *testing.T) {
	for _, command := range []string{
		"ionice -c 3 sh -c 'unset CODEX_HOME; codex'",
		"taskset -c 0-3 sh -c 'unset CODEX_HOME; codex'",
		"ionice -c 3 unset CODEX_HOME",
		"taskset 0x1 env CODEX_HOME=/other codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q reaches the identity through a wrapper", command)
	}
}

func TestValidateAccountEnvironmentCommand_AllowsProcessOnlyWrapperModes(t *testing.T) {
	for _, command := range []string{
		"ionice -p 123",
		"ionice -p123",
		"ionice -tp 123",
		"ionice --pid 123",
		"ionice --pid=123",
		"ionice -P 123",
		"ionice --pgid 123",
		"ionice -u 1000",
		"ionice --uid 1000",
		// util-linux resolves long-option prefixes, and all three ionice selectors
		// mean "act on an already-running process", so an abbreviation that is
		// ambiguous only among THEM still launches no child. Measured on util-linux
		// 2.39.3: --pi/--pgi/--ui/--u all enter process-only mode, and --p exits
		// "ambiguous" without a child. taskset's --pid prefixes landed for the same
		// finding; these are their ionice siblings.
		"ionice --pi 123",
		"ionice --pi=123",
		"ionice --pgi 123",
		"ionice --pgi=123",
		"ionice --ui 1000",
		"ionice --ui=0",
		"ionice --u 1000",
		"ionice --p 123",
		"taskset -p 0x1 123",
		"taskset -cp 0-3 123",
		"taskset --pi 123",
		"taskset --pi 0x1 123",
		"taskset --pid 0x1 123",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"process-only command %q launches no child whose account environment could be changed", command)
	}
}

// A scheduling class, cpu mask or cpu list selects CPUs or a priority band. It
// can neither move the child boundary nor touch the child's environment, so it
// only has to be provably ONE argv word — it does not have to be literal. A
// double-quoted scalar expansion always is, even expanding empty.
//
// But ONE-word-ness is not inertness. The basename match cannot prove the
// binary is real util-linux, and a shadowed wrapper can exec the "operand"
// onward, so the operand stays a candidate for inspection: a literal operand
// is judged as a command head, and a dynamic operand is judged like the tail
// scan's unprovable words — it can expand to `env`, so the words after it are
// judged as env's argv.
//
// Measured on util-linux 2.39.3: an empty or unknown class exits with "unknown
// scheduling class", and an empty or unparseable mask with "failed to parse CPU
// mask"/"CPU list", both BEFORE launching anything; a valid value goes on to
// --help, -p mode, or the child. So every runtime value of a single-word operand
// leaves these no-child modes reachable.
func TestValidateAccountEnvironmentCommand_SingleWordWrapperOperandsStayVisible(t *testing.T) {
	for _, command := range []string{
		// `npm run dev` is a safe tail under both readings: env runs npm.
		"ionice -c \"$CLASS\" npm run dev",
		// taskset's mask is positional, so this applies once `--` has ended
		// option parsing and the next word is unambiguously the mask.
		"taskset -- \"$MASK\" npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q keeps the operand a candidate and the tail proves safe under both readings", command)
	}
	for _, command := range []string{
		// A shadowed wrapper can exec the operand onward: "$CLASS" may expand
		// to `env`, and env's parse of the option-shaped tail (--help, -V,
		// -p 123) cannot be proven safe, so these fail closed.
		"ionice -c \"$CLASS\" --help",
		"ionice -c \"$CLASS\" -V",
		"ionice -c \"$CLASS\" -p 123",
		"ionice -n \"$N\" --help",
		"ionice --class \"$CLASS\" -p 123",
		"ionice --classdata \"$N\" -p 123",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q leaves a tail env cannot prove safe under the shadowed-wrapper reading", command)
	}
}

// An operand a modeled option consumes stays a candidate for inspection: the
// basename match cannot prove the wrapper is real util-linux, and a
// repository-local or PATH-shadowed binary can parse differently and exec the
// "operand" onward — `./ionice -c env -u CODEX_HOME codex` runs `env -u
// CODEX_HOME codex` under a shadowed ionice.
func TestValidateAccountEnvironmentCommand_WrapperOperandsStayCandidates(t *testing.T) {
	for _, command := range []string{
		"./ionice -c env -u CODEX_HOME codex",
		"ionice -c env -u CODEX_HOME codex",
		"ionice -n env -u CODEX_HOME codex",
		"ionice --class env -u CODEX_HOME codex",
		"./taskset -c env CODEX_HOME=/other codex",
		"taskset env CODEX_HOME=/other codex",
		"timeout env -u CODEX_HOME codex",
		"timeout -k env -u CODEX_HOME codex",
		"nice -n env -u CODEX_HOME codex",
		"stdbuf -o env -u CODEX_HOME codex",
		"xargs -n env -u CODEX_HOME codex",
		"xargs --max-args env -u CODEX_HOME codex",
		"xargs --process-slot-var env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q hides a mutating tail behind an operand the basename-matched wrapper consumed", command)
	}
	for _, command := range []string{
		// Literal operands with benign tails stay accepted under both readings.
		"ionice -c 3 codex",
		"ionice -c env codex",
		"taskset -c 0-3 codex",
		"taskset 0x3 codex",
		"timeout 30 codex",
		"nice -n -5 codex",
		"stdbuf -oL codex",
		"xargs -n 2 codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q is safe under the real and shadowed readings alike", command)
	}
}

// A PROCESS SELECTOR carrying a quoted value needs no boundary decision at all.
// It switches the wrapper to acting on already-running processes, so the
// remaining operands are PIDs rather than a command and no expansion of the
// value can launch a child.
//
// Verified by attempting the override rather than by reading --help. On
// util-linux 2.39.3, with the value empty and valid alike:
//
//	ionice -p"$PID" env CODEX_HOME=/pwn sh -c 'echo PWNED=$CODEX_HOME'
//	  -> ionice: invalid PID argument: 'env'        (nothing printed)
//	ionice -c 2     env CODEX_HOME=/pwn sh -c 'echo PWNED=$CODEX_HOME'
//	  -> PWNED=/pwn                                  (control: a real exec)
//
// A valid PID does not help the attacker: the trailing word is read as a further
// PID, since -p takes a list. That control line is why this is decidable while
// the class options below are not — theirs genuinely execs once the value parses.
func TestValidateAccountEnvironmentCommand_QuotedProcessSelectorsLaunchNoChild(t *testing.T) {
	for _, command := range []string{
		"ionice -p\"$PID\"",
		"ionice --pid=\"$PID\"",
		"ionice --pi=\"$PID\"",
		"ionice -P\"$GROUP\"",
		"ionice --pgid=\"$GROUP\"",
		"ionice -u\"$UID\"",
		"ionice --uid=\"$UID\"",
		"ionice -tp\"$PID\"", // selector inside an argument-free cluster
		"taskset -p\"$PID\"",
		"taskset --pid=\"$PID\"",
		// A trailing PID operand stays accepted: the words after a selector
		// are still inspected, and a literal PID list judges as an
		// unrecognized literal command with nothing to refuse.
		"ionice -p\"$PID\" 456",
		"taskset -p\"$PID\" 456",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q selects processes and launches no child under any expansion", command)
	}
	for _, command := range []string{
		// A trailing command-shaped word is read as a further PID only on the
		// real util-linux binary. isAccountCommandName matches by basename,
		// which a PATH-shadowed or repo-local ionice/taskset satisfies while
		// exec'ing the tail, so the tail is still inspected — and an env
		// invocation in it is refused. The path-qualified spellings pin the
		// review finding's exact shape.
		"ionice -p\"$PID\" env CODEX_HOME=/other codex",
		"taskset -p\"$PID\" env CODEX_HOME=/other codex",
		"ionice -p env CODEX_HOME=/other codex",
		"taskset -p env CODEX_HOME=/other codex",
		"./ionice -p 123 env CODEX_HOME=/other codex",
		"./taskset -p 123 env CODEX_HOME=/other codex",
		"./ionice --pid=123 env CODEX_HOME=/other codex",
		"./taskset --pid 123 env CODEX_HOME=/other codex",
		"ionice -p 123 sh -c 'unset CODEX_HOME; codex'",
		"taskset -p 0x1 sh -c 'export CODEX_HOME=/x; codex'",
		"./ionice -h env CODEX_HOME=/other codex",
		"./taskset --help env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q hides a mutating tail behind a basename-matched selector", command)
	}
	for _, command := range []string{
		// Not a selector: an unquoted expansion word-splits, "$@" is zero-or-many,
		// and --ignore is not a selector at all.
		"ionice -p$PID --help",
		"ionice -p\"$@\" --help",
		"ionice --ignore=\"$X\" --help",
		// The class options keep their verdict: one of their readings execs.
		"ionice -c\"$CLASS\" --help",
		"ionice -n\"$N\" --help",
		"taskset -c\"$LIST\" npm",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q is not a process selector with a provable single word", command)
	}
}

// An ionice option token may carry its value as a quoted expansion and still be
// exactly ONE argv word, provided something pins the boundary for every value
// the expansion can take: a long option's literal '=', or literal value text
// after a short flag.
func TestValidateAccountEnvironmentCommand_IonicePinnedQuotedOptionTokens(t *testing.T) {
	for _, command := range []string{
		"ionice --class=\"$CLASS\" --help",
		"ionice --classdata=\"$N\" --help",
		"ionice --cla=\"$CLASS\" --help", // util-linux resolves long prefixes
		"ionice -c2\"$X\" --help",
		"ionice -n5\"$X\" --help",
		"ionice --class=\"$CLASS\" npm run dev",
		"ionice -c2\"$X\" npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q occupies one argv word for every value its expansion can take", command)
	}
	for _, command := range []string{
		// The token being understood does not exempt the child from the walk.
		"ionice --class=\"$CLASS\" env CODEX_HOME=/other codex",
		"ionice -c2\"$X\" env CODEX_HOME=/other codex",
		"ionice --class=\"$CLASS\" nohup env CODEX_HOME=/other codex",
		// NOT pinned: an empty expansion reduces `-c"$C"` to a bare `-c`, which
		// then takes the FOLLOWING word as the class and moves the child.
		// Measured on util-linux 2.39.3: with $C empty, `ionice -c"$C" /bin/echo X`
		// reports `unknown scheduling class: '/bin/echo'` and execs nothing,
		// while with $C=2 the same command prints X.
		"ionice -c\"$CLASS\" --help",
		"ionice -n\"$N\" --help",
		"ionice -c\"$CLASS\" env CODEX_HOME=/other codex",
		// Not a value-taking option, so no boundary to pin.
		"ionice --ignore=\"$X\" --help",
		"ionice -t\"$X\" --help",
		// Shapes that are not one word, or not an option token at all.
		"ionice --class=$CLASS --help",
		"ionice --class=\"$@\" --help",
		"ionice \"$X\" --help",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q is not a pinned single-word option token with a safe child", command)
	}
}

// The single-word rule is about ARITY, not trust. It must not admit a word that
// can produce a different number of argv words, and must not hide a child.
func TestValidateAccountEnvironmentCommand_SingleWordOperandRuleStaysNarrow(t *testing.T) {
	for _, command := range []string{
		// The child is still walked and still refused.
		"ionice -c \"$CLASS\" env CODEX_HOME=/other codex",
		"ionice -n \"$N\" env CODEX_HOME=/other codex",
		"ionice -c \"$CLASS\" nohup env CODEX_HOME=/other codex",
		"taskset -- \"$MASK\" env CODEX_HOME=/other codex",
		// Unquoted expansions word-split, so the boundary can shift.
		"ionice -c $CLASS --help",
		"taskset -- $MASK npm run dev",
		// "$@" expands to zero or many words.
		"ionice -c \"$@\" --help",
		"taskset -- \"$@\" npm run dev",
		// A dynamic word in OPTION position could expand to a flag such as -a,
		// which moves the mask and the child by one.
		"taskset \"$MASK\" npm run dev",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q does not prove a single unshifted boundary", command)
	}
}

// The selector-prefix rule must not swallow ionice's other long options. On
// util-linux 2.39.3, `ionice --i /bin/true` exits 0 after EXECUTING its child:
// --i resolves to --ignore, not to a selector, so its child stays inspected.
func TestValidateAccountEnvironmentCommand_IoniceNonSelectorPrefixesKeepChildVisible(t *testing.T) {
	for _, command := range []string{
		"ionice --i env CODEX_HOME=/other codex",
		"ionice --ig env CODEX_HOME=/other codex",
		"ionice --ignore env CODEX_HOME=/other codex",
		"ionice -t env CODEX_HOME=/other codex",
		"ionice -c 2 env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q executes a child whose account environment the assignment replaces", command)
	}
}

// util-linux resolves GNU long-option abbreviations, so --classd through
// --classdat all spell --classdata and take its value word the same way. A
// spelling shared only by --class and --classdata (--clas) is ambiguous to
// getopt_long, but both candidates consume exactly one value word, so the
// child boundary is provable either way — the same rule the selector-prefix
// handling applies.
func TestValidateAccountEnvironmentCommand_IoniceClassValueAbbreviations(t *testing.T) {
	for _, command := range []string{
		"ionice --classd 2 npm run dev",
		"ionice --classd=2 npm run dev",
		"ionice --classda 2 npm run dev",
		"ionice --classdat=2 npm run dev",
		"ionice --classdata 2 npm run dev",
		"ionice --classdata=2 npm run dev",
		"ionice --class 2 --classd 4 npm run dev",
		"ionice --clas 2 npm run dev",
		"ionice --clas=2 npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q schedules an ordinary child the walk can see", command)
	}
	for _, command := range []string{
		"ionice --classd 2 env CODEX_HOME=/other codex",
		"ionice --classd=2 env CODEX_HOME=/other codex",
		"ionice --classdat 2 env CODEX_HOME=/other codex",
		"ionice --clas 2 env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q must not hide the mutating child behind an abbreviated option", command)
	}
	// A value-taking abbreviation with no value word left still fails closed.
	require.Error(t, ValidateAccountEnvironmentCommand(
		"ionice --classd", scopedProcessTabAccount()),
		"a bare abbreviated value option must refuse rather than guess the boundary")
}

func TestValidateAccountEnvironmentCommand_AllowsTerminalUtilLinuxWrapperModes(t *testing.T) {
	for _, command := range []string{
		"ionice -h",
		"ionice -th",
		"ionice --help",
		"ionice --he",
		"ionice -V",
		"ionice -tV",
		"ionice --version",
		"ionice --ver",
		"taskset -h",
		"taskset -ah",
		"taskset --help",
		"taskset --he",
		"taskset -V",
		"taskset -aV",
		"taskset --version",
		"taskset --ver",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"terminal command %q launches no child whose account environment could be changed", command)
	}
}

// `wait -p VAR` names a variable to receive the job id. After the first result
// target the option scan treated a DYNAMIC word as safe, but bash keeps parsing
// options there: `x=-p` expands to a second `-p`, so the NEXT word is another
// result target. That assignment also strips the export attribute, so the child
// receives no selected root at all. Verified against the installed bash.
func TestValidateAccountEnvironmentCommand_RefusesDynamicWaitOptionAfterTarget(t *testing.T) {
	for _, command := range []string{
		`x=-p; sleep 0 & wait -p safe "$x" CODEX_HOME $!; codex`,
		`wait -p safe "$AF_OPT" CODEX_HOME`,
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q can retarget wait at the selected root", command)
	}
}

// The refusals above must stay narrow: ordinary process-tab commands that touch
// none of the identity names keep working.
func TestValidateAccountEnvironmentCommand_FollowupsStayNarrow(t *testing.T) {
	for _, command := range []string{
		"(( counter[index] ))",
		"(( arr[i=42] )); npm run dev",
		"let 'total += 1'",
		"nice -n 10 npm run dev",
		// The new wrappers must UNWRAP, not blanket-refuse: the command they
		// schedule is inspected, and an ordinary one still runs.
		"ionice -c 3 npm run dev",
		"ionice -c 3 -n 7 npm run dev",
		"ionice --class 2 --classdata 4 npm run dev",
		"taskset -c 0-3 npm run dev",
		"taskset 0x1 npm run dev",
		"ionice -c 3 taskset -c 0-3 npm run dev",
		"wait -p done_pid 12345",
		// $! is the one expansion that cannot become an option, so the common
		// job-spec form must survive the refusal above.
		"sleep 0 & wait -p done_pid $!",
		`sleep 0 & wait -p done_pid "$!"`,
		"wait",
		"npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q touches no identity name and must stay allowed", command)
	}
}
