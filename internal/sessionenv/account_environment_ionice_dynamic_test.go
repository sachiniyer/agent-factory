package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #4460: `ionice -c"$CLASS" <cmd>` is not a self-contained token. When $CLASS
// expands empty the word is bare `-c`, and getopt takes the NEXT argv word as
// the class. ionice then goes on to run a command only if that word is a valid
// class. Measured on util-linux 2.39.3:
//
//	C=;  ionice -c"$C" /bin/echo X     -> unknown scheduling class: '/bin/echo'
//	C=2; ionice -c"$C" /bin/echo X     -> X
//	C=;  ionice -c"$C" 2 /bin/echo X   -> X    (the class swallowed "2")
//
// So the token is admitted only when the swallowing reading provably starts no
// command. Everything below must stay ALLOWED.
func TestValidateAccountEnvironmentCommand_IoniceDynamicClassCanaries(t *testing.T) {
	for _, command := range []string{
		`ionice -c"$CLASS" codex`,
		`ionice -c"$CLASS" npm run dev`,
		`ionice -c"${CLASS}" npm run dev`,
		`ionice -n"$LEVEL" npm run dev`,
		`ionice -c"$1" npm run dev`,
		// W1 is another option. As a class it is invalid, so the empty reading
		// exits and the non-empty reading keeps parsing options.
		`ionice -c"$CLASS" -t npm run dev`,
		`ionice -c"$CLASS" -n 4 npm run dev`,
		`ionice -c"$CLASS" -- npm run dev`,
		`ionice -n"$LEVEL" -c 3 npm run dev`,
		// A chain of dynamic tokens: each one's W1 starts with '-', which no
		// class or level can.
		`ionice -c"$CLASS" -n"$LEVEL" npm run dev`,
		`ionice -n"$LEVEL" -c"$CLASS" npm run dev`,
		// Nothing after the token: the empty reading exits for want of a value.
		`ionice -c"$CLASS"`,
		`ionice -n"$LEVEL"`,
		// Nested wrappers still unwrap.
		`ionice -c"$CLASS" taskset -c 0-3 npm run dev`,
		`nice -n 10 ionice -c"$CLASS" npm run dev`,
		// Words that look like class names but are not exact matches.
		`ionice -c"$CLASS" best npm run dev`,
		`ionice -c"$CLASS" idler`,
	} {
		t.Run(command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"command %q cannot change the account environment and must stay allowed", command)
		})
	}
}

// Admitting the token must never skip the command it schedules: the child is
// judged exactly as it would be after a literal `-c2`.
func TestValidateAccountEnvironmentCommand_IoniceDynamicClassStillJudgesTheChild(t *testing.T) {
	for _, command := range []string{
		`ionice -c"$CLASS" env CODEX_HOME=/other codex`,
		`ionice -c"$CLASS" sh -c 'unset CODEX_HOME; codex'`,
		`ionice -c"$CLASS" unset CODEX_HOME`,
		`ionice -n"$LEVEL" -c 3 env CODEX_HOME=/other codex`,
		`ionice -c"$CLASS" -- env CODEX_HOME=/other codex`,
		`ionice -c"$CLASS" -n"$LEVEL" env CODEX_HOME=/other codex`,
	} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"command %q changes the account environment through ionice's child", command)
		})
	}
}

// When W1 could be a valid class or level, the empty-value reading runs the
// words AFTER W1 as the command, a different child from the one the
// non-empty reading runs. These must stay REFUSED.
//
// The child is `unset CODEX_HOME` on purpose. With a literal class in front of
// W1 (`ionice -c2 2 unset CODEX_HOME`) the walker ALLOWS the command, because
// the non-empty reading runs a program named "2". Only the empty-value reading
// reaches `unset`, so each case fails only through the swallow check. An
// `env NAME=...` child would be refused by the unrecognized-wrapper scan
// either way and would prove nothing here.
func TestValidateAccountEnvironmentCommand_IoniceDynamicClassRefusesALiveSwallow(t *testing.T) {
	const child = " unset CODEX_HOME"
	for _, prefix := range []string{
		`ionice -c"$C" 2`,
		`ionice -c"$C" 0`,
		`ionice -c"$C" 02`,
		`ionice -c"$C" 9`,
		`ionice -c"$C" idle`,
		`ionice -c"$C" IDLE`,
		`ionice -c"$C" Best-Effort`,
		`ionice -c"$C" none`,
		`ionice -c"$C" realtime`,
		`ionice -c"$C" 'idle'`,
		`ionice -c"$C" "2"`,
		`ionice -n"$N" 4`,
		`ionice -n"$N" +4`,
		`ionice -n"$N" ' 4'`,
		// The shell rewrites W1 before ionice sees it: `\2` is 2, and brace or
		// glob expansion can produce a valid class.
		`ionice -c"$C" \2`,
		`ionice -c"$C" {2,x}`,
		`ionice -c"$C" [2]`,
		`ionice -c"$C" ?dle`,
		// A later dynamic token with a live W1 still refuses the whole command.
		`ionice -c"$C" -c"$D" 2`,
		`ionice -n"$N" -c"$C" idle`,
	} {
		command := prefix + child
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"command %q runs `unset` when the value expands empty", command)
		})
	}
	// W1 starts with, or could complete through, an expansion. The walker
	// refuses a dynamic command word anyway, so ioniceEmptyValueReadingLive's
	// own table pins the verdict for these. They are listed so that the
	// integration answer is recorded as well.
	for _, command := range []string{
		`ionice -c"$C" "$X" env CODEX_HOME=/other codex`,
		`ionice -c"$C" $X env CODEX_HOME=/other codex`,
		`ionice -c"$C" id"$X" env CODEX_HOME=/other codex`,
		`ionice -c"$C" 2"$X" env CODEX_HOME=/other codex`,
		`ionice -n"$N" +"$X" env CODEX_HOME=/other codex`,
	} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"command %q can run a different child when the class expands empty", command)
		})
	}
}

// Only the shape the dead-reading proof covers is admitted. Other dynamic
// option words keep failing closed exactly as before.
func TestValidateAccountEnvironmentCommand_IoniceDynamicClassStaysNarrow(t *testing.T) {
	for _, command := range []string{
		`ionice -c$CLASS npm run dev`,
		`ionice -c${CLASS} npm run dev`,
		`ionice -c"$@" npm run dev`,
		`ionice -c"${CLASS:-2}" npm run dev`,
		`ionice -c"$(printf 2)" npm run dev`,
		`ionice -c"${#CLASS}" npm run dev`,
		`ionice -c"${arr[@]}" npm run dev`,
		`ionice "-c$CLASS" npm run dev`,
		`ionice -tc"$CLASS" npm run dev`,
		`ionice -p"$PID" npm run dev`,
		`ionice "$OPT" npm run dev`,
		`ionice -\c"$CLASS" npm run dev`,
		`ionice -c$'x'"$CLASS" npm run dev`,
	} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"command %q is outside the modelled shape and must keep failing closed", command)
		})
	}
}
