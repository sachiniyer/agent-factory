package sessionenv

import (
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

func TestValidateAccountEnvironmentCommand_StraceNoChildHazards(t *testing.T) {
	for _, command := range []string{
		"strace -E CODEX_HOME=/other -p 123",
		"strace --env=CODEX_HOME=/other --attach=123",
		`strace -ECODEX_HOME="$OTHER_HOME" -p 123`,
	} {
		t.Run("environment/"+command, func(t *testing.T) {
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"environment option in attach-only command %q has no child to modify", command)
		})
	}

	for _, command := range []string{
		"strace -E CODEX_HOME=/other env PORT=3000 npm run dev",
		"strace -o '|env CODEX_HOME=/other codex' -p 123",
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
		`strace --output="$OUTPUT" codex`,
		`strace -o"$OUTPUT" codex`,
	} {
		t.Run("unsafe/"+command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"dynamic security-sensitive prefix in %q must fail closed", command)
		})
	}
}
