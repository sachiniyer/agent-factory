package quotaobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/quota"
)

// These tests drive Collect against a fake AF home: fake accounts under
// <home>/accounts/codex/, a fake state file under <home>/instances/, and a
// handwritten config.toml. All fixtures are sanitized — invented names and
// paths only, no real account data anywhere.

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	return home
}

func writeGlobalConfig(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeAccountRollout(t *testing.T, home, account, dayDir, line string) {
	t.Helper()
	dir := filepath.Join(home, "accounts", "codex", account, "sessions", "2026", "10", dayDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}
	name := "rollout-2026-10-31T04-00-00-00000000-0000-4000-8000-000000000001.jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
}

func writeStateFile(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, "instances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir instances: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auto-handoff-state.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
}

// claimLine is a sanitized rate_limits record — the shape
// quota.ParseCodexRateLimitsLine reads, with invented values only.
const claimLine = `{"timestamp":"2026-10-31T04:52:13.492Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":98.5,"window_minutes":300,"resets_at":1761902400},"secondary":{"used_percent":41.2,"window_minutes":10080,"resets_at":1762387200}}}}`

// The off-by-default invariant (#6776): with no [account_handoff] table and no
// state file, Collect must produce no account section at all — the same
// Result shape the feature would have produced before it existed.
func TestCollect_AbsentTableAndNoStateFileRendersNothing(t *testing.T) {
	fakeHome(t)
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Accounts != nil {
		t.Fatalf("Accounts = %+v, want nil under absent table", result.Accounts)
	}
	if result.Mover != nil {
		t.Fatalf("Mover = %+v, want nil under absent table", result.Mover)
	}
	if warnings := result.Warnings(); len(warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", warnings)
	}
}

// A registered account with NO opted-in machinery still produces nothing: the
// gate is engagement, not the mere existence of account directories.
func TestCollect_AccountsExistButNoOptIn(t *testing.T) {
	home := fakeHome(t)
	writeAccountRollout(t, home, "work", "31", claimLine)
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Accounts != nil || result.Mover != nil {
		t.Fatalf("section materialized without opt-in: accounts=%+v mover=%+v", result.Accounts, result.Mover)
	}
}

// A table written out explicitly with mode = "off" is exactly the
// absent-table case — "off" means off, however it got there. (The materialized
// default config.toml also writes this shape, which is why the gate keys on
// the resolved mode and not the table's presence.)
func TestCollect_ExplicitOffTableRendersNothing(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "[account_handoff]\nmode = \"off\"\n")
	writeAccountRollout(t, home, "work", "31", claimLine)
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Accounts != nil || result.Mover != nil {
		t.Fatalf("section materialized under mode=off: accounts=%+v mover=%+v", result.Accounts, result.Mover)
	}
}

func TestCollect_TablePresentReadsAccountWindows(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
	writeAccountRollout(t, home, "work", "31", claimLine)

	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Accounts) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(result.Accounts))
	}
	acct := result.Accounts[0]
	if acct.Agent != "codex" || acct.Name != "work" {
		t.Fatalf("account = %s/%s", acct.Agent, acct.Name)
	}
	if acct.Observation != quota.AccountObservationObserved {
		t.Fatalf("Observation = %v, want observed", acct.Observation)
	}
	if len(acct.Reading.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", acct.Reading.Windows)
	}
	if result.Mover == nil || result.Mover.Mode != "shadow" || result.Mover.Halted {
		t.Fatalf("Mover = %+v, want non-halted shadow", result.Mover)
	}
}

func TestCollect_EnabledWithNoAccountsStillReportsMover(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Accounts) != 0 {
		t.Fatalf("Accounts = %+v, want empty", result.Accounts)
	}
	if result.Mover == nil || result.Mover.String() != "shadow" {
		t.Fatalf("Mover = %+v, want mover: shadow", result.Mover)
	}
}

// R16: a halt recorded durably must surface even with no [account_handoff]
// table — the state file alone engages the section, because a halted mover
// must never be silent.
func TestCollect_StateFileAloneEngagesSection(t *testing.T) {
	home := fakeHome(t)
	writeStateFile(t, home, `{"schema_version":1,"halted":{"at":"2026-10-31T05:00:00Z","reason":"refused every target"}}`)
	writeAccountRollout(t, home, "work", "31", claimLine)

	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Mover == nil || !result.Mover.Halted {
		t.Fatalf("Mover = %+v, want halted", result.Mover)
	}
	if got := result.Mover.String(); got != "halted: refused every target since 2026-10-31T05:00:00Z" {
		t.Fatalf("mover line = %q", got)
	}
	if len(result.Accounts) != 1 {
		t.Fatalf("Accounts = %+v, want the registered account still reported", result.Accounts)
	}
}

func TestCollect_CorruptStateFileReportsUnreadable(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
	writeStateFile(t, home, `{"schema_version":999,"bogus":`)
	writeAccountRollout(t, home, "work", "31", claimLine)

	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Mover == nil || !result.Mover.StateUnreadable {
		t.Fatalf("Mover = %+v, want state-unreadable", result.Mover)
	}
	if !result.HandoffUnreadable {
		t.Fatal("HandoffUnreadable = false, want true")
	}
	var found bool
	for _, warning := range result.Warnings() {
		found = found || strings.Contains(warning, "auto-handoff state file")
	}
	if !found {
		t.Fatalf("warnings missing the state-file caveat: %v", result.Warnings())
	}
}

// Watermarks must reach the indexer: a claim line entirely inside a recorded
// carried prefix holds the SOURCE account's numbers and must be ignored — the
// section then honestly reports no evidence rather than a poisoned reading.
func TestCollect_CarriedPrefixWatermarkIsHonored(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
	writeAccountRollout(t, home, "work", "31", claimLine)

	rollout := filepath.Join(home, "accounts", "codex", "work",
		"sessions", "2026", "10", "31",
		"rollout-2026-10-31T04-00-00-00000000-0000-4000-8000-000000000001.jsonl")
	info, err := os.Stat(rollout)
	if err != nil {
		t.Fatalf("stat rollout: %v", err)
	}
	writeStateFile(t, home, `{"schema_version":1,"carried_prefix_len":{"`+rollout+`":`+
		strconv.FormatInt(info.Size(), 10)+`}}`)

	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Accounts) != 1 || result.Accounts[0].Observation != quota.AccountObservationNone {
		t.Fatalf("Accounts = %+v, want one account with no evidence (whole claim watermarked)", result.Accounts)
	}
}
