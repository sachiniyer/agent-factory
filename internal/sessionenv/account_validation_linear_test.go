package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// countLiteralShellWordCalls runs fn with literalShellWordWork counting and
// returns the count. The walk literalizes the word at every step, so this is
// its work in word visits — a deterministic stand-in for time, which a
// wall-clock budget cannot tell apart from a slow runner. Past limit the count
// fails the test instead of letting a superlinear walk run for minutes (on
// master, 512 nested env words would never finish).
func countLiteralShellWordCalls(t *testing.T, limit int, fn func()) int {
	t.Helper()
	counter := &workCounter{limit: limit}
	literalShellWordWork = counter
	defer func() { literalShellWordWork = nil }()
	exceeded := func() (exceeded bool) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered != errWorkLimitExceeded {
					panic(recovered)
				}
				exceeded = true
			}
		}()
		fn()
		return false
	}()
	require.False(t, exceeded, "the walk passed its budget of %d word visits", limit)
	return counter.calls
}

// TestAccountValidationWorkIsLinearInWordCount pins #4966's property: the
// account-command walk visits each word a bounded number of times, so growing
// a command 4x grows the work about 4x. On master the env families doubled per
// added env word (exponential) and the long argument list grew 16x per 4x
// (quadratic, via fileHasProvenShell), so both fail this ratio at once.
func TestAccountValidationWorkIsLinearInWordCount(t *testing.T) {
	const small, large = 512, 2048
	// maxVisitsPerWord is generous: every family measures 2-62 visits per
	// word, flat from 250 to 2000 words. It catches a large constant sneaking
	// in; the ratio below is what catches superlinear growth.
	const maxVisitsPerWord = 100
	families := []struct {
		name    string
		command func(n int) string
		refused bool
	}{
		{"echo env … env x", func(n int) string { return "echo " + strings.Repeat("env ", n) + "x" }, false},
		{"env … env x", func(n int) string { return strings.Repeat("env ", n) + "x" }, false},
		{"strace env … env x", func(n int) string { return "strace " + strings.Repeat("env ", n) + "x" }, false},
		{"env … env unset CODEX_HOME", func(n int) string { return strings.Repeat("env ", n) + "unset CODEX_HOME" }, true},
		{"echo a … a", func(n int) string { return "echo" + strings.Repeat(" a", n) }, false},
		{"echo a … a /bin/sh -i", func(n int) string { return "echo" + strings.Repeat(" a", n) + " /bin/sh -i" }, false},
		{"echo env -u env … x", func(n int) string { return "echo env" + strings.Repeat(" -u env", n) + " x" }, false},
		{"env -u env env -u env … x", func(n int) string { return strings.Repeat("env -u env ", n) + "x" }, false},
		{"echo A=$x … x", func(n int) string { return "echo" + strings.Repeat(" A=$x", n) + " x" }, false},
		{"echo env -v env -v … x", func(n int) string { return "echo" + strings.Repeat(" env -v", n) + " x" }, false},
		{"strace -E x … x", func(n int) string { return "strace" + strings.Repeat(" -E x", n) + " x" }, false},
		{"nohup … nohup x", func(n int) string { return strings.Repeat("nohup ", n) + "x" }, false},
	}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			work := func(n int) int {
				command := family.command(n)
				return countLiteralShellWordCalls(t, maxVisitsPerWord*n, func() {
					err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
					if family.refused {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				})
			}
			smallWork, largeWork := work(small), work(large)
			require.LessOrEqual(t, largeWork, 5*smallWork,
				"4x the words took %d/%d = %.1fx the work; linear is ~4x",
				largeWork, smallWork, float64(largeWork)/float64(smallWork))
		})
	}
}

// TestFileHasProvenShellWorkIsLinear isolates the stdin half: it asks
// accountShellCommandWordsProven about every suffix of every call, and each
// ask must cost O(1) unless the suffix is already the trusted form's length.
func TestFileHasProvenShellWorkIsLinear(t *testing.T) {
	work := func(n int) int {
		command := "echo" + strings.Repeat(" /bin/bash", n)
		return countLiteralShellWordCalls(t, 100*n, func() {
			require.False(t, commandFeedsProvenShell(command))
		})
	}
	smallWork, largeWork := work(1000), work(4000)
	require.LessOrEqual(t, largeWork, 5*smallWork,
		"4x the words took %d/%d = %.1fx the work; linear is ~4x",
		largeWork, smallWork, float64(largeWork)/float64(smallWork))
}
