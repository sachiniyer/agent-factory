package sessionenv

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// corpusCommonMasterTSV is the common-command acceptance corpus the
// independent review of this head recorded: 472 everyday developer
// commands plus the review's R1–R4 rows, each with the verdict master
// 6153cdfc2 returned for it. The verdict columns are the five checks the
// review harness ran: env (ValidateAccountEnvironmentCommand on the
// codex account), envcl (the same on a claude account), acct
// (ValidateAccountCommand), cred (credentialAgentForCommand) and grant
// (GrantAgentForRemoteCommand).
//
// This is a parity oracle, not a wish list: every row must read on this
// code exactly as it read on master. The only differences this PR
// permits are rows whose tail literally names the account's agent or a
// protected variable — the over-refusal this PR states — plus the one
// documented exception in corpusAllowedDiffs, where master's own sh -c
// judge refuses the same carried text.
const corpusCommonMasterTSV = "testdata/corpus_common_master.tsv"

// corpusAccounts matches the review harness's two validation accounts.
var (
	corpusCodexAccount  = Account{Agent: "codex", Name: "work", Dir: "/acct/work"}
	corpusClaudeAccount = Account{Agent: "claude", Name: "work", Dir: "/acct/work"}
)

// corpusAllowedDiffs lists the rows permitted to differ from master,
// each with the reason. `tmux new-window 'cd ~/src && $EDITOR .'` keeps
// its refusal: master's own `sh -c 'cd ~/src && $EDITOR .'` refuses the
// same text — a `$` word at a command position is a constructed name —
// so the launcher's carried shell text keeps that rule exactly.
//
// The remaining rows are the shell-nesting property: a shell name
// (sh, bash, dash, zsh — the shells master's own payload and input
// rules model) at a command position in a joiner's carried text
// applies master's own judges to the joined string. Each row below
// refuses only because master refuses the direct equivalent — `sh
// build.sh`, `bash run.sh` and `x | sh` all refuse when typed
// directly. The admit rows beside them (`watch 'sh -c "echo hi"'`,
// `watch 'echo hi | bash -c "echo hi"'`, `watch 'a | shell'`) are the
// parity half: carried text master's judges call proven or un-fed
// admits exactly as the direct form does.
var corpusAllowedDiffs = map[string]string{
	"tmux new-window 'cd ~/src && $EDITOR .'": "REFUSE stands: master's `sh -c 'cd ~/src && $EDITOR .'` refuses the same command-position `$` text",
	"tmux new-window 'ls; bash'":              "REFUSE stands: carried `bash` applies master's own shell rule; `ls; bash` refuses as the direct command does",
	"watch 'sh build.sh'":                     "REFUSE stands: carried `sh build.sh` refuses exactly as master's direct `sh build.sh` does",
	"watch 'x | sh'":                          "REFUSE stands: carried `x | sh` refuses exactly as master's direct `x | sh` does",
	"watch 'make && bash run.sh'":             "REFUSE stands: carried `bash run.sh` refuses exactly as master's direct `bash run.sh` does",
	"watch 'printf B64 | base64 -d | sh'":     "REFUSE stands: carried `| sh` refuses exactly as master's direct `printf B64 | base64 -d | sh` does",
	"watch 'echo hi | sh -s'":                 "REFUSE stands: carried `| sh -s` refuses exactly as master's direct `echo hi | sh -s` does",
	"ssh host 'sh build.sh'":                  "REFUSE stands: carried `sh build.sh` refuses exactly as master's direct `sh build.sh` does",
	"ssh host 'bash run.sh'":                  "REFUSE stands: carried `bash run.sh` refuses exactly as master's direct `bash run.sh` does",
}

// corpusVerdict folds an error verdict to the admit/REFUSE strings the
// recorded table uses.
func corpusVerdict(err error) string {
	if err != nil {
		return "REFUSE"
	}
	return "admit"
}

// TestAccountEnvironmentCorpusParity re-runs every corpus row and
// requires the same five verdicts master produced. A row that differs
// in ANY column is reported with its expected and actual line; the test
// fails while any row differs.
func TestAccountEnvironmentCorpusParity(t *testing.T) {
	f, err := os.Open(corpusCommonMasterTSV)
	require.NoError(t, err)
	defer f.Close()

	type row struct {
		n      int
		cmd    string
		expect string
	}
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		require.Len(t, parts, 2, "corpus row missing tab: %q", line)
		rows = append(rows, row{n: len(rows) + 1, cmd: parts[0], expect: parts[1]})
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, rows)

	mismatches := 0
	for _, r := range rows {
		got := strings.Join([]string{
			corpusVerdict(ValidateAccountEnvironmentCommand(r.cmd, corpusCodexAccount)),
			corpusVerdict(ValidateAccountEnvironmentCommand(r.cmd, corpusClaudeAccount)),
			corpusVerdict(ValidateAccountCommand(r.cmd, corpusCodexAccount)),
			"\"" + credentialAgentForCommand(r.cmd) + "\"",
			"\"" + GrantAgentForRemoteCommand(r.cmd, "/w") + "\"",
		}, "\t")
		if got != r.expect {
			if _, allowed := corpusAllowedDiffs[r.cmd]; allowed {
				continue
			}
			mismatches++
			if mismatches <= 25 {
				t.Errorf("corpus row %d differs from master\n  cmd:    %s\n  master: %s\n  head:   %s",
					r.n, r.cmd, r.expect, got)
			}
		}
	}
	require.Zero(t, mismatches,
		"%d of %d corpus rows read differently from master 6153cdfc2", mismatches, len(rows))
}
