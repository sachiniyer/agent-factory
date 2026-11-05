package quotaobs

import (
	"crypto/sha256"
	"fmt"
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

// An EMPTY state file is no more opt-in than an absent one: nothing recorded
// means nothing to report. This is the shape a no-op durable write leaves
// behind (e.g. a startup reconcile on a home that never enabled handoff), and
// it must not enrol the home in the section.
func TestCollect_EmptyStateFileRendersNothing(t *testing.T) {
	home := fakeHome(t)
	writeStateFile(t, home, `{"schema_version":1}`)
	writeAccountRollout(t, home, "work", "31", claimLine)
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Accounts != nil || result.Mover != nil {
		t.Fatalf("section materialized off a record-free state file: accounts=%+v mover=%+v",
			result.Accounts, result.Mover)
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
	// Unknown watermarks mean unknown readings: the claim may live inside a
	// carried prefix holding the SOURCE account's numbers, so the row must
	// not present it as this account's observed evidence.
	if len(result.Accounts) != 1 ||
		result.Accounts[0].Observation != quota.AccountObservationUnknown ||
		result.Accounts[0].Reason != "watermarks unreadable" {
		t.Fatalf("Accounts = %+v, want one unknown row with the watermark reason", result.Accounts)
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

// An unreadable config must still warn when nothing else engages the section:
// a failed load does not establish that handoff is off, so an operator who
// enabled shadow mode and then broke the TOML elsewhere needs the caveat even
// though no section renders.
func TestCollect_UnreadableConfigStillWarns(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "not valid toml [[[\n")
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !result.ConfigUnreadable {
		t.Fatal("ConfigUnreadable = false, want true")
	}
	if result.Accounts != nil || result.Mover != nil {
		t.Fatalf("section rendered off an unreadable config: accounts=%+v mover=%+v", result.Accounts, result.Mover)
	}
	var found bool
	for _, warning := range result.Warnings() {
		found = found || strings.Contains(warning, "could not read its config")
	}
	if !found {
		t.Fatalf("warnings missing the config caveat: %v", result.Warnings())
	}
}

// The same unreadable config with a state file present: the section still
// renders (a recorded halt is never silent), and the mover line must not
// claim a configured mode af never saw.
func TestCollect_UnreadableConfigWithStateFile(t *testing.T) {
	home := fakeHome(t)
	writeGlobalConfig(t, home, "not valid toml [[[\n")
	writeStateFile(t, home, `{"schema_version":1,"halted":{"at":"2026-10-31T05:00:00Z","reason":"x"}}`)
	result, err := Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !result.ConfigUnreadable {
		t.Fatal("ConfigUnreadable = false, want true")
	}
	if result.Mover == nil || !result.Mover.ModeUnknown || !result.Mover.Halted {
		t.Fatalf("Mover = %+v, want mode-unknown + halted", result.Mover)
	}
	if result.Mover.Mode != "" {
		t.Fatalf("Mover.Mode = %q, want empty — af never saw the configured mode", result.Mover.Mode)
	}
}

// `af quota` is a read-only command: Collect must not create or rewrite any
// file under the af home — not a materialized config.toml on a home without
// one, and not a config.json → config.toml conversion on a legacy home.
func TestCollect_ReadOnlyLeavesHomeUntouched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"no config files", func(t *testing.T, home string) {}},
		{"enabled handoff config", func(t *testing.T, home string) {
			writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
		}},
		{"legacy config.json only", func(t *testing.T, home string) {
			if err := os.WriteFile(filepath.Join(home, "config.json"),
				[]byte(`{"default_program":"claude"}`), 0o600); err != nil {
				t.Fatalf("write config.json: %v", err)
			}
		}},
		{"account and state file present", func(t *testing.T, home string) {
			writeGlobalConfig(t, home, "[account_handoff]\nmode = \"shadow\"\n")
			writeAccountRollout(t, home, "work", "31", claimLine)
			writeStateFile(t, home, `{"schema_version":1}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			tc.setup(t, home)
			before := snapshotTree(t, home)
			if _, err := Collect(); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			after := snapshotTree(t, home)
			if len(before) != len(after) {
				t.Fatalf("entry count changed: %d -> %d (new: %v)", len(before), len(after), missingKeys(before, after))
			}
			for path, sum := range before {
				if after[path] != sum {
					t.Fatalf("%s changed across Collect", path)
				}
			}
		})
	}
}

// snapshotTree maps every entry under root to a content fingerprint —
// directories to a marker, files to their byte sum. Byte-for-byte comparison
// of before/after proves Collect wrote nothing anywhere under the home.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatalf("walk home: %v", err)
	}
	return tree
}

func missingKeys(before, after map[string]string) []string {
	var extra []string
	for k := range after {
		if _, ok := before[k]; !ok {
			extra = append(extra, k)
		}
	}
	return extra
}
