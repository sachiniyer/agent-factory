package sessionenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The account-environment guard is a security predicate about what a shell
// does, and every other test in this package asserts that predicate against
// af's own model of a shell. That is circular where it matters most: the brace
// detector agreed with itself about `{unset,a[b} CODEX_HOME` while bash unset
// the identity variable (#4579). These two tests break the circle by asking the
// real binaries.
//
// The bash oracle is ONE-DIRECTIONAL, which is the honest shape for a
// fail-closed predicate: every command bash actually mutates the identity
// variables under must be refused, and af may refuse more (the priced class-B
// over-refusals). So the witnesses assert "bash mutates AND af refuses", and a
// separate control set — which keeps the test from passing vacuously on a
// refuse-everything model — asserts "bash does not mutate AND af accepts".

// oracleIdentityNames are the variables the oracle watches. They are observed
// in two places because a command can mutate either: in the shell that runs the
// rest of the command list, and in the environment the launched agent receives.
var oracleIdentityNames = []string{"CODEX_HOME", "OPENAI_API_KEY"}

const (
	oracleCodexHome = "/original/codex-home"
	oracleAPIKey    = "original-key"
	oracleUnset     = "__UNSET__"
)

// bashOracleHarness is a throwaway directory holding a `codex` stub that
// reports the environment it was launched with.
type bashOracleHarness struct {
	bash string
	bin  string
	work string
	env  []string
}

func newBashOracleHarness(t *testing.T) *bashOracleHarness {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not installed: %v", err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	work := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.MkdirAll(work, 0o755))

	var stub strings.Builder
	stub.WriteString("#!/bin/sh\n")
	for _, name := range oracleIdentityNames {
		stub.WriteString("printf 'CHILD:" + name + "=%s\\n' \"${" + name + "-" + oracleUnset + "}\"\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(stub.String()), 0o755))

	harness := &bashOracleHarness{
		bash: bash,
		bin:  bin,
		work: work,
		env: []string{
			"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/local/bin",
			"CODEX_HOME=" + oracleCodexHome,
			"OPENAI_API_KEY=" + oracleAPIKey,
			"HOME=" + work,
			"LC_ALL=C",
		},
	}
	// Preflight: prove the harness itself works before any row is judged by it,
	// so an environment where the stub cannot run skips instead of failing rows
	// for a reason that has nothing to do with the predicate.
	if mutated, output := harness.run(t, "codex"); mutated ||
		!strings.Contains(output, "CHILD:CODEX_HOME="+oracleCodexHome) {
		t.Skipf("bash oracle harness does not work here; `codex` produced:\n%s", output)
	}
	return harness
}

// run executes command under `bash --posix` with the identity variables set,
// and reports whether either of them reached the launched agent — or survived
// into the rest of the command list — holding anything but its original value.
func (h *bashOracleHarness) run(t *testing.T, command string) (mutated bool, output string) {
	t.Helper()
	var script strings.Builder
	script.WriteString(command)
	script.WriteString("\n")
	for _, name := range oracleIdentityNames {
		script.WriteString("printf 'SHELL:" + name + "=%s\\n' \"${" + name + "-" + oracleUnset + "}\"\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bash, "--posix", "-c", script.String())
	cmd.Dir = h.work
	cmd.Env = h.env
	raw, _ := cmd.CombinedOutput()
	output = string(raw)

	baseline := map[string]string{"CODEX_HOME": oracleCodexHome, "OPENAI_API_KEY": oracleAPIKey}
	for _, line := range strings.Split(output, "\n") {
		place, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || (place != "SHELL" && place != "CHILD") {
			continue
		}
		name, value, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		if want, watched := baseline[name]; watched && value != want {
			mutated = true
		}
	}
	return mutated, output
}

// Every command bash actually mutates the identity variables under must be
// refused. The first four rows are the #4579 measurement — an unclosed '[' used
// to switch the brace detector off for the rest of the word, so af admitted a
// command that unsets the selected account's root.
func TestValidateAccountEnvironmentCommand_BashOracleRefusesEveryMutation(t *testing.T) {
	harness := newBashOracleHarness(t)
	for _, command := range []string{
		`{unset,echo} CODEX_HOME; codex`,
		`{unset,a[b} CODEX_HOME; codex`,
		`{env,CODEX_HOME=/other[x} codex`,
		`{env,CODEX_HOME=/other} codex`,
		`unset CODEX_HOME; codex`,
		`unset OPENAI_API_KEY; codex`,
		`export CODEX_HOME=/other; codex`,
		`CODEX_HOME=/other codex`,
		`env CODEX_HOME=/other codex`,
		`env -u CODEX_HOME codex`,
		`e\nv CODEX_HOME=/other codex`,
		`env OPENAI_API_KEY=sk-stolen codex`,
	} {
		mutated, output := harness.run(t, command)
		if !assert.True(t, mutated,
			"witness %q no longer demonstrates a mutation under this bash; output:\n%s", command, output) {
			continue
		}
		assert.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"bash mutates the identity under %q, so af must refuse it", command)
	}
}

// The canary for the rule above: without it a model that refuses everything
// passes. These are commands bash leaves the identity alone under, and af
// accepts them — including the words ask 1's fix must NOT have swept up, where
// an unclosed '[' really is an ordinary character.
func TestValidateAccountEnvironmentCommand_BashOracleAcceptsBenignCommands(t *testing.T) {
	harness := newBashOracleHarness(t)
	for _, command := range []string{
		`codex`,
		`codex --flag value`,
		`codex file[unfinished`,
		`codex 'a[b'`,
		`env PORT=3000 codex`,
		`echo CODEX_HOME=/tmp >/dev/null; codex`,
		`echo file[unfinished >/dev/null; codex`,
		`nice -n 5 codex`,
		`env PORT=3000 nice -n 5 codex`,
		// The discriminating control for the #4579 fix. An unclosed '[' with
		// no brace or glob syntax after it really is an ordinary character, so
		// the repair had to keep feeding the other readers rather than refuse
		// the word outright: the cruder "refuse any unclosed '['" reading
		// refuses this one, and bash launches the agent with the identity
		// intact. Measured both ways.
		`env PORT=3000 codex file[unfinished`,
	} {
		mutated, output := harness.run(t, command)
		if !assert.False(t, mutated,
			"control %q was expected to leave the identity alone; output:\n%s", command, output) {
			continue
		}
		assert.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"bash leaves the identity alone under %q, so af must accept it", command)
	}
}

// The hazard record (Q2) claims strace has exactly two option shapes that act
// OUTSIDE the child argv, where a suffix scan cannot see them. That claim is
// about a real binary's option set, so check it against the real binary: a
// future strace that grows a third one must fail this test rather than be
// discovered by a report.
//
// What `--help` can prove and what it cannot, stated rather than assumed: it
// carries the -E/--env semantics verbatim ("put VAR=VAL in the environment for
// command" / "remove VAR from the environment for command"), and it enumerates
// the long options, so prefix resolution and completeness are checkable. It
// does NOT document that an -o operand beginning with | or ! is spawned as a
// command — that lives in strace(1) — so this test pins only that -o/--output
// exists and takes a FILE operand; the pipe-exec behaviour was verified against
// strace 6.8 by hand (#4466).
func TestStraceHazardRecordMatchesInstalledBinary(t *testing.T) {
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skipf("strace is not installed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, _ := exec.CommandContext(ctx, strace, "--help").CombinedOutput()
	help := string(raw)
	require.Contains(t, help, "--env", "strace --help did not list its options:\n%s", help)

	// 1. The two declared operand-taking spellings exist, with the semantics
	//    straceOptionMutatesAccountEnvironment gives them.
	assert.Contains(t, help, "-E VAR=VAL, --env=VAR=VAL",
		"the -E/--env injection spelling the hazard record judges")
	assert.Contains(t, help, "put VAR=VAL in the environment for command",
		"-E must still be an environment injection")
	assert.Contains(t, help, "remove VAR from the environment for command",
		"-E VAR must still REMOVE the variable — the mutation env -u makes")
	assert.Contains(t, help, "-o FILE, --output=FILE",
		"the -o/--output spelling the hazard record judges, still operand-taking")

	// 2. Completeness of the class: no OTHER option describes itself as
	//    touching the traced command's environment. The record is small
	//    because the binary's environment surface is small — if that changes,
	//    a suffix scan cannot see the new one either.
	for _, line := range strings.Split(help, "\n") {
		if !strings.Contains(line, "environment") {
			continue
		}
		assert.Contains(t, []string{
			"                 put VAR=VAL in the environment for command",
			"                 remove VAR from the environment for command",
		}, line, "an undeclared option acts on the traced command's environment: %q", line)
	}

	// 3. getopt_long prefix resolution, which the record relies on: --e/--en
	//    resolve to --env only while --env is the sole long option starting
	//    with "e", and the "output" arm is written for exactly the --output*
	//    family this enumerates.
	var startingWithE, startingWithOutput []string
	for _, field := range strings.Fields(strings.NewReplacer(",", " ", "=", " ").Replace(help)) {
		name, ok := strings.CutPrefix(field, "--")
		if !ok || name == "" {
			continue
		}
		switch {
		case strings.HasPrefix(name, "e"):
			startingWithE = append(startingWithE, name)
		case strings.HasPrefix(name, "output"):
			startingWithOutput = append(startingWithOutput, name)
		}
	}
	assert.ElementsMatch(t, []string{"env"}, unique(startingWithE),
		"--e and --en resolve to --env only while --env is the sole long option starting with e")
	assert.ElementsMatch(t,
		[]string{"output", "output-append-mode", "output-separately"}, unique(startingWithOutput),
		"the --output* family the hazard record's operand/no-operand split enumerates")
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
