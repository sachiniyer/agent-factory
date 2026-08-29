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
