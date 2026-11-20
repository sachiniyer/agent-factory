package sessionenv

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// corpusCommonMasterTSV is the common-command acceptance corpus: everyday
// developer commands plus the review probes #7617 accumulated — exact-spelling
// protected names, carried shell names, operand `$` text, newline boundaries,
// versioned shell spellings, brace-word budget rows and the final review's
// R1–R4 everyday commands and R5 escaped-name rows — each with the verdict
// master returned for it (re-verified against this branch's merge base). The
// verdict columns are the five checks the review harness ran: env
// (ValidateAccountEnvironmentCommand on the codex account), envcl (the same
// on a claude account), acct (ValidateAccountCommand), cred
// (credentialAgentForCommand) and grant (GrantAgentForRemoteCommand).
//
// This is a parity oracle, not a wish list: every row must read on this code
// exactly as it read on master. The only differences this PR permits are
// rows whose text literally names a protected variable or the account's
// agent behind an escape or a quote (`env 'CODEX\_HOME=/o' prog`,
// `bwrap "--clea'renv" codex`) — the over-refusal this fix states. Those are
// listed in corpusAllowedDiffs, and each may differ only by the env column
// flipping admit→REFUSE.
const corpusCommonMasterTSV = "testdata/corpus_common_master.tsv"

// corpusAccounts matches the review harness's two validation accounts.
var (
	corpusCodexAccount  = Account{Agent: "codex", Name: "work", Dir: "/acct/work"}
	corpusClaudeAccount = Account{Agent: "claude", Name: "work", Dir: "/acct/work"}
)

// corpusAllowedDiffs lists the rows permitted to differ from master, each
// with the reason — the stated over-refusal of this PR: the command's text
// literally names a protected variable or the account's agent behind a
// backslash escape or a quote, which the exact-OR-normalized mark now
// refuses (#7627). Every entry must differ ONLY by the env column
// admit→REFUSE.
var corpusAllowedDiffs = map[string]string{
	"env 'CODEX\\_HOME=/o' prog":                  "escaped protected name in env's assignment operand",
	"env -u 'CODEX\\_HOME' prog":                  "escaped protected name in env's -u operand",
	"nohup env 'CODEX\\_HOME=/o' prog":            "escaped protected name under nohup's env tail",
	"doas env 'CODEX\\_HOME=/o' prog":             "escaped protected name under doas's env tail",
	"sudo 'CODEX\\_HOME=/o' prog":                 "escaped protected name in sudo's lexical cut",
	"sudo -E 'CODEX\\_HOME=/o' prog":              "escaped protected name under sudo -E",
	"docker run -e 'CODEX\\_HOME=/o' img prog":    "escaped protected name in docker's -e operand",
	"docker run --env 'CODEX\\_HOME=/o' img prog": "escaped protected name in docker's --env operand",
	"bwrap --setenv 'CODEX\\_HOME' /o prog":       "escaped protected name in bwrap's --setenv operand",
	"bwrap --unsetenv 'CODEX\\_HOME' prog":        "escaped protected name in bwrap's --unsetenv operand",
	`bwrap "--clea'renv" codex`:                   "bwrap --clearenv spelled behind a quote, reset arm reaches codex",
	`sudo "co'dex'"`:                              "agent name spelled behind a quote, sudo reset arm",
	"sudo 'co\\dex'":                              "agent name spelled behind an escape, sudo reset arm",
	"env 'CO\\DEX_HOME=/o' prog":                  "escaped protected name mid-name in env's operand",
	"env 'CODEX_HOM\\E=/o' prog":                  "escaped last letter of the protected name",
	"env 'CODEX_HOME\\==/o' prog":                 "escape before the assignment joiner",
	"env 'C\\ODEX_HOME=/o' prog":                  "escape inside the protected name's first half",
}

// corpusVerdict folds an error verdict to the admit/REFUSE strings the
// recorded table uses.
func corpusVerdict(err error) string {
	if err != nil {
		return "REFUSE"
	}
	return "admit"
}

// corpusRowVerdicts computes the five verdict columns the corpus records,
// tab-separated, for one command.
func corpusRowVerdicts(cmd string) string {
	return strings.Join([]string{
		corpusVerdict(ValidateAccountEnvironmentCommand(cmd, corpusCodexAccount)),
		corpusVerdict(ValidateAccountEnvironmentCommand(cmd, corpusClaudeAccount)),
		corpusVerdict(ValidateAccountCommand(cmd, corpusCodexAccount)),
		"\"" + credentialAgentForCommand(cmd) + "\"",
		"\"" + GrantAgentForRemoteCommand(cmd, "/w") + "\"",
	}, "\t")
}

// corpusDiffersOnlyEnvRefuse reports whether got differs from want ONLY by
// the env column flipping admit→REFUSE — the single over-refusal direction
// corpusAllowedDiffs permits.
func corpusDiffersOnlyEnvRefuse(want, got string) bool {
	wc, gc := strings.Split(want, "\t"), strings.Split(got, "\t")
	if len(wc) != len(gc) || len(wc) != 5 {
		return false
	}
	if wc[0] != "admit" || gc[0] != "REFUSE" {
		return false
	}
	for i := 1; i < len(wc); i++ {
		if wc[i] != gc[i] {
			return false
		}
	}
	return true
}

// TestAccountEnvironmentCorpusParity re-runs every corpus row and
// requires the same five verdicts master produced. A row that differs
// in ANY column is reported with its expected and actual line — unless it
// is a corpusAllowedDiffs row, which must differ only by the env column
// flipping admit→REFUSE. The test fails while any row differs otherwise,
// or while a listed row's diff is anything but that one direction.
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
		got := corpusRowVerdicts(r.cmd)
		if got == r.expect {
			continue
		}
		if _, allowed := corpusAllowedDiffs[r.cmd]; allowed &&
			corpusDiffersOnlyEnvRefuse(r.expect, got) {
			continue
		}
		mismatches++
		if mismatches <= 25 {
			t.Errorf("corpus row %d differs from master\n  cmd:    %s\n  master: %s\n  head:   %s",
				r.n, r.cmd, r.expect, got)
		}
	}
	if mismatches > 0 {
		t.Errorf("%d corpus rows differ from master", mismatches)
	}
}
