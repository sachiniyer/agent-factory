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
// a command 4x grows the work about 4x. Before #4966 the env families doubled
// per added env word (exponential) and the long argument list grew 16x per 4x
// (quadratic, via fileHasProvenShell). Before #4968 a run of value-taking
// options whose operands are the wrapper itself (`nice -n nice -n …`) and
// xargs's per-env operand scan were quadratic.
func TestAccountValidationWorkIsLinearInWordCount(t *testing.T) {
	const small, large = 512, 2048
	// maxVisitsPerWord is generous: every family measures 2-62 visits per
	// word, flat from 250 to 2000 repetitions. It catches a large constant
	// sneaking in; the ratio below is what catches superlinear growth.
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
		// #4968: option runs whose operands start nested wrapper layers, and
		// xargs's env operand-region scan.
		{"nice -n nice -n … x", func(n int) string { return "nice -n" + strings.Repeat(" nice -n", n) + " x" }, false},
		{"timeout -k timeout -k … 1 x", func(n int) string { return "timeout -k" + strings.Repeat(" timeout -k", n) + " 1 x" }, false},
		{"stdbuf -o stdbuf -o … x", func(n int) string { return "stdbuf -o" + strings.Repeat(" stdbuf -o", n) + " x" }, false},
		{"ionice -c ionice -c … x", func(n int) string { return "ionice -c" + strings.Repeat(" ionice -c", n) + " x" }, false},
		{"xargs -E xargs -E … x", func(n int) string { return "xargs -E" + strings.Repeat(" xargs -E", n) + " x" }, false},
		{"xargs -I xargs -I … x", func(n int) string { return "xargs -I" + strings.Repeat(" xargs -I", n) + " x" }, false},
		{"xargs -n 1 xargs -n 1 … x", func(n int) string { return strings.Repeat("xargs -n 1 ", n) + "x" }, false},
		{"xargs a env … env x", func(n int) string { return "xargs a" + strings.Repeat(" env", n) + " x" }, false},
		{"xargs env xargs env … x", func(n int) string { return strings.Repeat("xargs env ", n) + "x" }, false},
		{"xargs -I{} echo env -u env … x", func(n int) string { return "xargs -I{} echo env" + strings.Repeat(" -u env", n) + " x" }, false},
		{"xargs -I{} echo env … env x", func(n int) string { return "xargs -I{} echo" + strings.Repeat(" env", n) + " x" }, false},
		{"taskset -c 1 taskset -c 1 … x", func(n int) string { return strings.Repeat("taskset -c 1 ", n) + "x" }, false},
		// #4978: the xargs input walk follows the chain through env and
		// strace and scans nested xargs option regions; every scan is
		// memoized per position (Codex on #4980: `-a xargs -a …` rescanned
		// the rest of the argv per xargs word, 5.96s at 8,000 words).
		{"xargs -I{} echo xargs -a … /tmp/f codex", func(n int) string { return "xargs -I{} echo" + strings.Repeat(" xargs -a", n) + " /tmp/f codex" }, false},
		{"xargs strace -f xargs strace -f … echo x", func(n int) string { return strings.Repeat("xargs strace -f ", n) + "echo x" }, false},
		{"xargs env A=1 strace xargs … echo x", func(n int) string { return strings.Repeat("xargs env A=1 strace ", n) + "echo x" }, false},
		{"xargs strace --fol … echo x", func(n int) string { return "xargs strace" + strings.Repeat(" --fol", n) + " echo x" }, false},
		{"xargs -I{} strace -o {} … echo x", func(n int) string { return "xargs -I{} strace" + strings.Repeat(" -o {}", n) + " echo x" }, false},
		{"xargs -I{} strace -f -f … {}", func(n int) string { return "xargs -I{} strace" + strings.Repeat(" -f", n) + " -{} echo" }, true},
	}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			work := func(n int) int {
				command := family.command(n)
				return countLiteralShellWordCalls(t, maxVisitsPerWord*len(strings.Fields(command)), func() {
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
