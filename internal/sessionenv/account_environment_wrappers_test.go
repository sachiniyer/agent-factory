package sessionenv

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAccountEnvironmentCommand_RefusesDirectWrapperChildMutation(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"ltrace", "ltrace env CODEX_HOME=/other codex"},
		{"valgrind", "valgrind env CODEX_HOME=/other codex"},
		{"chrt", "chrt -o 0 env CODEX_HOME=/other codex"},
		{"unshare", "unshare env CODEX_HOME=/other codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"%s must expose its child command to account-environment validation", test.name)
		})
	}
}

func TestValidateAccountEnvironmentCommand_RefusesNestedAndAbsoluteWrapperChildMutation(t *testing.T) {
	for _, command := range []string{
		"/usr/bin/ltrace env CODEX_HOME=/other codex",
		"nohup ltrace env CODEX_HOME=/other codex",
		"valgrind nice env CODEX_HOME=/other codex",
		"setsid chrt -o env CODEX_HOME=/other codex",
		"unshare timeout 10 env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"every wrapper layer must expose its child in %q", command)
	}
}

func TestValidateAccountEnvironmentCommand_NewWrapperUnknownSyntaxFailsClosed(t *testing.T) {
	for _, command := range []string{
		"ltrace --future-option npm run dev",
		"valgrind --future-option npm run dev",
		"chrt --future-option npm run dev",
		"unshare --future-option npm run dev",
		`ltrace "$AF_LTRACE_OPTION" npm run dev`,
		`valgrind "$AF_VALGRIND_OPTION" npm run dev`,
		`chrt "$AF_CHRT_OPTION" npm run dev`,
		`unshare "$AF_UNSHARE_OPTION" npm run dev`,
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"unreduced wrapper input %q must not default to safe", command)
	}
}

func TestValidateAccountEnvironmentCommand_NewWrapperNoValueOptionsDoNotConsumeChild(t *testing.T) {
	for _, test := range []struct {
		wrapper       string
		options       string
		beforeCommand string
	}{
		{"ltrace", "-b -c -C -f -i -L -r -S -t -T --no-signals --demangle", ""},
		{"valgrind", "-q -v -d -s --quiet --verbose", ""},
		{"chrt", "-b -d -e -f -i -o -r -R -a -v -G -O --batch --deadline --ext --fifo --idle --other --rr --reclaim-grub --deadline-overrun --reset-on-fork --all-tasks --verbose", "10"},
		{"unshare", "-f -r -c -m -u -i -n -p -U -C -T --fork --forward-signals --map-root-user --map-current-user --map-auto --map-subids --keep-caps --mount --uts --ipc --net --pid --user --cgroup --time --kill-child --mount-proc --mount-binfmt", ""},
	} {
		for _, option := range strings.Fields(test.options) {
			t.Run(test.wrapper+" "+option, func(t *testing.T) {
				command := strings.TrimSpace(fmt.Sprintf("%s %s %s env CODEX_HOME=/other codex",
					test.wrapper, option, test.beforeCommand))
				require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
					"argument-free option %s must not swallow the child executable", option)
			})
		}
	}
}

func TestValidateAccountEnvironmentCommand_NewWrapperValueOptionsConsumeOperands(t *testing.T) {
	for _, test := range []struct {
		wrapper       string
		option        string
		value         string
		beforeCommand string
	}{
		{"ltrace", "-a", "40", ""},
		{"ltrace", "-A", "8", ""},
		{"ltrace", "-d", "3", ""},
		{"ltrace", "-D", "77", ""},
		{"ltrace", "-e", "malloc", ""},
		{"ltrace", "-F", "/tmp/ltrace.conf", ""},
		{"ltrace", "-l", "libc.so", ""},
		{"ltrace", "-n", "2", ""},
		{"ltrace", "-o", "/tmp/ltrace.out", ""},
		{"ltrace", "-p", "1234", ""},
		{"ltrace", "-s", "256", ""},
		{"ltrace", "-u", "nobody", ""},
		{"ltrace", "-w", "4", ""},
		{"ltrace", "-x", "malloc", ""},
		{"ltrace", "--align", "40", ""},
		{"ltrace", "--debug", "77", ""},
		{"ltrace", "--config", "/tmp/ltrace.conf", ""},
		{"ltrace", "--library", "libc.so", ""},
		{"ltrace", "--indent", "2", ""},
		{"ltrace", "--max-depth", "3", ""},
		{"ltrace", "--output", "/tmp/ltrace.out", ""},
		{"ltrace", "--where", "4", ""},
		{"chrt", "-T", "100000", "10"},
		{"chrt", "-P", "100000", "10"},
		{"chrt", "-D", "100000", "10"},
		{"chrt", "-U", "100", "10"},
		{"chrt", "-X", "900", "10"},
		{"chrt", "--sched-runtime", "100000", "10"},
		{"chrt", "--sched-period", "100000", "10"},
		{"chrt", "--sched-deadline", "100000", "10"},
		{"chrt", "--clamp-min", "100", "10"},
		{"chrt", "--clamp-max", "900", "10"},
		{"unshare", "-R", "/tmp", ""},
		{"unshare", "-w", "/tmp", ""},
		{"unshare", "-S", "1000", ""},
		{"unshare", "-G", "1000", ""},
		{"unshare", "--map-user", "1000", ""},
		{"unshare", "--map-group", "1000", ""},
		{"unshare", "--map-users", "0:1000:1", ""},
		{"unshare", "--map-groups", "0:1000:1", ""},
		{"unshare", "--owner", "1000:1000", ""},
		{"unshare", "--propagation", "private", ""},
		{"unshare", "--setgroups", "deny", ""},
		{"unshare", "--root", "/tmp", ""},
		{"unshare", "--wd", "/tmp", ""},
		{"unshare", "--setuid", "1000", ""},
		{"unshare", "--setgid", "1000", ""},
		{"unshare", "--monotonic", "0", ""},
		{"unshare", "--boottime", "0", ""},
	} {
		t.Run(test.wrapper+" "+test.option, func(t *testing.T) {
			command := strings.TrimSpace(fmt.Sprintf("%s %s %s %s npm run dev",
				test.wrapper, test.option, test.value, test.beforeCommand))
			require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"value-taking option %s must consume its own operand", test.option)
		})
	}

	for _, command := range []string{
		"valgrind --tool=memcheck npm run dev",
		"valgrind --leak-check=full npm run dev",
		"valgrind --log-file=/tmp/valgrind.out npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"valgrind's value options are complete --name=value words")
	}
}

func TestValidateAccountEnvironmentCommand_ChrtOptionalPriorityDoesNotHideChild(t *testing.T) {
	for _, command := range []string{
		"chrt -o env CODEX_HOME=/other codex",
		"chrt --other env CODEX_HOME=/other codex",
		"chrt -o 0 env CODEX_HOME=/other codex",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"chrt's optional priority must not be confused with its child in %q", command)
	}
}

func TestValidateAccountEnvironmentCommand_RefusesUnshareEnvironmentAndInterpreterOptions(t *testing.T) {
	for _, command := range []string{
		"unshare --clear-env npm run dev",
		"unshare --whitelist-env PATH npm run dev",
		"unshare --whitelist-env=PATH npm run dev",
		"unshare -l ':af:M::magic::/tmp/interpreter:' /bin/true",
		"unshare --load-interp=':af:M::magic::/tmp/interpreter:' /bin/true",
		"unshare",
	} {
		require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"unshare command %q cannot prove that the selected account reaches its child", command)
	}
}

func TestValidateAccountEnvironmentCommand_WrapperOutputDataDoesNotBecomeACommand(t *testing.T) {
	for _, command := range []string{
		`ltrace -o '|env CODEX_HOME=/other codex' true`,
		`ltrace --output='!env CODEX_HOME=/other codex' true`,
		`valgrind --log-file='|env CODEX_HOME=/other codex' true`,
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"unlike strace, %q documents its output operand as data, not an executable command", command)
	}
}

func TestAccountCommandWrapperSpecsDoNotOverlapOptionKinds(t *testing.T) {
	for name, spec := range accountCommandWrapperSpecs {
		short := make(map[rune]string)
		for kind, options := range map[string]string{
			"no value": spec.shortNoValue, "value": spec.shortValue,
			"optional value": spec.shortOptionalValue, "terminal": spec.shortTerminal,
			"environment": spec.shortEnvironmentValue, "executable output": spec.shortExecutableOutput,
			"unsafe": spec.shortUnsafe,
		} {
			for _, option := range options {
				previous, exists := short[option]
				require.False(t, exists, "%s -%c appears in both %s and %s", name, option, previous, kind)
				short[option] = kind
			}
		}

		long := make(map[string]string)
		for kind, options := range map[string][]string{
			"no value": spec.longNoValue, "value": spec.longValue,
			"optional value": spec.longOptionalValue, "terminal": spec.longTerminal,
			"environment": spec.longEnvironmentValue, "executable output": spec.longExecutableOutput,
			"unsafe": spec.longUnsafe,
		} {
			for _, option := range options {
				previous, exists := long[option]
				require.False(t, exists, "%s %s appears in both %s and %s", name, option, previous, kind)
				long[option] = kind
			}
		}
	}
}

func TestAccountCommandWrapperSetIsExplicit(t *testing.T) {
	names := make([]string, 0, len(accountCommandWrapperSpecs))
	for name := range accountCommandWrapperSpecs {
		names = append(names, name)
	}
	slices.Sort(names)
	require.Equal(t, []string{
		"chrt", "ionice", "ltrace", "nice", "nohup", "setsid", "stdbuf",
		"strace", "taskset", "timeout", "unshare", "valgrind",
	}, names)
	for _, excluded := range []string{"doas", "gdb", "perf", "sudo", "xargs"} {
		_, exists := accountCommandWrapperSpecs[excluded]
		require.False(t, exists, "%s needs a dedicated analyzer, not the single-child wrapper parser", excluded)
	}
}

func TestValidateAccountEnvironmentCommand_WrapperSetStaysNarrow(t *testing.T) {
	for _, command := range []string{
		"echo CODEX_HOME=/tmp",
		"rg 'OPENAI_API_KEY=x'",
		"set +e - -k; npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"unclassified leaf command %q must remain accepted", command)
	}
}
