package sessionenv

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// corpusCommonMasterTSV is the common-command acceptance corpus the
// independent review of this head recorded: everyday developer commands
// plus the first review's and the narrow re-review's R1–R7 rows —
// exact-spelling protected names, carried shell names, operand `$`
// text, newline boundaries, versioned shell spellings and brace-word
// budget rows — each with the verdict master 6153cdfc2 returned for
// it. The verdict columns are the five checks the review harness ran:
// env (ValidateAccountEnvironmentCommand on the codex account), envcl
// (the same on a claude account), acct (ValidateAccountCommand), cred
// (credentialAgentForCommand) and grant (GrantAgentForRemoteCommand).
//
// This is a parity oracle, not a wish list: every row must read on this
// code exactly as it read on master. The only differences this PR
// permits are rows whose tail literally names the account's agent or a
// protected variable, or that use a named launcher's hidden-input
// option — the over-refusal this PR states.
const corpusCommonMasterTSV = "testdata/corpus_common_master.tsv"

// corpusAccounts matches the review harness's two validation accounts.
var (
	corpusCodexAccount  = Account{Agent: "codex", Name: "work", Dir: "/acct/work"}
	corpusClaudeAccount = Account{Agent: "claude", Name: "work", Dir: "/acct/work"}
)

// corpusAllowedDiffs lists the rows permitted to differ from master,
// each with the reason — the stated over-refusal of this PR: a launcher
// tail that literally names the agent or a protected variable, or a
// named hidden-input option. Empty today: no corpus row carries one.
var corpusAllowedDiffs = map[string]string{}

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
		// The review harness encodes a literal newline as <LF> so a
		// command stays one TSV row.
		rows = append(rows, row{n: len(rows) + 1,
			cmd:    strings.ReplaceAll(parts[0], "<LF>", "\n"),
			expect: parts[1]})
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
