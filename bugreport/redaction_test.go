package bugreport

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// Username / branch redaction cases (#2533). Split out of bugreport_test.go to
// keep that file under the 1500-line limit (#1145); package and helpers are shared.

// redactOneInstanceData runs the per-record policy the way redactInstancesJSON
// does: register what the record knows — its titles, its tmux names, its path
// roots — and only then redact against that. Naming a path takes the run's roots
// exactly as removing a title takes the run's titles (#3588), so a test that
// called the redaction alone would be testing a configuration production never
// uses.
func redactOneInstanceData(d *session.InstanceData) {
	r := &redactor{}
	r.noteSession(d)
	r.redactInstanceData(d)
}

// TestScrubRedactsUsernameEndingInNonWordChar is the #2533 regression: an OS
// username ending in a non-word rune (hyphen, dot, …) has no word boundary after
// it, so the old `\b<name>\b` regex never matched it. The branch field
// (`<username>/<session>`, left for the text scrub) then leaked the username in a
// bundle meant to be safe to share. Against master these leak.
func TestScrubRedactsUsernameEndingInNonWordChar(t *testing.T) {
	for _, tc := range []struct {
		name, user, in, leak, want string
	}{
		{"trailing hyphen", "test-", "branch test-/fix-login-bug", "test-/fix-login-bug", userMarker + "/fix-login-bug"},
		{"trailing dot", "svc.", "worktree /srv/svc./repo", "svc./repo", userMarker + "/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{home: "/home/" + tc.user, users: []string{tc.user}}
			out := r.scrub(tc.in)
			if strings.Contains(out, tc.leak) {
				t.Errorf("username ending in a non-word char leaked through scrub: %q\n%s", tc.leak, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q after scrub:\n%s", tc.want, out)
			}
		})
	}
}

// TestScrubDoesNotOverRedactUsernameInsideAWord guards the boundary from the other
// side: a username must not be blanked when it is only part of a longer word.
func TestScrubDoesNotOverRedactUsernameInsideAWord(t *testing.T) {
	r := &redactor{home: "/home/al", users: []string{"al"}}
	if out := r.scrub("algorithm alerts"); out != "algorithm alerts" {
		t.Errorf("username 'al' over-redacted inside a word: %q", out)
	}
}

// TestRedactInstanceDataBranchUsernameScrubbed is the end-to-end #2533 lock: the
// Branch field is not field-redacted (the repo retains the branch suffix by
// standing policy), so a username with a non-word tail must still be blanked out
// of the rendered branch by the scrub the redaction path applies.
func TestRedactInstanceDataBranchUsernameScrubbed(t *testing.T) {
	r := &redactor{home: "/home/test-", users: []string{"test-"}}
	if out := r.scrub(`"branch":"test-/fix-login-bug"`); strings.Contains(out, "test-/fix-login-bug") {
		t.Errorf("branch username leaked through the redaction scrub:\n%s", out)
	}
}

// TestScrubRedactsMixedCaseUsernameLowercasedInBranch is the #2533 mixed-case half:
// config.BranchPrefix lowercases the username, so a mixed-case account carries a
// LOWERCASED branch prefix. Byte-exact matching against the raw username alone
// misses it, so appendUserToken registers the lowercased form too. Against master
// (raw token only) the username ships verbatim.
func TestScrubRedactsMixedCaseUsernameLowercasedInBranch(t *testing.T) {
	r := &redactor{home: "/home/Sachin.Iyer", users: appendUserToken(nil, "Sachin.Iyer")}
	out := r.scrub(`"branch":"sachin.iyer/fix-login-bug"`)
	if strings.Contains(out, "sachin.iyer/fix-login-bug") {
		t.Errorf("lowercased branch username leaked for a mixed-case account:\n%s", out)
	}
	if !strings.Contains(out, userMarker+"/fix-login-bug") {
		t.Errorf("expected [user]/fix-login-bug after scrub:\n%s", out)
	}
}

// TestScrubUsernameLongestFirstAvoidsPrefixShadow is the #2533 ordering invariant:
// a short username that is a prefix of a longer one (raw "jdoe" vs a home basename
// "jdoe.admin") must be redacted longest-first, or redacting "jdoe" first destroys
// the only match for the longer token and strands ".admin" — the same invariant the
// title scrub already keeps. Without the sort this leaks ".admin".
func TestScrubUsernameLongestFirstAvoidsPrefixShadow(t *testing.T) {
	r := &redactor{home: "/home/jdoe.admin", users: []string{"jdoe", "jdoe.admin"}}
	out := r.scrub("branch jdoe.admin/fix-1")
	if strings.Contains(out, ".admin") {
		t.Errorf("shorter username shadowed the longer one, stranding a suffix:\n%s", out)
	}
	if !strings.Contains(out, userMarker+"/fix-1") {
		t.Errorf("expected [user]/fix-1 (longest-first match):\n%s", out)
	}
}

// TestScrubAndScrubLogDoNotCrossNewlineOnSchemeWord is the end-to-end lock at the
// two vulnerable surfaces named in the bug report. collectConfig hands the whole
// config.toml to r.scrub (bugreport.go), and collectLog hands the daemon log tail
// to r.scrubLog (redact.go), both as genuine multi-line text blobs in a single
// call. authScheme's separator used to be `\s+`, which matches newlines, so a
// line ending in bare `bearer`/`basic` absorbed the leading token-run of the
// next unrelated line — on the config path the TOML key the prior comment
// documented, on the log path the timestamp prefix of the next line. An empty
// redactor isolates the credscrub pass from the path/username/title sweeps.
func TestScrubAndScrubLogDoNotCrossNewlineOnSchemeWord(t *testing.T) {
	r := &redactor{}

	// collectConfig path: config.toml text ending a comment in bare `bearer`
	// stays unchanged, so the TOML key it documents survives.
	cfg := "[network]\n# demand a bearer\nrequire_token = true\n"
	if got := r.scrub(cfg); got != cfg {
		t.Fatalf("scrub crossed a newline over config.toml text:\n in: %q\nout: %q", cfg, got)
	}
	// The basic variant: the comment AND the key name survive; only the value
	// is redacted, in place, by the intended same-line keyValueSecret pass.
	cfgBasic := "# fall back to basic\nhttp_password = \"x\"\n"
	got := r.scrub(cfgBasic)
	if !strings.Contains(got, "# fall back to basic") {
		t.Fatalf("comment ending in 'basic' absorbed across the newline:\n in: %q\nout: %q", cfgBasic, got)
	}
	if !strings.Contains(got, "http_password") {
		t.Fatalf("TOML key name 'http_password' absorbed across the newline:\n in: %q\nout: %q", cfgBasic, got)
	}
	if !strings.Contains(got, `http_password = "`+secretMarker+`"`) {
		t.Fatalf("expected the value redacted in place, leaving the key name:\n in: %q\nout: %q", cfgBasic, got)
	}

	// collectLog path: a daemon log line ending in bare `bearer` followed by a
	// normal timestamped line stays unchanged, so the next line's date prefix
	// survives. The timestamp 2026-01-01 is 10 chars of [0-9-], both in the
	// token class, which is exactly what let `\s+` absorb it before.
	log := "2026-01-01 listener needs a bearer\n2026-01-01 daemon started\n"
	if got := r.scrubLog(log); got != log {
		t.Fatalf("scrubLog crossed a newline over the daemon log tail:\n in: %q\nout: %q", log, got)
	}

	// No-regression half: a genuine same-line credential within the log tail is
	// still redacted, and the following unrelated line is preserved.
	logReal := "2026-01-01 auth: Bearer abcdefghijkl1234\n2026-01-01 daemon started\n"
	gotLog := r.scrubLog(logReal)
	if strings.Contains(gotLog, "abcdefghijkl1234") {
		t.Fatalf("same-line credential survived scrubLog:\n in: %q\nout: %q", logReal, gotLog)
	}
	if !strings.Contains(gotLog, "2026-01-01 daemon started") {
		t.Fatalf("unrelated following line absorbed by scrubLog:\n in: %q\nout: %q", logReal, gotLog)
	}
}
