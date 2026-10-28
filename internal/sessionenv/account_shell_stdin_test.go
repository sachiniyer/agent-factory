package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A proven startup-free shell reads code from its standard input, so a command
// that feeds that input from anywhere but the pane's terminal runs text the
// command chose — the same unproven code `sh ./script` is refused for (Codex on
// #4474). These forms were admitted for every trusted shell, not only zsh.
func TestApplyAccountEnvironment_RefusesInputFedInteractiveShell(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"/bin/bash --noprofile --norc -i < ./repo-script",
		"/bin/bash --noprofile --norc -i 0< ./repo-script",
		"/bin/sh -i <<'EOF'\nexport CODEX_HOME=/other\ncodex\nEOF",
		"/bin/sh -i <<-EOF\n\texport CODEX_HOME=/other\nEOF",
		"cat ./repo-script | /bin/bash --noprofile --norc -i",
		"cat ./repo-script | nohup /bin/sh -i",
		"exec < ./repo-script; /bin/sh -i",
		"{ /bin/csh -f -i; } < ./repo-script",
		"exec 3< ./repo-script; /bin/sh -i 0>&3",
		"exec 3< ./repo-script; /bin/sh -i 00>&3",
		"/bin/sh -i <> ./repo-script",
		"/bin/sh -i <&3",
		"nohup /bin/sh -i < ./repo-script",
		"while read -r line; do /usr/bin/dash -i; done < ./repo-script",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "%q hands an interactive shell code the terminal did not type", command)
		require.Contains(t, err.Error(), "input other than the terminal", command)
	}
}

// The bash-only feeds are refused too. The walk already refuses these because
// its POSIX parse fails; this pins the scan's own answer, so dropping that
// parse-failure refusal cannot quietly admit them.
func TestCommandFeedsProvenShell_BashOnlyFeeds(t *testing.T) {
	for _, command := range []string{
		"/bin/bash --noprofile --norc -i <<< 'export CODEX_HOME=/other'",
		"cat ./repo-script |& /bin/bash --noprofile --norc -i",
		"coproc /bin/bash --noprofile --norc -i",
		"echo 'export CODEX_HOME=/other' > >(/bin/bash --noprofile --norc -i)",
		"/bin/bash --noprofile --norc -i < <(cat ./repo-script)",
	} {
		require.True(t, commandFeedsProvenShell(command), command)
	}
}

// What stays admitted (#4474 review). The refusal needs BOTH a proven shell and
// a construct that can reach descriptor 0; either one alone is ordinary.
func TestApplyAccountEnvironment_AdmitsUnfedShellsAndShelllessRedirection(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		// The shells themselves, and output-only redirection around them.
		"/bin/bash --noprofile --norc -i",
		"/bin/sh -i",
		"/bin/zsh -f -i",
		"/bin/bash --noprofile --norc -i 2>/dev/null",
		"/bin/bash --noprofile --norc -i >&2",
		"make >build.log 2>&1; /bin/bash --noprofile --norc -i",
		"exec 3>/tmp/trace.log; /bin/sh -i",
		// Input redirection, pipes and here-documents with no proven shell.
		"sort < data.txt",
		"git log --oneline | head -5",
		"ps -ef | grep -c zsh",
		"cat <<'EOF'\nhello\nEOF",
		"npm run dev < /dev/null",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.NoError(t, err, command)
	}
}
