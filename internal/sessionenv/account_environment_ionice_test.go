package sessionenv

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/syntax"
)

// ioniceValueWitnesses are literal words tried as the value an empty
// `-c"$C"` / `-n"$N"` would swallow. The oracle test runs each one through the
// real binary; the table test states the verdict af gives it.
var ioniceValueWitnesses = []string{
	"0", "2", "3", "4", "9", "02", "2t", "+2", " 2", "2 ", "-2", "x2", "",
	"idle", "IDLE", "Idle", "best-effort", "Best-Effort", "best", "idl", "idler",
	"none", "realtime", "\u017fone", "\uff12",
	"-1", "+3", " 5", "\t7", "5x", "x", "-t", "--", "- 3", "+-3", "99999999999",
	"codex", "npm", "-n", "-c", "--help", "/bin/true",
}

// TestIoniceValueCheckIsSoundAgainstTheRealBinary runs every witness through
// util-linux ionice, with -t so that a refused ioprio_set cannot hide a live
// reading. If ionice ran the child, af must have called the value possibly
// valid. The converse is not required: calling a value valid when ionice would
// reject it only costs a refusal.
func TestIoniceValueCheckIsSoundAgainstTheRealBinary(t *testing.T) {
	path, err := exec.LookPath("ionice")
	if err != nil {
		t.Skip("util-linux ionice not on PATH; TestIoniceValueCheckTable covers the rule here")
	}
	child, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no `true` to run as the child")
	}
	started := 0
	for _, flag := range []byte{'c', 'n'} {
		for _, value := range ioniceValueWitnesses {
			ran := exec.Command(path, "-"+string(flag), value, "-t", child).Run() == nil
			if !ran {
				continue
			}
			started++
			var afSaysValid bool
			if flag == 'c' {
				afSaysValid = ioniceClassMayBeValid(value, true)
			} else {
				afSaysValid = ioniceClassDataMayBeValid(value, true)
			}
			require.Truef(t, afSaysValid,
				"ionice -%c %q ran its child, but af called the value invalid, so an empty %q value would be wrongly admitted",
				flag, value, "-"+string(flag)+`"$V"`)
		}
	}
	require.NotZero(t, started, "no witness started the child, so this oracle checked nothing")
	t.Logf("%d of %d witness runs started the child; af called every one of those values valid",
		started, 2*len(ioniceValueWitnesses))
}

// TestIoniceValueCheckTable pins the verdicts, including the deliberate
// over-approximations and the partial-prefix rules the binary cannot exercise.
func TestIoniceValueCheckTable(t *testing.T) {
	for _, tc := range []struct {
		prefix   string
		complete bool
		class    bool
		data     bool
	}{
		// Whole literal values. Measured: ionice accepts every "true" here
		// except where noted.
		{"2", true, true, true},
		{"02", true, true, true},
		{"4", true, true, true},
		{"2t", true, true, true}, // ionice rejects both; af over-approximates
		{"+2", true, false, true},
		{" 2", true, false, true},
		{"-2", true, false, true}, // -n -2: ioprio_set fails; with -t it runs
		{"idle", true, true, false},
		{"IdLe", true, true, false},
		{"best-effort", true, true, false},
		{"best", true, false, false},
		{"idler", true, false, false},
		{"\u017fone", true, false, false}, // strcasecmp folds ASCII only
		{"\uff12", true, false, false},    // a fullwidth 2 is not an ASCII digit
		{"", true, false, false},
		{"-", true, false, false},
		{"--", true, false, false},
		{"-t", true, false, false},
		{"+-3", true, false, false},
		{"codex", true, false, false},
		// Known prefixes of a longer, partly dynamic value.
		{"", false, true, true},
		{"id", false, true, false},
		{"BEST-", false, true, false},
		{"2", false, true, true},
		{"x", false, false, false},
		{"-n", false, false, false},
		{"-c", false, false, false},
		{"+", false, false, true},
		{"  ", false, false, true},
		{"-", false, false, true},
		{"./bin/", false, false, false},
	} {
		require.Equalf(t, tc.class, ioniceClassMayBeValid(tc.prefix, tc.complete),
			"class verdict for %q (complete=%v)", tc.prefix, tc.complete)
		require.Equalf(t, tc.data, ioniceClassDataMayBeValid(tc.prefix, tc.complete),
			"class-data verdict for %q (complete=%v)", tc.prefix, tc.complete)
	}
}

// TestPlainLiteralPrefix pins what counts as text the shell passes unchanged.
func TestPlainLiteralPrefix(t *testing.T) {
	for _, tc := range []struct {
		word     string
		prefix   string
		complete bool
	}{
		{`idle`, "idle", true},
		{`'idle'`, "idle", true},
		{`"2"`, "2", true},
		{`id"le"'x'`, "idlex", true},
		{`id"$X"`, "id", false},
		{`"$X"`, "", false},
		{`$X`, "", false},
		{`\2`, "", false},
		{`"\2"`, "", false},
		{`{2,x}`, "", false},
		{`[2]`, "", false},
		{`?dle`, "", false},
		{`*`, "", false},
		{`~2`, "", false},
		{`a~b`, "a~b", true},
		{`$'2'`, "", false},
		{`$"2"`, "", false},
		{`-n"$N"`, "-n", false},
	} {
		word := parseSingleWordForTest(t, tc.word)
		prefix, complete := plainLiteralPrefix(word.Parts)
		require.Equalf(t, tc.prefix, prefix, "prefix of %s", tc.word)
		require.Equalf(t, tc.complete, complete, "completeness of %s", tc.word)
	}
}

func parseSingleWordForTest(t *testing.T, source string) *syntax.Word {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader("x "+source), "")
	require.NoError(t, err, source)
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	require.True(t, ok, source)
	require.Len(t, call.Args, 2, source)
	return call.Args[1]
}
