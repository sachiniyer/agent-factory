package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// bash's `printf` builtin takes its assignment target from a `-v name` option,
// parses EVERY `-v` it sees, and applies the LAST one. A benign first `-v A`
// pair therefore masks a later `-v CODEX_HOME` that retargets the assignment at
// a denied variable — `printf -v A -v CODEX_HOME '%s' /other` assigns
// CODEX_HOME, not A (verified on real bash: GNU bash 5.2.37 sets CODEX_HOME and
// leaves A empty). The account-boundary gate's printf predicate judged only the
// FIRST `-v <name>` pair and stopped, so the equivalent single-`-v` form
// `printf -v CODEX_HOME /other` was refused while the repeated-`-v` bypass was
// admitted — letting a sibling pane re-point the selected account's identity.
//
// The fix makes printfMutatesAccountEnvironment scan the whole option stream the
// way waitMutatesAccountEnvironment does for `wait -p`: each literal `-v <name>`
// pair is judged and consumed, and the scan keeps going because a later `-v`
// retargets the assignment. A non-literal word reached while options are still
// open fails closed — an expansion can yield `-v` and retarget at a denied name
// (verified: `X=-v; printf -v A $X CODEX_HOME y` sets CODEX_HOME=y), exactly the
// dynamic retarget `wait`'s option loop refuses.

// TestValidateAccountEnvironmentCommand_RefusesPrintfRepeatedV pins the
// repeated-`-v` bypass the report enumerated: a benign first pair masking a
// later denied target, three-`-v` chains, every peeled wrapper, the nested
// `bash -c`/`sh -c` payload, and the dynamic-retarget fail-closed. Controls
// confirm the single-`-v` form, the reversed order, and the attached `-vX`
// spelling stay refused.
func TestValidateAccountEnvironmentCommand_RefusesPrintfRepeatedV(t *testing.T) {
	for _, command := range []string{
		// Controls: already refused before the fix (single `-v`, reversed
		// order, attached `-vX` whose target cannot be proven).
		"printf -v CODEX_HOME other; make",
		"printf -v CODEX_HOME -v A x; codex",
		"printf -vA x; codex",
		"printf -vA -v CODEX_HOME x; codex",
		// The reported bypass — a benign first `-v` pair masks a later denied
		// one (last `-v` wins). All now refused.
		"printf -v A -v CODEX_HOME '%s' /other; codex",
		"printf -v a -v CODEX_HOME x; codex",
		// A shell-startup name retargets the sibling's sourced startup file;
		// a credential name retargets the identity.
		"printf -v A -v BASH_ENV '%s' /tmp/evil; codex",
		"printf -v A -v OPENAI_API_KEY '%s' sk-attacker; codex",
		// Three `-v` pairs: a denied last target mutates, and a denied MIDDLE
		// target fails closed (any denied `-v` is unsafe to admit).
		"printf -v A -v B -v CODEX_HOME x; codex",
		"printf -v A -v CODEX_HOME -v B x; codex",
		// Every modelled wrapper that unwraps to the printf builtin.
		"command printf -v a -v CODEX_HOME x; codex",
		"builtin printf -v a -v CODEX_HOME x; codex",
		"nice printf -v a -v CODEX_HOME x; codex",
		"nohup printf -v a -v CODEX_HOME x; codex",
		"exec printf -v a -v CODEX_HOME x; codex",
		// The nested `-c` payload decode re-judges the inner line and refuses
		// the same shape past the shell layer.
		"bash -c 'printf -v A -v CODEX_HOME %s /other; codex'",
		"sh -c 'printf -v A -v CODEX_HOME %s /other; codex'",
		// Dynamic retarget: a non-literal word while the `-v` option stream is
		// still open can expand to `-v` and retarget, so the gate fails closed
		// exactly as `wait`'s option loop does.
		"printf -v A -v $TARGET x; codex",
		"printf -v A $FMT x; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must refuse the repeated-`-v` printf mutation", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q: refusal must come from the account-environment guard", command)
	}
}

// TestValidateAccountEnvironmentCommand_AdmitsPrintfRepeatedVNarrowness pins the
// shapes that must stay ADMITTED so the fix does not over-refuse ordinary printf
// use: a benign last `-v` target, a `--` that closes the option stream before a
// later literal `-v CODEX_HOME` (then a format string, not a target), a denied
// name in the FORMAT position (printed, not assigned), a dangling `-v` (a bash
// usage error), and an unrecognized option (which bash errors on).
func TestValidateAccountEnvironmentCommand_AdmitsPrintfRepeatedVNarrowness(t *testing.T) {
	for _, command := range []string{
		"printf -v A -v B x; codex",
		"printf -v A -v PORT x; codex",
		// `--` ends option parsing; the later `-v CODEX_HOME` is a format string
		// plus an argument, not an assignment target.
		"printf -v A -- -v CODEX_HOME x; codex",
		// A denied name in the FORMAT position after a benign `-v` is printed
		// into the safe target, never assigned.
		"printf -v A CODEX_HOME x; codex",
		// A dangling `-v` with no operand is a bash usage error (inert).
		"printf -v; codex",
		"printf -v A -v; codex",
		// Lone safe `-v`, no `-v`, and an unrecognized option (bash errors) stay
		// admitted.
		"printf -v A x; codex",
		"printf '%s\\n' hi; codex",
		"printf -x CODEX_HOME; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.NoError(t, err, "command %q must stay admitted (no identity mutation)", command)
	}
}
