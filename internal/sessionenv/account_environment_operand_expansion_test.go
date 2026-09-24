package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The wrapper-operand candidates (#4465) and the expansion-safe word model
// (#4466) meet at the same words: a consumed operand is judged as a command
// head, and a head is provable only when /bin/sh cannot expand it into other
// argv. An unquoted glob or brace operand can split into several words under
// bash, so the real binary's child no longer starts where the model thinks —
// those fail closed on every operand path, including the ones #4465 added.
// Several guards refuse them (the operand read, the candidate head check, and
// unwrapAccountCommand's own head read); this pins the combined verdict so no
// single one can be relaxed unnoticed.
func TestValidateAccountEnvironmentCommand_WrapperOperandsMustBeExpansionSafe(t *testing.T) {
	for _, command := range []string{
		"ionice -c 2* codex",
		"ionice --class 2* codex",
		"taskset 0{1,2} codex",
		"taskset -- 0{1,2} codex",
		"timeout -- 1* codex",
		"timeout -k 1* 10 codex",
		"nice -n 1* codex",
	} {
		assert.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q hands the wrapper an operand /bin/sh can split", command)
	}
}

// Canary for the rule above: ordinary literal operands — a class number, a
// mask, a cpu list, a duration, a niceness — are expansion-safe and keep the
// wrapper transparent to the account boundary.
func TestValidateAccountEnvironmentCommand_LiteralWrapperOperandsStayAccepted(t *testing.T) {
	for _, command := range []string{
		"ionice -c 2 codex",
		"ionice --class 2 -n 4 codex",
		"taskset 0x3 codex",
		"taskset -c 0-3 codex",
		"taskset -- 0x3 codex",
		"timeout 10s codex",
		"timeout -- 10s codex",
		"timeout -k 5 10 codex",
		"nice -n 5 codex",
	} {
		assert.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q uses only literal, expansion-safe operands", command)
	}
}
