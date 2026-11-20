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
// rows whose text literally names a protected variable, the account's
// agent, or a hidden-input/env option spelling behind an escape or a quote
// (`env 'CODEX\_HOME=/o' prog`, `bwrap "--clea'renv" codex`,
// `echo strace \-ECODEX_HOME=/o codex`) — the over-refusal this fix
// states. Those are listed in corpusAllowedDiffs, and each may differ only
// by the env and/or envcl column flipping admit→REFUSE.
const corpusCommonMasterTSV = "testdata/corpus_common_master.tsv"

// corpusAccounts matches the review harness's two validation accounts.
var (
	corpusCodexAccount  = Account{Agent: "codex", Name: "work", Dir: "/acct/work"}
	corpusClaudeAccount = Account{Agent: "claude", Name: "work", Dir: "/acct/work"}
)

// corpusAllowedDiffs lists the rows permitted to differ from master, each
// with the reason — the stated over-refusal of this PR: the command's text
// literally names a protected variable, the account's agent, or a hidden
// option spelling behind a backslash escape or a quote, which the
// exact-OR-normalized mark now refuses (#7627). Every entry must differ
// ONLY by the env and/or envcl column admit→REFUSE — a codex-only name
// flips env alone, a name both accounts deny flips both.
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

	// The independent review's R1: an escaped dash hides the strace
	// option itself — `\-…` read as the traced command on master's
	// raw-byte option walk.
	"echo strace \\-ECODEX_HOME=/other codex":     "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"echo strace \\-E CODEX_HOME=/other codex":    "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"echo strace \\--env CODEX_HOME=/other codex": "escaped dash spells strace's --env option; master refuses the unquoted twin",
	"echo strace \\-fECODEX_HOME=/o codex":        "escaped dash spells strace's -E cluster; master refuses the unquoted twin",
	"echo strace -f \\-ECODEX_HOME=/o codex":      "escaped dash spells strace's -E cluster; master refuses the unquoted twin",
	"echo strace '-f' \\-ECODEX_HOME=/o codex":    "escaped dash spells strace's -E cluster; master refuses the unquoted twin",
	"firejail strace \\-ECODEX_HOME=/o codex":     "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"prog strace \\-ECODEX_HOME=/o codex":         "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"make strace \\-ECODEX_HOME=/o codex":         "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"ssh host strace \\-ECODEX_HOME=/o codex":     "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"watch strace \\-ECODEX_HOME=/o codex":        "escaped dash spells strace's -E option; master refuses the unquoted twin",
	"echo strace \\'-E CODEX_HOME=/o codex":       "literal quote byte hides strace's -E option; master refuses the unquoted twin",
	"echo strace \\'--env CODEX_HOME=/o codex":    "literal quote byte hides strace's --env option; master refuses the unquoted twin",

	// The review's R2: the shared name predicate's union also reaches the
	// any-command --opt=VALUE rule, the builtin writers (export, unset,
	// read, printf -v) and xargs — each row's unquoted twin already
	// refuses on master, so the reach is stated rather than narrowed.
	"git log --grep='\"ENV\"'":                      "master refuses the unquoted twin",
	"npm run build --env='\"ENV\"'":                 "master refuses the unquoted twin",
	"git --preserve-env=\\\\CODEX_HOME - nice":      "master refuses the unquoted twin",
	"export 'CODEX\\_HOME=/o'":                      "master refuses the unquoted twin",
	"unset 'CODEX\\_HOME'":                          "master refuses the unquoted twin",
	"read 'CODEX\\_HOME'":                           "master refuses the unquoted twin",
	"printf -v 'CODEX\\_HOME' x":                    "master refuses the unquoted twin",
	"xargs --process-slot-var='CODEX\\_HOME' codex": "master refuses the unquoted twin",
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
// the env and/or envcl columns flipping admit→REFUSE — the single
// over-refusal direction corpusAllowedDiffs permits. A name both accounts
// deny (a shell-startup name like ENV) flips the two columns together; a
// codex-only name flips env alone.
func corpusDiffersOnlyEnvRefuse(want, got string) bool {
	wc, gc := strings.Split(want, "\t"), strings.Split(got, "\t")
	if len(wc) != len(gc) || len(wc) != 5 {
		return false
	}
	flipped := false
	for i := 0; i < 2; i++ {
		switch {
		case wc[i] == gc[i]:
		case wc[i] == "admit" && gc[i] == "REFUSE":
			flipped = true
		default:
			return false
		}
	}
	for i := 2; i < len(wc); i++ {
		if wc[i] != gc[i] {
			return false
		}
	}
	return flipped
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
