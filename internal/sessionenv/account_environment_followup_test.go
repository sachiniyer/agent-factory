package sessionenv

import (
	"strings"
	"testing"
	"time"

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
// onward — so the operand stays a candidate for inspection, and a literal one
// is judged as a command head. A DYNAMIC operand is the shadowed command's
// head itself: the expansion can resolve to `env`, a same-shell builtin such
// as unset/export, or a shell reading the tail as a script — the position
// unwrappedAccountCommandMutates refuses outright — so it fails closed for
// every tail, not only the option-shaped ones (Codex on #4465: `$CLASS` may
// expand to `sh`, making `ionice -c "$CLASS" /tmp/x` the `sh /tmp/x` reading
// the literal spelling already refused).
//
// Measured on util-linux 2.39.3: an empty or unknown class exits with "unknown
// scheduling class", and an empty or unparseable mask with "failed to parse CPU
// mask"/"CPU list", both BEFORE launching anything; a valid value goes on to
// --help, -p mode, or the child. So every runtime value of a single-word operand
// leaves these no-child modes reachable — which is why the operand is allowed
// to be dynamic for the REAL binary at all, and only the shadowed reading
// refuses.
func TestValidateAccountEnvironmentCommand_SingleWordWrapperOperandsStayVisible(t *testing.T) {
	for _, command := range []string{
		// A shadowed wrapper can exec the operand onward: "$CLASS" may expand
		// to `env` and the tail to env's argv — or to `sh` and the tail to a
		// script. The head being unprovable fails all of these closed.
		"ionice -c \"$CLASS\" npm run dev",
		// taskset's mask is positional, so this applies once `--` has ended
		// option parsing and the next word is unambiguously the mask.
		"taskset -- \"$MASK\" npm run dev",
		// The option-shaped tails fail closed under either head reading.
		"ionice -c \"$CLASS\" --help",
		"ionice -c \"$CLASS\" -V",
		"ionice -c \"$CLASS\" -p 123",
		"ionice -n \"$N\" --help",
		"ionice --class \"$CLASS\" -p 123",
		"ionice --classdata \"$N\" -p 123",
		// And the empty tail: the expansion could be `sh` itself, which the
		// command walk already refuses unproven.
		"ionice -c \"$CLASS\"",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q leaves a dynamic operand the shadowed wrapper can exec as a command head", command)
	}
}

// The operand-candidate check walks the operand-onward tail once per operand,
// and the enclosing unwrap loop walks the same suffix again — without the
// operandTailMemo the walk is exponential in the depth of nested value-taking
// wrappers (Codex on #4465 measured ~3s at depth 20 and gave up at 25; this
// runs synchronously inside command validation). The memo keys each remaining
// suffix by its first element pointer, so depth 60 answers in milliseconds.
func TestValidateAccountEnvironmentCommand_NestedOperandWalkIsBounded(t *testing.T) {
	command := "nice " + strings.Repeat("-n nice ", 60) + "codex"
	start := time.Now()
	err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
	require.NoError(t, err)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("nested operand walk took %s — the suffix answers must be memoized", elapsed)
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
		// A literal PID operand must not mask a dynamic word deeper in the
		// selector tail: a shadowed wrapper can strip a different operand
		// count and exec the expansion as the command head — "$CMD"=sh runs
		// `sh /tmp/launch-agent` (Codex on #4465).
		"./ionice -p\"$PID\" 123 \"$CMD\" /tmp/launch-agent",
		"./ionice -p 123 \"$CMD\" /tmp/launch-agent",
		"ionice --pid=\"$PID\" 123 \"$CMD\" /tmp/launch-agent",
		"./taskset -p\"$PID\" 123 \"$CMD\" /tmp/launch-agent",
		"./taskset -p 0x1 123 \"$CMD\" /tmp/launch-agent",
		// Terminal-option tails are childless the same way.
		"./ionice --version 1 \"$CMD\" /tmp/launch-agent",
		"./taskset --help 1 \"$CMD\" /tmp/launch-agent",
		// An all-literal tail is not automatically safe: a shadowed wrapper can
		// discard more operands than the model did and exec the suffix that
		// begins at a modeled command, so every literal boundary of a childless
		// tail is judged as a command (Codex on #4465).
		"./ionice -p 123 xargs --process-slot-var CODEX_HOME codex",
		"./taskset -p 123 xargs --process-slot-var=CODEX_HOME codex",
		"./ionice -p 123 env CODEX_HOME=/other codex",
		"./taskset --version 1 unset CODEX_HOME",
		"./ionice -p 123 ionice -c 3 sh -c 'unset CODEX_HOME; codex'",
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

// Judging every literal suffix of a childless tail (Codex on #4465) must not
// refuse the tails the real binaries take: several PIDs, options getopt
// permutes past the selector, a path-qualified binary with an ordinary tail,
// and a terminal option on its own.
func TestValidateAccountEnvironmentCommand_ChildlessTailsStayAdmitted(t *testing.T) {
	for _, command := range []string{
		"ionice -c3 -p 101 102 103",
		"ionice -p 123 -c 3 -n 7",
		"ionice -t -p 1 2",
		"ionice -P 4242 4243",
		"taskset -a -p 0x3 1234",
		"taskset -p -c 0-3 1234",
		"./ionice -p 123 456",
		"/usr/bin/taskset -p 0x1 123",
		"ionice --version",
		"taskset --help",
		"ionice -p " + strings.TrimSpace(strings.Repeat("1 ", shadowedTailOperandLimit)),
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()), "command %q", command)
	}
}

// The every-suffix scan is quadratic in the tail's length, so a tail past the
// cap refuses rather than stalling validation: 8000 literal PIDs took 8s before
// the cap.
func TestValidateAccountEnvironmentCommand_ChildlessTailIsBounded(t *testing.T) {
	over := "./ionice -p " + strings.TrimSpace(strings.Repeat("1 ", shadowedTailOperandLimit+1))
	require.Error(t, ValidateAccountEnvironmentCommand(over, scopedProcessTabAccount()))

	huge := "./taskset -p " + strings.TrimSpace(strings.Repeat("nice ", 8000))
	start := time.Now()
	require.Error(t, ValidateAccountEnvironmentCommand(huge, scopedProcessTabAccount()))
	require.Less(t, time.Since(start), 2*time.Second, "a long childless tail must not stall validation")
}

// A self-contained option token cannot move the child boundary, so its value is
// not a command head (#4465 review). Real util-linux rejects a non-numeric
// class value before launching anything, and a shadowed wrapper that forwards
// whole words execs the token as `--classd=env`, which is not found. Only a
// script that cuts the value out of the token runs env — and that repo-local
// file needs no argv to unset the root, so refusing these would only add false
// positives. The first command is the review's exact shape, admitted on
// purpose. A token whose empty expansion would leave a SEPARATE-value option
// (`-c"$CLASS"`) can swallow the next word. It is admitted only when that
// swallow reading runs nothing, as it does before `npm` (#4460); the
// IoniceDynamicClass tests pin the live-swallow refusals.
func TestValidateAccountEnvironmentCommand_AttachedIoniceValuesAreSelfContained(t *testing.T) {
	for _, command := range []string{
		`CMD=env; ./ionice --classd="$CMD" -u CODEX_HOME codex`,
		`./ionice --classdata=env -u CODEX_HOME codex`,
		`ionice --classdata="$N" npm run dev`,
		`ionice --class="$CLASS" npm run dev`,
		`ionice --classd="$N" -p 123`,
		`ionice -c3 npm run dev`,
		`ionice -c"$CLASS" npm run dev`,
		`ionice -n"$N" npm run dev`,
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()), "command %q", command)
	}
	for _, command := range []string{
		`ionice --classdata "$N" npm run dev`,
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()), "command %q", command)
	}
}

// The setsid/stdbuf/xargs terminal-option branches (--help/--version and the
// setsid -h/-V short forms) returned nil,false, dropping every word after the
// option without inspecting it. A spelled-out mutation that each function's
// default branch refuses (setsid env CODEX_HOME=/other codex) was therefore
// ADMITTED once a terminal option preceded it — the same mutation accepted or
// rejected depending on whether the option came first. The hardened
// ionice/taskset terminal branches already inspect that tail via
// shadowedOperandTailMutates (the words after the option still get inspected as
// a command, since the basename match cannot prove this IS the real util-linux
// binary). These tests pin the three branches to that same behavior. Verified
// against the installed bash before this test was written.
func TestValidateAccountEnvironmentCommand_TerminalOptionsInspectMutatingTail(t *testing.T) {
	for _, command := range []string{
		// The three formerly-unhardened branches must now judge the same
		// spelled-out mutation their default branch already refuses.
		"setsid -h env CODEX_HOME=/other codex",
		"setsid --help env CODEX_HOME=/other codex",
		"setsid -V env CODEX_HOME=/other codex",
		"setsid --version env CODEX_HOME=/other codex",
		"setsid --version sh -c 'unset CODEX_HOME; codex'",
		"./setsid -h env CODEX_HOME=/other codex",
		"/usr/bin/setsid -V env CODEX_HOME=/other codex",
		"setsid -h env OPENAI_API_KEY=sk codex",
		"stdbuf --help env CODEX_HOME=/other codex",
		"stdbuf --version env CODEX_HOME=/other codex",
		"stdbuf --version sh -c 'unset CODEX_HOME; codex'",
		"./stdbuf --help env CODEX_HOME=/other codex",
		"stdbuf --help env OPENAI_API_KEY=sk codex",
		"xargs --help env CODEX_HOME=/other codex",
		"xargs --version env CODEX_HOME=/other codex",
		"./xargs --version env CODEX_HOME=/other codex",
		"xargs --version sh -c 'unset CODEX_HOME; codex'",
		"xargs --help env OPENAI_API_KEY=sk codex",
		// A shadowed wrapper may `shift N` past the option AND an operand, so
		// every literal suffix is judged: the mutation at a non-zero offset
		// is still refused.
		"setsid -h 123 env CODEX_HOME=/other codex",
		"stdbuf --help 123 sh -c 'unset CODEX_HOME; codex'",
		"xargs --version 123 env CODEX_HOME=/other codex",
		// Parity with the hardened siblings: their terminal branches already
		// refuse this; assert the fix did not regress them.
		"ionice --help env CODEX_HOME=/other codex",
		"taskset --version env CODEX_HOME=/other codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q hides an identity mutation behind a terminal option", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The fix above must not over-refuse. A terminal option with NO words after it
// prints help/version and exits childless on the real binary; the empty tail
// keeps the pinned no-child admission (e.g. {"setsid -h", false},
// {"stdbuf --version", false}, {"xargs --version", false}). A tail that carries
// no identity mutation must also stay allowed, since the only reading that runs
// it is a shadowed wrapper executing an ordinary command.
func TestValidateAccountEnvironmentCommand_TerminalOptionsAdmitChildlessAndCleanTail(t *testing.T) {
	for _, command := range []string{
		// No words after the option: real binary exits childless.
		"setsid -h",
		"setsid --help",
		"setsid -V",
		"setsid --version",
		"stdbuf --help",
		"stdbuf --version",
		"xargs --help",
		"xargs --version",
		"./setsid -h",
		"/usr/bin/setsid --version",
		"./stdbuf --version",
		"./xargs --help",
		// A clean (non-mutating) tail after the option stays allowed.
		"setsid -h make",
		"setsid --version env codex",
		"stdbuf --help make",
		"stdbuf --version echo hi",
		"xargs --version echo hi",
		"xargs --help env codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q has no identity mutation and must stay allowed", command)
	}
}

// The `shadowedOperandTailMutates` walk (Codex on #4465) re-checks every literal
// suffix of a returned tail as a possible exec boundary, so a shadowed
// `./ionice` or `./taskset` that does `shift N; exec "$@"` cannot bury a
// mutating command behind an opaque leaf. #4465 applied that walk to the
// process-selector and terminal-option branches but stopped short of three
// sibling branches: ionice `-t`/`--ignore`, ionice `-c`/`-n`/`--class`/
// `--classdata` value options, and `tasksetCommandAfterMask` (reached from
// bare `taskset <mask>`, `taskset -c <list>`, and `taskset --cpu-list <list>`).
// A buried two-word `xargs --process-slot-var <DENIED>` behind an opaque leaf
// (`echo`) was therefore admitted, though the attached form
// `--process-slot-var=<DENIED>` was already refused by
// `unrecognizedWrapperHidesAccountAssignment`'s `--opt=DENIED` scan. The fix
// gives those three branches the same every-suffix walk their siblings
// already perform; this test pins it.
func TestValidateAccountEnvironmentCommand_IoniceTasksetOptionValueBranchesInspectBuriedXargs(t *testing.T) {
	for _, command := range []string{
		// ionice -t/--ignore: the option takes no value, so the child starts at
		// the next word on the real binary; a shadowed `./ionice` with `shift
		// 2; exec "$@"` lands on the xargs boundary.
		"ionice -t echo xargs --process-slot-var CODEX_HOME codex",
		"ionice --ignore echo xargs --process-slot-var CODEX_HOME codex",
		"./ionice -t echo xargs --process-slot-var CODEX_HOME codex",
		// ionice -c/-n/--class/--classdata value: the option's value word is
		// consumed before the child; `shift 3; exec "$@"` lands on the xargs
		// boundary.
		"ionice -c 3 echo xargs --process-slot-var CODEX_HOME codex",
		"ionice -n 5 echo xargs --process-slot-var CODEX_HOME codex",
		"ionice --class best-effort echo xargs --process-slot-var CODEX_HOME codex",
		"ionice --classdata 4 echo xargs --process-slot-var CODEX_HOME codex",
		// `--` ends option parsing, so the child is words[1:] and a shadowed
		// wrapper can shift past it to the xargs boundary.
		"ionice -- echo xargs --process-slot-var CODEX_HOME codex",
		// taskset mask: bare `taskset <mask>` reaches tasksetCommandAfterMask
		// via the default arm; `shift 2; exec "$@"` lands on the xargs boundary.
		"taskset 0xff echo xargs --process-slot-var CODEX_HOME codex",
		"./taskset 0xff echo xargs --process-slot-var CODEX_HOME codex",
		// taskset -c/--cpu-list: consumes the option, then the list word falls
		// to default → tasksetCommandAfterMask.
		"taskset -c 0xff echo xargs --process-slot-var CODEX_HOME codex",
		"taskset --cpu-list 0xff echo xargs --process-slot-var CODEX_HOME codex",
		// The mutation lands at a non-zero offset behind the opaque leaf: a
		// shadowed wrapper may shift more than one operand, so every literal
		// suffix is judged.
		"ionice -t echo true xargs --process-slot-var CODEX_HOME codex",
		// Parity with the already-hardened selector branch: the sibling
		// `ionice -p 123` walk must refuse the identical buried tail.
		"ionice -p 123 echo xargs --process-slot-var CODEX_HOME codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q buries an identity mutation behind an opaque leaf inside an ionice/taskset option branch", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The every-suffix walk added to the ionice `-t`/`--ignore`, ionice class-value,
// and taskset mask branches must not over-refuse: a clean child tail still
// runs under the real binary, and the dynamic-value admission (#4460/#4532)
// for `-c"$CLASS"` keeps its reviewed shape — including combinations of a
// fixed option branch with a later dynamic token that the "obvious" in-branch
// walk would have broken.
func TestValidateAccountEnvironmentCommand_IoniceTasksetOptionValueBranchesAdmitCleanTail(t *testing.T) {
	for _, command := range []string{
		// No identity mutation in the tail: the real binary runs the ordinary
		// child and a shadowed wrapper execs an ordinary command.
		"ionice -t echo codex",
		"ionice -c 3 echo codex",
		"ionice -n 5 echo codex",
		"ionice --class best-effort echo codex",
		"ionice --classdata 4 echo codex",
		"taskset 0xff echo codex",
		"taskset -c 0xff echo codex",
		"taskset --cpu-list 0xff echo codex",
		// A multi-word benign child is still fine: the walk judges every
		// suffix and finds no mutation.
		"ionice -c 3 npm run dev",
		// Option composition still works after a value-taking option.
		"ionice -c 3 -n 7 npm run dev",
		// The #4460/#4532 dynamic-value admission for `-c"$CLASS"` is
		// untouched by this fix (it flows through the non-literal branch).
		`ionice -c"$CLASS" npm run dev`,
		// A fixed option branch followed by a dynamic `-c"$CLASS"` token
		// stays admitted: the walk runs at the child-return branches, after
		// the dynamic token is consumed, not inside the option branch where
		// it would fail closed on the non-literal option word.
		`ionice --ignore -c"$CLASS" npm run dev`,
		`ionice --classdata 4 -c"$CLASS" npm run dev`,
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q has no identity mutation and must stay allowed", command)
	}
}

// The child-tail branches scanned by this fix (ionice `--` and `default`, and
// tasksetCommandAfterMask) see the real util-linux binary's own argv, so they
// use shadowedChildTailMutates, which drops the childless PID bound the
// selector/terminal branches keep: a command may legitimately take any number
// of operands, so the tail's length is not an environment mutation. The
// childless scan had rejected `ionice echo a1 … a65` solely for having 65
// arguments (Codex review on #4708). A long benign child stays admitted; a
// mutation buried PAST the childless bound is still refused, because every
// literal suffix is still judged.
func TestValidateAccountEnvironmentCommand_LongChildTailStaysAdmitted(t *testing.T) {
	args := func(n int) string { return strings.TrimSpace(strings.Repeat("arg ", n)) }
	for _, command := range []string{
		// Each scanned tail exceeds the childless bound (shadowedTailOperandLimit
		// = 64) on the branch it exercises, where shadowedOperandTailMutates had
		// rejected for length alone.
		"ionice echo " + args(65),      // default branch: scans 65 operand words
		"ionice -- echo " + args(64),   // `--` branch: scans echo + 64 args = 65
		"taskset 0x1 echo " + args(65), // tasksetCommandAfterMask: scans 65 words
		"taskset -c 0-3 echo " + args(65),
		"./ionice echo " + args(65), // basename-matched shadowed form
		"ionice echo " + args(100),  // comfortably past the bound
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q is a benign long child tail whose length is not a mutation", command)
	}
	for _, command := range []string{
		// A mutation buried past the childless bound is still refused: every
		// literal suffix is still judged, so the walk reaches the xargs boundary.
		"ionice echo " + args(65) + " xargs --process-slot-var CODEX_HOME codex",
		"taskset 0x1 echo " + args(65) + " xargs --process-slot-var CODEX_HOME codex",
		"ionice -- echo " + args(64) + " xargs --process-slot-var CODEX_HOME codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q buries an identity mutation past the childless bound", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The every-suffix walk in shadowedChildTailMutates stays linear in the tail's
// length after the childless cap was dropped from the child tail (Codex on
// #4708): judging every suffix through wrapperOperandTailMutates re-scans the
// remainder through unrecognizedWrapperHidesAccountAssignment and is quadratic,
// so a long benign child argv (`ionice echo a1 … aN` with N in the thousands)
// stalls an apply/swap. The scan judges the full tail once (catching a buried
// env word, shell, or `--opt=DENIED` from any prefix) and only the suffix
// starting at a word whose own name begins a verdict (a wrapper, a direct
// account-mutating builtin, strace, or a non-literal — see
// accountChildTailSuffixStartsVerdict). A long benign argv has none of those
// words, so it does not re-walk every suffix, and 8000 operand args do not
// stall commandMutatesAccountEnvironment. The unrelated commandFeedsProvenShell
// path is bypassed here so the timing reflects the walk this fix changed.
func TestValidateAccountEnvironmentCommand_ChildTailScanStaysLinear(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for _, n := range []int{4000, 8000} {
		// A long benign child tail whose length is not a mutation: it scans
		// linearly so it does not stall validation past a generous budget.
		benign := "ionice echo " + strings.TrimSpace(strings.Repeat("arg ", n))
		start := time.Now()
		require.False(t, commandMutatesAccountEnvironment(benign, names),
			"a long benign child tail of %d operand words is not an identity mutation", n)
		require.Less(t, time.Since(start), 2*time.Second,
			"a long benign child tail must not stall commandMutatesAccountEnvironment")

		// A mutation buried past the long benign tail is still refused at linear
		// cost: the only candidate position the per-suffix pass judges is the
		// xargs itself.
		buried := benign + " xargs --process-slot-var CODEX_HOME codex"
		start = time.Now()
		require.True(t, commandMutatesAccountEnvironment(buried, names),
			"command buries an identity mutation past a long child tail")
		require.Less(t, time.Since(start), 2*time.Second,
			"a mutation buried past a long child tail must still be caught quickly")

		// A direct account-mutating builtin buried past the long benign tail is
		// also refused: it is a candidate suffix position, so it is judged.
		buriedBuiltin := "ionice echo " + strings.TrimSpace(strings.Repeat("arg ", n)) +
			" unset CODEX_HOME"
		require.True(t, commandMutatesAccountEnvironment(buriedBuiltin, names),
			"command buries a direct account-mutating builtin past a long child tail")
	}
}
