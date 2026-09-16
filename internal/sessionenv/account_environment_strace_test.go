package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAccountEnvironmentCommand_RefusesUnprovableStraceChildEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"dynamic assignment word", `override=CODEX_HOME=/other; strace env "$override" codex`},
		{"short split-string", "strace env -SCODEX_HOME=/other codex"},
		{"long split-string", "strace env --split-string=CODEX_HOME=/other codex"},
		{"ANSI-C quoted assignment", "strace env $'CODEX_HOME=/other' codex"},
		{"ordinary nested env", "strace -f -o /tmp/trace env CODEX_HOME=/other codex"},
		{"strace separate env option", "strace -E CODEX_HOME=/other codex"},
		{"strace attached env option", "strace -ECODEX_HOME=/other codex"},
		{"strace long env option", "strace --env=CODEX_HOME=/other codex"},
		{"strace abbreviated long env option", "strace --en=CODEX_HOME=/other codex"},
		{"strace expression option", "strace --expr trace=all env CODEX_HOME=/other codex"},
		{"strace unsets protected env", "strace -E CODEX_HOME codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"command %q can replace the selected account environment", test.command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceUnresolvedInputsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"dynamic option operand", `strace -o "$AF_TRACE_FILE" npm run dev`},
		{"dynamic executable", `strace "$AF_TRACE_PROGRAM"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"unresolved strace input %q must not default to safe", test.command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceSelfContainedOptionsLeaveChildVisible(t *testing.T) {
	for _, option := range []string{
		"-T=ns",
		"--always-show-pid",
		"--summary",
		"--some-future-flag",
		"--some-future-flag=v",
	} {
		t.Run(option, func(t *testing.T) {
			require.NoError(t,
				ValidateAccountEnvironmentCommand(
					"strace "+option+" npm run dev",
					scopedProcessTabAccount(),
				),
				"a self-contained option cannot consume the child executable")
			require.Error(t,
				ValidateAccountEnvironmentCommand(
					"strace "+option+" env CODEX_HOME=/other codex",
					scopedProcessTabAccount(),
				),
				"a self-contained option must leave the mutating child visible")
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceSeparateValuesLeaveChildVisible(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"mutating child after literal value", "strace --columns 120 env CODEX_HOME=/other codex"},
		{"abbreviated option keeps child boundary", "strace --colum 120 env CODEX_HOME=/other codex"},
		{"current upstream option keeps child boundary", "strace --color always env CODEX_HOME=/other codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t,
				ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"a separate-value option must not lose the child boundary")
		})
	}
}

func TestValidateAccountEnvironmentCommand_RefusesExecutableStraceOutputTarget(t *testing.T) {
	for _, command := range []string{
		`strace --output='|env CODEX_HOME=/other codex' true`,
		`strace --output '!env CODEX_HOME=/other codex' true`,
		`strace -o'|env CODEX_HOME=/other codex' true`,
		`strace -o '!env CODEX_HOME=/other codex' true`,
	} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"executable strace output target %q inherits the selected account environment", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceTerminalOptionsOverrideEarlierRefusals(t *testing.T) {
	for _, command := range []string{
		"strace -E CODEX_HOME=/other --version",
		"strace --env=CODEX_HOME=/other --version",
		"strace -E CODEX_HOME=/other --help",
		"strace --env=CODEX_HOME=/other -h",
		"strace -o '|env CODEX_HOME=/other codex' -V",
		`strace -E "$SPEC" --version`,
		`strace -E "$SPEC" -h`,
		`strace --env="$ENV_CHANGE" --version`,
		`strace --output="$OUTPUT" -V`,
		`strace -o "$OUT" --help`,
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"terminal strace command %q launches no child or output helper", command)
	}

	require.NoError(t,
		ValidateAccountEnvironmentCommand("strace -E --version", scopedProcessTabAccount()),
		"a required option value consumes --version, then no child exists to receive the environment change")
}

func TestValidateAccountEnvironmentCommand_StraceShortOptionArity(t *testing.T) {
	t.Run("Y takes no value", func(t *testing.T) {
		require.Error(t,
			ValidateAccountEnvironmentCommand("strace -Y env CODEX_HOME=/other codex", scopedProcessTabAccount()),
			"argument-free -Y must not consume the env executable")
	})
	t.Run("s takes a value", func(t *testing.T) {
		require.NoError(t,
			ValidateAccountEnvironmentCommand("strace -s 256 npm run dev", scopedProcessTabAccount()),
			"value-taking -s must consume its string-limit operand")
	})
}

func TestValidateAccountEnvironmentCommand_StraceSyscallTimesOptionalValue(t *testing.T) {
	for _, command := range []string{
		"strace --syscall-times npm run dev",
		"strace --syscall-times=ns npm run dev",
	} {
		require.NoError(t,
			ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"--syscall-times takes an optional attached value and must leave the child visible")
	}

	require.Error(t,
		ValidateAccountEnvironmentCommand(
			"strace --syscall-times env CODEX_HOME=/other codex",
			scopedProcessTabAccount(),
		),
		"bare --syscall-times must not consume the env executable as its optional value")
}

func TestValidateAccountEnvironmentCommand_StraceKVMValue(t *testing.T) {
	require.NoError(t,
		ValidateAccountEnvironmentCommand(
			"strace --kvm=vcpu npm run dev",
			scopedProcessTabAccount(),
		),
		"--kvm's attached value must be consumed before inspecting the child")
}

func TestValidateAccountEnvironmentCommand_StraceStackTraceSpellings(t *testing.T) {
	for _, spelling := range []string{"--stack-trace", "--stack-traces"} {
		t.Run(spelling, func(t *testing.T) {
			require.NoError(t,
				ValidateAccountEnvironmentCommand(
					"strace "+spelling+" npm run dev",
					scopedProcessTabAccount(),
				),
				"bare %s must leave the child executable visible", spelling)
			require.NoError(t,
				ValidateAccountEnvironmentCommand(
					"strace "+spelling+"=symbol npm run dev",
					scopedProcessTabAccount(),
				),
				"%s's optional value must remain attached", spelling)
			require.Error(t,
				ValidateAccountEnvironmentCommand(
					"strace "+spelling+" env CODEX_HOME=/other codex",
					scopedProcessTabAccount(),
				),
				"bare %s must not consume the env executable", spelling)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceAlternativeSpellings(t *testing.T) {
	for _, option := range []string{"--failing-only", "--pidns-translation"} {
		require.Error(t,
			ValidateAccountEnvironmentCommand(
				"strace "+option+" env CODEX_HOME=/other codex",
				scopedProcessTabAccount(),
			),
			"argument-free alias %s must leave the child executable visible", option)
	}

	for option, value := range map[string]string{
		"--decode-pid": "comm",
		"--signals":    "none",
		"--trace-fd":   "3",
	} {
		require.NoError(t,
			ValidateAccountEnvironmentCommand(
				"strace "+option+"="+value+" npm run dev",
				scopedProcessTabAccount(),
			),
			"required-value alias %s must consume its own value", option)
	}

	for option, value := range map[string]string{
		"--daemonised": "grandchild",
		"--daemonized": "grandchild",
		"--decode-fd":  "path",
		"--silence":    "none",
		"--silent":     "none",
		"--timestamps": "time",
	} {
		require.NoError(t,
			ValidateAccountEnvironmentCommand(
				"strace "+option+"="+value+" npm run dev",
				scopedProcessTabAccount(),
			),
			"optional-value alias %s must keep an attached value", option)
		require.Error(t,
			ValidateAccountEnvironmentCommand(
				"strace "+option+" env CODEX_HOME=/other codex",
				scopedProcessTabAccount(),
			),
			"bare optional-value alias %s must leave the child executable visible", option)
	}
}

func TestValidateAccountEnvironmentCommand_StraceEquivalentAliasPrefixes(t *testing.T) {
	for option, value := range map[string]string{
		"--decode-pi": "comm",
		"--signa":     "none",
		"--trace-f":   "3",
	} {
		t.Run(option, func(t *testing.T) {
			require.NoError(t,
				ValidateAccountEnvironmentCommand(
					"strace "+option+" "+value+" npm run dev",
					scopedProcessTabAccount(),
				),
				"equivalent aliases must not make %s ambiguous", option)
			require.Error(t,
				ValidateAccountEnvironmentCommand(
					"strace "+option+" "+value+" env CODEX_HOME=/other codex",
					scopedProcessTabAccount(),
				),
				"%s must consume its value and leave the mutating child visible", option)
		})
	}
}

// Inequivalent families can share a prefix. When every candidate consumes one
// separate operand, the child sits at the same word whichever one the installed
// strace picks — and if it calls the prefix ambiguous it exits without a child
// at all — so the boundary is provable even though the option is not. `--col`
// is the reported case: strace 6.8 resolves it to --columns, while the
// cross-version table also holds --color.
//
// --env and --output are excluded: their OPERAND carries the security meaning,
// not their arity, so a prefix that could reach one of them cannot be given
// those semantics on a guess and keeps failing closed.
func TestValidateAccountEnvironmentCommand_StraceSameArityPrefixesResolve(t *testing.T) {
	for _, option := range []string{"--col", "--colu", "--colo"} {
		t.Run(option, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(
				"strace "+option+" 120 npm run dev", scopedProcessTabAccount()),
				"%s consumes one operand under every resolution, so it must not be refused", option)
			require.Error(t, ValidateAccountEnvironmentCommand(
				"strace "+option+" 120 env CODEX_HOME=/other codex", scopedProcessTabAccount()),
				"%s must still consume exactly one operand and leave the child visible", option)
		})
	}
}

// A shared prefix is only resolvable when the candidates agree. These keep
// failing closed for two distinct reasons, pinned separately so a later change
// cannot collapse them into one rule.
func TestValidateAccountEnvironmentCommand_StraceUnresolvablePrefixesFailClosed(t *testing.T) {
	for _, test := range []struct {
		command string
		reason  string
	}{
		// Reachable candidates disagree on arity: --signal consumes an operand,
		// --stack-trace-frame-limit does too, but --summary-* and terminal
		// options do not all agree, so no single boundary is provable.
		{"strace --s 1 env CODEX_HOME=/other codex", "candidates disagree"},
		// Security-sensitive candidate reachable: --e can resolve to --env, whose
		// operand is the mutation itself.
		{"strace --e=CODEX_HOME=/other codex", "--e can reach --env"},
		{"strace --o '|env CODEX_HOME=/other codex' true", "--o can reach --output"},
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
			"%q must fail closed (%s)", test.command, test.reason)
	}
}

// Resolving same-arity prefixes must not disturb the abbreviations that name
// exactly one family, including the terminal options, which have a different
// result and therefore never group.
func TestValidateAccountEnvironmentCommand_StraceSingleFamilyPrefixesUnchanged(t *testing.T) {
	for _, command := range []string{
		"strace --hel",
		"strace --vers",
		"strace --en=PORT=3000 codex",
		"strace --expr trace=all npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q names one family and must keep its existing verdict", command)
	}
	require.Error(t, ValidateAccountEnvironmentCommand(
		"strace --en=CODEX_HOME=/other codex", scopedProcessTabAccount()),
		"--en names --env alone and must still refuse a denied assignment")
}

func TestValidateAccountEnvironmentCommand_FailClosedBoundaryStaysNarrow(t *testing.T) {
	for _, command := range []string{
		"echo CODEX_HOME=/tmp",
		"rg 'OPENAI_API_KEY=x'",
		"set +e - -k; npm run dev",
		"echo env CODEX_HOME=/tmp",
		"strace npm run dev",
		"strace echo CODEX_HOME=/tmp",
		"strace rg OPENAI_API_KEY=x",
		"strace -f -o /tmp/trace npm run dev",
		"strace --follow-forks --output=/tmp/trace npm run dev",
		"strace -ff -etrace=execve npm run dev",
		"strace -E PORT=3000 npm run dev",
		"strace -- env PORT=3000 npm run dev",
		"strace -p 1234",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q uses assignment-shaped text as data, not a child-environment mutation", command)
	}
}

func TestValidateAccountEnvironmentCommand_StraceAttachPIDForms(t *testing.T) {
	options := []struct {
		name      string
		separated string
		attached  string
	}{
		{name: "short", separated: "-p ", attached: "-p"},
		{name: "clustered short", separated: "-fp ", attached: "-fp"},
		{name: "long", separated: "--attach ", attached: "--attach="},
		{name: "abbreviated long", separated: "--att ", attached: "--att="},
	}
	operands := []struct {
		name  string
		value string
	}{
		{name: "bare", value: "1234"},
		{name: "quoted", value: `"$PID"`},
	}
	for _, option := range options {
		for _, layout := range []struct {
			name   string
			prefix string
		}{
			{name: "separated", prefix: option.separated},
			{name: "attached", prefix: option.attached},
		} {
			for _, operand := range operands {
				command := "strace " + layout.prefix + operand.value
				t.Run(option.name+"/"+layout.name+"/"+operand.name, func(t *testing.T) {
					require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
						"attach-only command %q launches no child whose account environment could be changed", command)
					require.Error(t,
						ValidateAccountEnvironmentCommand(
							command+" env CODEX_HOME=/other codex",
							scopedProcessTabAccount(),
						),
						"attach option in %q must not conceal a trailing child", command,
					)
				})
			}
		}
	}

	for _, command := range []string{
		`strace -p $PID`,
		`strace -p$PID`,
		`strace -fp $PID`,
		`strace -fp$PID`,
		`strace --attach $PID`,
		`strace --attach=$PID`,
		`strace --att $PID`,
		`strace --att=$PID`,
		`strace --attach "$@"`,
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"expanding PID in %q can change the argv boundary and must fail closed", command)
	}
}

func TestValidateAccountEnvironmentCommand_StraceDynamicValueBoundaries(t *testing.T) {
	t.Run("empty attached value moves child boundary", func(t *testing.T) {
		require.Error(t,
			ValidateAccountEnvironmentCommand(
				`PID=; strace -p"$PID" 123 env CODEX_HOME=/other codex`,
				scopedProcessTabAccount(),
			),
			"an empty attached PID consumes 123, leaving env as the mutating child",
		)
	})

	for _, command := range []string{
		`strace -p"$PID" npm run dev`,
		`strace -p"$PID" 123 npm run dev`,
		`strace -p0"$PID" npm run dev`,
		`strace -e "$FILTER" npm run dev`,
		`strace -e"$FILTER" npm run dev`,
		`strace -eall"$FILTER" npm run dev`,
		`strace --expr "$FILTER" npm run dev`,
		`strace --expr="$FILTER" npm run dev`,
		`strace --columns "$AF_TRACE_COLUMNS" npm run dev`,
	} {
		t.Run("safe/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"every possible child boundary in %q is an ordinary command", command)
		})
	}

	for _, command := range []string{
		`strace -E"$ENV_CHANGE" codex`,
		`strace -o"$OUTPUT" codex`,
		`strace -e$FILTER codex`,
		`strace --expr=$FILTER codex`,
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"unprovable value in %q must remain fail-closed", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceOutputAppendModeArity(t *testing.T) {
	for _, command := range []string{
		"strace --output-append-mode append env CODEX_HOME=/other codex",
		"strace --output-append-mode env CODEX_HOME=/other codex",
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"either supported output-append-mode arity in %q reaches a mutating child", command)
		})
	}

	for _, command := range []string{
		"strace --output-append-mode npm run dev",
		"strace --output-append-mode append npm run dev",
		"strace --output-append-mode=append npm run dev",
	} {
		t.Run("safe/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"every supported output-append-mode boundary in %q reaches an ordinary command", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceUnmodeledBareOptionChecksBothBoundaries(t *testing.T) {
	for _, command := range []string{
		"strace -A append env CODEX_HOME=/other codex",
		"strace --some-future-flag value env CODEX_HOME=/other codex",
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"an unmodeled bare option in %q must expose either possible child boundary", command)
		})
	}

	for _, command := range []string{
		"strace -A npm run dev",
		"strace -A append npm run dev",
		"strace --some-future-flag npm run dev",
		"strace --some-future-flag value npm run dev",
	} {
		t.Run("safe/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"every possible child boundary in %q is an ordinary command", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceAmbiguousBoundariesHaveLinearCost(t *testing.T) {
	const optionCount = 30
	command := "strace " + strings.Repeat("--future ", optionCount) + "npm run dev"
	call, ok := singleCallIgnoringRedirections(command)
	require.True(t, ok)

	evaluation := &straceBoundaryEvaluation{}
	_, unsafe := unwrapStraceState(
		call.Args[1:],
		straceDeferredHazards{},
		map[string]struct{}{"CODEX_HOME": {}},
		evaluation,
	)
	require.False(t, unsafe)
	require.NotEmpty(t, evaluation.memo, "ambiguous suffix results must be memoized")
	require.LessOrEqual(t, len(evaluation.memo), optionCount*2,
		"each remaining-suffix boundary state should be evaluated at most once")
	require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
		"memoization must not reject a legitimate command with many self-contained options")
}

func TestValidateAccountEnvironmentCommand_StraceNoChildHazards(t *testing.T) {
	for _, command := range []string{
		"strace -E CODEX_HOME=/other -p 123",
		"strace --env=CODEX_HOME=/other --attach=123",
		`strace -ECODEX_HOME="$OTHER_HOME" -p 123`,
		`strace -E "$SPEC" -p 123`,
		`strace --env="$ENV_CHANGE" --attach=123`,
	} {
		t.Run("environment/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"environment option in attach-only command %q has no child to modify", command)
		})
	}

	for _, command := range []string{
		"strace -E CODEX_HOME=/other env PORT=3000 npm run dev",
		`strace -E "$SPEC" env PORT=3000 npm run dev`,
		"strace -o '|env CODEX_HOME=/other codex' -p 123",
		`strace -o "$OUT" -p 123`,
	} {
		t.Run("active/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"executable hazard in %q remains active", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceStaticSafeDynamicSemanticPrefixes(t *testing.T) {
	for _, command := range []string{
		`strace --env=PORT="$PORT" npm run dev`,
		`strace --env PORT="$PORT" npm run dev`,
		`strace -EPORT="$PORT" npm run dev`,
		`strace -E PORT="$PORT" npm run dev`,
		`strace --output=/tmp/trace-"$PID" npm run dev`,
		`strace --output /tmp/trace-"$PID" npm run dev`,
		`strace -o/tmp/trace-"$PID" npm run dev`,
		`strace -o /tmp/trace-"$PID" npm run dev`,
	} {
		t.Run("safe/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"literal semantic prefix in %q proves the dynamic operand safe", command)
		})
	}

	for _, command := range []string{
		`strace --env=CODEX_HOME="$OTHER_HOME" codex`,
		`strace -ECODEX_HOME="$OTHER_HOME" codex`,
		`strace --env="$ENV_CHANGE" codex`,
		`strace -E"$ENV_CHANGE" codex`,
		`strace -E "$ENV_CHANGE" codex`,
		`strace --output="$OUTPUT" codex`,
		`strace -o"$OUTPUT" codex`,
		`strace -o "$OUTPUT" codex`,
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"dynamic security-sensitive prefix in %q must fail closed", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceQuotedSelfContainedValues(t *testing.T) {
	for _, command := range []string{
		`strace --decode-fds="$SET" npm run dev`,
		`strace --some-future-flag="$VALUE" npm run dev`,
	} {
		t.Run("safe/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"the attached quoted value in %q cannot move the child boundary", command)
		})
	}

	for _, command := range []string{
		`strace --decode-fds="$SET" env CODEX_HOME=/other codex`,
		`strace --some-future-flag="$VALUE" env CODEX_HOME=/other codex`,
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"the attached quoted value in %q must leave the mutating child visible", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceQuotedScalarAttachPIDs(t *testing.T) {
	for _, option := range []struct {
		name      string
		separated string
		attached  string
	}{
		{name: "short", separated: "-p ", attached: "-p"},
		{name: "long", separated: "--attach ", attached: "--attach="},
	} {
		for _, parameter := range []string{
			`"$1"`,
			`"$!"`,
			`"$?"`,
			`"$#"`,
			`"$$"`,
			`"$-"`,
			`"$*"`,
		} {
			for _, spelling := range []string{option.separated + parameter, option.attached + parameter} {
				command := "strace " + spelling
				t.Run(option.name+"/"+spelling, func(t *testing.T) {
					require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
						"quoted scalar PID in %q remains one argv word and launches no child", command)
					require.Error(t,
						ValidateAccountEnvironmentCommand(
							command+" env CODEX_HOME=/other codex",
							scopedProcessTabAccount(),
						),
						"attach option in %q must leave any trailing mutating child visible", command,
					)
				})
			}
		}
	}
}

// A short-option cluster is attacker-sized: an account-scoped request body may
// carry ~16MiB (daemon.maxHTTPBodyBytes). The parser scanned it one stack frame
// per option byte, which overflows the goroutine stack — a FATAL runtime error
// that no recover() in the request path can contain, so the whole daemon dies
// with every live session rather than the tab request being refused. Measured
// before the fix: with an 8MiB stack the recursion died between 40k and 80k
// bytes, putting the default 1GiB limit around 7M.
//
// The bound asserted here is wall-clock and depth: a full body-sized cluster
// must parse without overflowing, and must still refuse its mutating child.
func TestValidateAccountEnvironmentCommand_StraceLargeClusterHasNoStackDepth(t *testing.T) {
	for _, size := range []int{80000, 1 << 20, 16 << 20} {
		cluster := "-" + strings.Repeat("Z", size)
		require.Error(t, ValidateAccountEnvironmentCommand(
			"strace "+cluster+" env CODEX_HOME=/other codex", scopedProcessTabAccount()),
			"a %d-byte cluster must still refuse the mutating child", size)
		require.NoError(t, ValidateAccountEnvironmentCommand(
			"strace "+cluster+" npm run dev", scopedProcessTabAccount()),
			"a %d-byte cluster must not refuse an ordinary child", size)
	}
}

// An attached quoted operand that may expand EMPTY leaves a bare option which
// consumes the following word instead. Both readings are kept, and each is
// judged by its OWN operand: on the empty reading the expansion is gone and
// words[1] is the operand, which is often provable when the expansion was not.
func TestValidateAccountEnvironmentCommand_StraceVanishingAttachedValueKeepsBothBoundaries(t *testing.T) {
	for _, command := range []string{
		// Neither reading can launch a child, so neither hazard can land.
		`strace -E"$SPEC" --version`,
		`strace -E"$SPEC" -V`,
		`strace -o"$OUT" --version`,
		`strace --env="$SPEC" --version`,
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q launches no child under either boundary", command)
	}
	for _, command := range []string{
		// A child exists under one reading or the other, so the deferred hazard
		// lands and the command is refused.
		`strace -E"$SPEC" codex`,
		`strace -o"$OUT" codex`,
		`strace -E"$SPEC" env CODEX_HOME=/other codex`,
		`strace -o"$OUT" 123 env CODEX_HOME=/other codex`,
		// An executable output target still executes in attach mode, where no
		// tracee child exists at all — the hazard a terminal option clears but
		// a childless attach must not.
		`strace -o"$OUT" -p 123`,
		`strace -o"$OUT" '|evil' -p 123`,
		`strace -o '|env CODEX_HOME=/other codex' true`,
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q reaches a child or an executable output target", command)
	}
}

// The boundary memo must be shared with every wrapper the walk reaches, not
// created per wrapper. A nested strace reached through an ambiguous boundary
// otherwise starts an empty memo, so both readings of every level re-derive the
// same suffixes and the cost doubles per level: at the per-wrapper memo, a
// 20-level, 203-byte command took 4.8s (8 levels 2ms, 16 levels 355ms), which a
// 16MiB request body can extend without bound.
//
// The assertion is that cost stays workable at a depth the exponential form
// could not reach at all, and that the verdicts are unchanged at every depth.
func TestValidateAccountEnvironmentCommand_NestedStraceWrappersShareTheBoundaryMemo(t *testing.T) {
	for _, levels := range []int{8, 20, 200, 2000} {
		nest := strings.TrimSpace(strings.Repeat("strace -Z ", levels))
		require.NoError(t, ValidateAccountEnvironmentCommand(
			nest+" npm run dev", scopedProcessTabAccount()),
			"%d nested wrappers around an ordinary child must not be refused", levels)
		require.Error(t, ValidateAccountEnvironmentCommand(
			nest+" env CODEX_HOME=/other codex", scopedProcessTabAccount()),
			"%d nested wrappers must not hide the mutating child", levels)
	}
	// Other wrappers between the strace levels reach shared states by different
	// paths; the memo keys on the argv suffix and hazards, so those must agree.
	for _, command := range []string{
		"strace -Z nohup strace -Z env CODEX_HOME=/other codex",
		"strace -Z perf strace -Z env CODEX_HOME=/other codex",
		"strace -Z env PORT=1 strace -Z env CODEX_HOME=/other codex",
		"strace -Z ionice -c 2 strace -Z env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"%q reaches a mutating child through mixed wrappers", command)
	}
	require.NoError(t, ValidateAccountEnvironmentCommand(
		"strace -Z nice strace -Z npm run dev", scopedProcessTabAccount()),
		"mixed wrappers around an ordinary child must not be refused")
}

// A request body can carry ~1M nested `strace -Z ` prefixes under the HTTP body
// limit (daemon.maxHTTPBodyBytes ~16MiB). Each level used to cost several stack
// frames, so that input overflowed the goroutine stack and killed the process
// instead of returning a verdict — daemon-fatal, not a refusal. The evaluation
// budget must refuse it closed while real nesting orders of magnitude below it
// stays proven.
func TestValidateAccountEnvironmentCommand_WrapperNestingBudgetFailsClosed(t *testing.T) {
	// Over the budget but cheap to build: every level charges at least one
	// unit, so the budget must trip far below the fatal stack depth.
	nest := strings.TrimSpace(strings.Repeat("strace -Z ", accountEnvironmentEvaluationBudget+2000))
	require.Error(t, ValidateAccountEnvironmentCommand(
		nest+" npm run dev", scopedProcessTabAccount()),
		"nesting past the evaluation budget must fail closed around an ordinary child")
	require.Error(t, ValidateAccountEnvironmentCommand(
		nest+" env CODEX_HOME=/other codex", scopedProcessTabAccount()),
		"nesting past the evaluation budget must fail closed around a mutating child")

	// The reported crash shape — ~1M levels inside one body-size request —
	// must come back as a refusal rather than a fatal stack overflow.
	deep := strings.TrimSpace(strings.Repeat("strace -Z ", 1_000_000))
	require.Error(t, ValidateAccountEnvironmentCommand(
		deep+" npm run dev", scopedProcessTabAccount()),
		"a ~1M-level wrapper chain must refuse rather than crash the process")
}

// env nesting is the same attack through a different descent: an unrecognized
// wrapper's tail scan reaches envCallMutatesAccountEnvironment once per env
// word it finds and the env arm reaches it again after, so a chain of env
// invocations fans out instead of recursing in a straight line. Charging the
// argv length against the same budget refuses both the deep chain and the
// fan-out before either exhausts the process.
func TestValidateAccountEnvironmentCommand_EnvNestingBudgetFailsClosed(t *testing.T) {
	for _, levels := range []int{200, 5000} {
		require.Error(t, ValidateAccountEnvironmentCommand(
			strings.Repeat("env ", levels)+"npm run dev", scopedProcessTabAccount()),
			"%d nested env invocations must fail closed, not fan out", levels)
	}
	// A wrapper tail packed with env words spends the same budget: every
	// position the scan delegates is a recursive descent.
	packed := "perf" + strings.Repeat(" env", 5000) + " true"
	require.Error(t, ValidateAccountEnvironmentCommand(packed, scopedProcessTabAccount()),
		"a tail of env words must fail closed rather than evaluate each one recursively")
	// Ordinary env usage stays untouched far below the budget.
	require.NoError(t, ValidateAccountEnvironmentCommand(
		"env PORT=8080 npm run dev", scopedProcessTabAccount()),
		"a plain env invocation must stay allowed")
}
