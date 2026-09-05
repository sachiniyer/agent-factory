package bugreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
)

// Account-label redaction (#3871). The record-side policy already replaces
// InstanceData.Account with the marker (#3051, #3588), but the label reaches a
// bundle through two sections that carry text verbatim — the daemon log tail and
// the global config file — and naming the account is the point of both. So the
// label is removed at bundle time, from the set of names the accounts registry
// actually holds, in the one pass both sections share.

// accountHome plants a temp AF home and returns it, with one registered account
// per (agent, name) pair given. It registers them the way `af accounts add`
// does — the registry is a directory tree, so the fixture is the real thing.
func accountHome(t *testing.T, registered map[string][]string) string {
	t.Helper()
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	if err := os.MkdirAll(afHome, 0o755); err != nil {
		t.Fatalf("create af home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	for agent, names := range registered {
		for _, name := range names {
			if _, err := agentaccount.Register(afHome, agent, name); err != nil {
				t.Fatalf("register account %s/%s: %v", agent, name, err)
			}
		}
	}
	return afHome
}

// accountLogLines renders the shapes the daemon actually logs an account with:
// the %q form every current emitter uses, the path form agent_credentials.go
// prints, and the bare form accountlogin.go's unauthenticated-account error
// renders as "--account %s" — which is why quoted-only matching would leak.
func accountLogLines(label string) string {
	return strings.Join([]string{
		"account login: started claude in /tmp/x for claude account " + strconv.Quote(label),
		"device flow for account " + strconv.Quote(label) + " ended before the handover",
		"registered but not logged in — a session started with --account " + label + " would run unauthenticated",
		"account " + strconv.Quote(label) + " path " + strconv.Quote("/home/u/.agent-factory/accounts/claude/"+label),
		"next diagnostic survives",
	}, "\n")
}

// TestScrubLogRedactsRegisteredAccountLabel is the #3871 regression on the log
// section: a label the registry knows must not reach a shared bundle as log text,
// in the quoted shape, the bare shape, or as the last component of the account's
// own credential path. Against master every one of these leaks.
func TestScrubLogRedactsRegisteredAccountLabel(t *testing.T) {
	const label = "acme-prod"
	r := &redactor{}
	r.noteAccount(label)

	out := r.scrubLog(accountLogLines(label))

	mustNotContain(t, "daemon log", out, label, strconv.Quote(label))
	mustContain(t, "daemon log", out,
		"account login: started claude",
		strconv.Quote(redactedMarker),
		"--account "+redactedMarker+" would run unauthenticated",
		"next diagnostic survives")
}

// The config section is the OTHER half, and it shares no pass with the log except
// scrub(): collectConfig hands the global config file straight to scrub and
// touches nothing else. A fix placed beside the title pass covers the log and
// silently misses this, and no log-side assertion can tell. #3867's
// `default_accounts` and #3174's `limit_account_candidates` are the first config
// keys to hold an account name.
func TestScrubRedactsAccountLabelInTheConfigSection(t *testing.T) {
	r := &redactor{}
	r.noteAccount("work")

	out := r.scrub(strings.Join([]string{
		`[default_accounts]`,
		`codex = "work"`,
		`limit_account_candidates = ["work"]`,
	}, "\n"))

	mustNotContain(t, "config", out, `"work"`)
	mustContain(t, "config", out, `codex = `+strconv.Quote(redactedMarker))
}

// Anti-vacuity: the pass keys on the REGISTERED set, not on an "account <word>"
// shape. An unregistered name, and a near-miss of a registered one, are ordinary
// text and stay — otherwise the pass is indistinguishable from a regex that eats
// every word after "account".
func TestScrubLogKeepsUnregisteredAccountLikeText(t *testing.T) {
	r := &redactor{}
	r.noteAccount("acme-prod")

	in := strings.Join([]string{
		`af skill: cannot resolve account "acme-staging" for claude`,
		"account login: started claude in /tmp/x for claude account " + strconv.Quote("acmeprod"),
		"reaped the accounts reservation directory",
	}, "\n")
	out := r.scrubLog(in)

	mustContain(t, "daemon log", out,
		`account "acme-staging" for claude`,
		strconv.Quote("acmeprod"),
		"reaped the accounts reservation directory")
	mustNotContain(t, "daemon log", out, redactedMarker)
}

// Short, common account names are legal — "work", "dev", "personal" — and in a
// bundled config they also sit inside unrelated settings. The boundary is the
// LABEL's alphabet (letters, digits, '.', '-', '_'), not the general word rune the
// title and username passes use, precisely because the two disagree about '-' and
// '.': under the word rule "work" is a complete token inside "work-stuff" and
// redacting it would corrupt a setting the report is supposed to show.
func TestScrubKeepsAccountLabelInsideALongerToken(t *testing.T) {
	r := &redactor{}
	r.noteAccount("work")
	r.noteAccount("claude")

	out := r.scrub(strings.Join([]string{
		`branch_prefix = "work-stuff"`,
		`workspace_root = "/srv/workspaces"`,
		`program_overrides = { codex = "/usr/local/bin/claude-wrapper" }`,
		`version = "1.work.2"`,
	}, "\n"))

	mustContain(t, "config", out,
		`branch_prefix = "work-stuff"`,
		`workspace_root = "/srv/workspaces"`,
		`"/usr/local/bin/claude-wrapper"`,
		`version = "1.work.2"`)
	mustNotContain(t, "config", out, redactedMarker)
}

// The residual of that trade, pinned as a decision rather than left to be
// discovered. A WHOLE token equal to the label is redacted wherever it appears —
// including a path component and a config or JSON key that happens to be spelled
// the same as somebody's account.
//
// It has to be that way round. An account's own credential directory ends in
// exactly that component, and agent_credentials.go logs `account %q path %q`, so
// refusing a match after '/' would leave the label in the bundle through the one
// line that prints it as a path. Keys are the same shape as values in every format
// bundled here, and the one textual rule that would separate them — refuse a match
// followed by `":` — is exactly the shape of `cannot resolve account %q: %w`
// (instance_factory.go:813), so it would trade a config nicety for a log leak.
//
// What is left is over-redaction for an operator who names an account after a
// config key: their bundle marks a key it did not have to. That is visible in a
// file they are told to open and read, and it is the safe direction for an
// artifact whose whole purpose is to be handed to a stranger.
func TestScrubRedactsAWholeTokenMatchingAnAccountLabel(t *testing.T) {
	r := &redactor{}
	r.noteAccount("claude")

	out := r.scrub(strings.Join([]string{
		"account path /home/u/.agent-factory/accounts/claude/claude",
		`program_overrides = { claude = "/opt/bin/claude-2" }`,
	}, "\n"))

	mustContain(t, "over-redaction residual", out,
		"/accounts/"+redactedMarker+"/"+redactedMarker,
		"{ "+redactedMarker+` = "/opt/bin/claude-2" }`)
}

// A shorter registered label alongside a longer one must not strand the longer
// one's suffix. The label alphabet already refuses "acme" inside "acme-prod", and
// the sweep is longest-first besides; this pins the outcome either way, because
// the ordering is the invariant a reader of the title pass expects to hold here.
func TestScrubLogRedactsLongerAccountLabelBeforeItsPrefix(t *testing.T) {
	r := &redactor{}
	r.noteAccount("acme")
	r.noteAccount("acme-prod")

	out := r.scrubLog(accountLogLines("acme-prod"))

	mustNotContain(t, "daemon log", out, "acme", "-prod", "acme-prod")
	mustContain(t, "daemon log", out, redactedMarker, "next diagnostic survives")
}

// The same prefix hazard ACROSS the sets: a session title that is a word-boundary
// prefix of a registered account label. The title pass matches on word runes, so
// it DOES rewrite "acme" inside "acme-prod"; if it ran first the exact match for
// the label would be gone before the catch-all saw it and "-prod" would ship. The
// log and diagnostic passes therefore sweep labels first.
func TestScrubLogRedactsAccountLabelSharingASessionTitlePrefix(t *testing.T) {
	r := &redactor{}
	r.noteTitle("acme")
	r.noteAccount("acme-prod")

	out := r.scrubLog(accountLogLines("acme-prod"))

	mustNotContain(t, "daemon log", out, "acme", "-prod", "acme-prod")
	mustContain(t, "daemon log", out, redactedMarker, "next diagnostic survives")
}

// A report must read consistently: the marker in the log and config sections is
// the SAME one the typed record path puts in InstanceData.Account, so a reader is
// not left wondering whether two redactions mean two different things. Derived
// from the record path rather than hardcoded, so the two cannot drift.
func TestAccountMarkerMatchesTheRecordPath(t *testing.T) {
	const label = "acme-prod"
	record := &session.InstanceData{ID: "abc", Account: label}
	redactOneInstanceData(record)
	if record.Account == "" || record.Account == label {
		t.Fatalf("record path did not redact the account label: %q", record.Account)
	}

	r := &redactor{}
	r.noteAccount(label)
	out := r.scrubLog("af skill: cannot resolve account " + strconv.Quote(label) + " for claude")

	mustContain(t, "daemon log", out, "cannot resolve account "+strconv.Quote(record.Account))
}

// scrubDiagnostic exists because an af-authored diagnostic that QUOTES another
// component's error names the same things the log does, and "a second policy for
// it would drift from the first" (#3588). A login or swap refusal carries the
// candidate account through %w, so it is one of those things.
func TestScrubDiagnosticRedactsRegisteredAccountLabel(t *testing.T) {
	const label = "acme-prod"
	r := &redactor{}
	r.noteAccount(label)

	out := r.scrubDiagnostic("restore failed: account " + strconv.Quote(label) + " is not registered for claude")

	mustNotContain(t, "diagnostic", out, label, strconv.Quote(label))
	mustContain(t, "diagnostic", out, "is not registered for claude", redactedMarker)
}

// noteRegisteredAccounts reads the registry the way `af accounts list` does, for
// every account-scoped agent — not just the one this box happens to use.
func TestNoteRegisteredAccountsReadsEveryAgentsRegistry(t *testing.T) {
	agents := sessionenv.AccountAgents()
	if len(agents) < 2 {
		t.Skipf("need at least two account-scoped agents, have %v", agents)
	}
	registered := map[string][]string{}
	for i, agent := range agents {
		registered[agent] = []string{"regacct" + strconv.Itoa(i)}
	}
	accountHome(t, registered)

	r := &redactor{}
	if err := r.noteRegisteredAccounts(); err != nil {
		t.Fatalf("noteRegisteredAccounts: %v", err)
	}

	for i := range agents {
		label := "regacct" + strconv.Itoa(i)
		if _, ok := r.accounts[label]; !ok {
			t.Errorf("account %q was registered but not noted; noted %v", label, r.accounts)
		}
	}
}

// A fresh install has no accounts directory at all. That is an ordinary state, not
// a failure — reporting it would put a scary line in every bug report filed by
// someone who has never touched accounts.
func TestNoteRegisteredAccountsAcceptsAnEmptyRegistry(t *testing.T) {
	accountHome(t, nil)

	r := &redactor{}
	if err := r.noteRegisteredAccounts(); err != nil {
		t.Fatalf("an install with no accounts must not be an error: %v", err)
	}
	if len(r.accounts) != 0 {
		t.Errorf("expected no noted accounts, got %v", r.accounts)
	}
}

// A registry that cannot be read is REPORTED. Failing closed on the bundle —
// refusing to build one — would deny a report to the user who most needs to file
// it; failing closed on the REPORT means the bundle is still built and says, in
// its own collection errors, that account labels were not swept out of it.
func TestNoteRegisteredAccountsReportsAnUnreadableRegistry(t *testing.T) {
	agents := sessionenv.AccountAgents()
	if len(agents) < 2 {
		t.Skipf("need at least two account-scoped agents, have %v", agents)
	}
	broken, readable := agents[0], agents[1]
	afHome := accountHome(t, map[string][]string{readable: {"readable-acct"}})
	// A regular file where the agent's account directory belongs: ReadDir fails
	// with ENOTDIR for every user, root included, so the case does not depend on
	// permissions the test runner may not have.
	writeFile(t, filepath.Join(afHome, agentaccount.DirName, broken), "not a directory")

	r := &redactor{}
	err := r.noteRegisteredAccounts()
	if err == nil {
		t.Fatal("an unreadable account registry must be reported, not skipped in silence")
	}
	mustContain(t, "registry error", err.Error(), broken, accountsUnredactedWarning)

	// Anti-vacuity: one unreadable agent must not discard the labels the other
	// agents did yield, or a single broken directory would silently un-redact every
	// account on the box.
	if _, ok := r.accounts["readable-acct"]; !ok {
		t.Errorf("a readable agent's accounts must still be noted; noted %v", r.accounts)
	}
}

// End to end: a real registry, a real log file, a real config file, and the bundle
// a user would attach. The sweep is only reached once Build reads the registry, so
// a pass nobody wires up is still a leak.
func TestBuildScrubsRegisteredAccountLabelFromLogAndConfig(t *testing.T) {
	const label = "acme-prod"
	afHome := accountHome(t, map[string][]string{"claude": {label}})
	writeFile(t, filepath.Join(afHome, "agent-factory.log"), accountLogLines(label)+"\n")
	writeFile(t, filepath.Join(afHome, config.TomlConfigFileName), strings.Join([]string{
		`branch_prefix = "acme-prod-branches"`,
		"[default_accounts]",
		`claude = "` + label + `"`,
	}, "\n")+"\n")

	res, err := Build(Inputs{AFVersion: "test", GeneratedAt: "now"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for surface, out := range map[string]string{"text": res.Text, "json": string(res.JSON), "draft": res.Body} {
		mustNotContain(t, surface, out, strconv.Quote(label), "--account "+label)
		mustContain(t, surface, out, "next diagnostic survives")
	}
	// The config value is marked; the longer branch_prefix that merely CONTAINS the
	// label is left alone. Asserted on the text bundle, which carries the whole
	// config section rather than the draft's bounded excerpt.
	mustContain(t, "text", res.Text, `claude = `+strconv.Quote(redactedMarker), `branch_prefix = "acme-prod-branches"`)
}

// And the reported half, end to end: an unreadable registry reaches the bundle's
// own collection errors, so whoever reads the bundle knows it was not swept.
func TestBuildReportsAnUnreadableAccountRegistry(t *testing.T) {
	afHome := accountHome(t, nil)
	writeFile(t, filepath.Join(afHome, agentaccount.DirName), "not a directory")
	writeFile(t, filepath.Join(afHome, "agent-factory.log"), "next diagnostic survives\n")

	res, err := Build(Inputs{AFVersion: "test", GeneratedAt: "now"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var bundle struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(res.JSON, &bundle); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	mustContain(t, "collection errors", strings.Join(bundle.Errors, "\n"), accountsUnredactedWarning)
	mustContain(t, "text bundle", res.Text, accountsUnredactedWarning)
}
