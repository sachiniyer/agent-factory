package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tilde path names its executable by its literal final component, so every
// modeled wrapper keeps its verdict however the path reaches it. At 8f4c2859b
// the name match refused every leading-~ word, so `~/../../usr/bin/env
// CODEX_HOME=/other codex` was admitted while running /usr/bin/env (Codex on
// #4466); strace, xargs, and the shell wrappers were hidden the same way. The
// rows are refused under their bare names — this asserts the spelling of the
// head cannot change that, in any position a wrapper's child can take.
func TestCommandMutatesAccountEnvironment_TildePathHeadsKeepTheirWrapperVerdict(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	rows := []struct{ wrapper, rest string }{
		{"env", "CODEX_HOME=/other codex"},
		{"env", "-i codex"},
		{"env", "-u CODEX_HOME codex"},
		{"strace", "-E CODEX_HOME codex"},
		{"strace", "-E CODEX_HOME=/other codex"},
		{"xargs", "--process-slot-var=CODEX_HOME codex"},
		{"xargs", "--process-slot-var CODEX_HOME codex"},
		{"nohup", "sh -c 'unset CODEX_HOME; codex'"},
		{"nice", "sh -c 'unset CODEX_HOME; codex'"},
		{"timeout", "10 sh -c 'unset CODEX_HOME; codex'"},
		{"setsid", "sh -c 'unset CODEX_HOME; codex'"},
	}
	spellings := []string{
		"%s",
		"/usr/bin/%s",
		"~/../../usr/bin/%s",
		"~root/../usr/bin/%s",
		"~+/../../usr/bin/%s",
		"~-/../../usr/bin/%s",
		"~/bin/%s",
	}
	contexts := []string{"", "nohup ", "exec ", "nice -n 5 ", "true; "}
	for _, row := range rows {
		for _, spelling := range spellings {
			head := strings.ReplaceAll(spelling, "%s", row.wrapper)
			for _, context := range contexts {
				command := context + head + " " + row.rest
				assert.True(t, commandMutatesAccountEnvironment(command, codex), "command %q", command)
			}
		}
	}
}

// Everything the walk now reads through a tilde path is sound only while the
// command leaves HOME, PWD, and OLDPWD alone: once it rebinds one, the word can
// expand to an option or an assignment. Measured under dash and bash,
// `HOME=CODEX_HOME=; env ~/other codex` runs `env CODEX_HOME=/other codex`.
func TestValidateAccountEnvironmentCommand_RefusesTildeAfterRebinding(t *testing.T) {
	for _, command := range []string{
		"HOME=CODEX_HOME=; env ~/other codex",
		"HOME=-ca; exec ~/x codex",
		"HOME='|unset CODEX_HOME'; strace -o ~/x codex",
		"export HOME=/tmp/h; ~/bin/tool",
		"read -r HOME < ./cfg; nohup ~/bin/tool",
		"printf -v HOME %s -ca; exec ~/x codex",
		": \"${HOME:=/tmp/h}\"; ~/bin/tool",
		"unset HOME; ~/bin/tool",
		"for PWD in /tmp; do ~+/bin/tool; done",
		"OLDPWD=/tmp; ~-/bin/tool",
		"f() { HOME=/tmp/h; }; f; ~/bin/tool",
		"~/bin/tool; HOME=/tmp/h",
		// A same-call prefix does not reach the expansion (the shell expands
		// words first), but the walk judges rebinding by name, not by order.
		"HOME=/tmp/h ~/bin/tool",
		"env HOME=/tmp/h ~/bin/tool",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		if !assert.Error(t, err, "command %q", command) {
			continue
		}
		assert.Contains(t, err.Error(), "write the path out in full", "command %q", command)
	}
	// An identity mutation keeps its own message even when a tilde is present.
	err := ValidateAccountEnvironmentCommand("unset CODEX_HOME; ~/bin/tool", scopedProcessTabAccount())
	require.Error(t, err)
	require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
}

// The directory-stack forms follow pushd, so even with a slash they are no
// fixed directory.
func TestCommandMutatesAccountEnvironment_RefusesDirectoryStackTildeHeads(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for _, command := range []string{
		"~1/bin/tool",
		"~+1/bin/tool",
		"~-0/bin/tool",
		"nohup ~2/bin/tool",
		`~us\er/bin/tool`,
	} {
		assert.True(t, commandMutatesAccountEnvironment(command, codex), "command %q", command)
	}
}

// False-positive canaries (#4466 review). 8f4c2859b refused a tilde path in
// every wrapper's child position — all of these but the rebinding-free `HOME`
// and `cd` lines were admitted on master and refused there. None of them can
// replace the account root.
func TestValidateAccountEnvironmentCommand_AdmitsTildePaths(t *testing.T) {
	for _, command := range []string{
		"nohup ~/bin/server --port 3000",
		"exec ~/bin/server",
		"nice -n 5 ~/bin/build --jobs 4",
		"timeout 30 ~/bin/check",
		"setsid ~/bin/daemon",
		"stdbuf -oL ~/bin/tail-logs",
		"xargs -n1 ~/bin/process",
		"xargs -a ~/items.txt ~/bin/process",
		"strace -f ~/bin/app",
		"strace -o ~/trace.log ~/bin/app",
		"command ~/bin/tool",
		"~/bin/tool --flag",
		"~-/bin/tool",
		"~root/bin/tool",
		"~www-data/bin/tool",
		"~first.last/bin/tool",
		"~/bin/env-check --all",
		"~/.cargo/bin/rg CODEX_HOME=/tmp",
		"cp ~/a.txt ~/b.txt",
		"ls ~",
		"cd ~/src/app && ~/bin/dev",
		"cd /srv && ~+/bin/dev",
		// Rebinding with no tilde to read it is not the boundary's concern.
		"export HOME=/tmp/h; make",
		"PWD=/srv make",
	} {
		assert.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()), "command %q", command)
	}
}
