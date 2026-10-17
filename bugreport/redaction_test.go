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

// TestScrubAndScrubLogRedactIndentedContinuation is the other half of the
// credentialKeyPattern cross-line lock at the two vulnerable surfaces. A key
// followed by a value on the NEXT line, INDENTED, is a continuation of the key
// — the YAML/config-shaped text collectConfig and the daemon log tail can
// carry — and must still redact. Mirrors
// TestScrubAndScrubLogDoNotCrossNewlineOnCredentialKey at the same two
// surfaces; the indent gate recovers this shape without re-opening the
// left-margin cross-line failure locked in that companion.
func TestScrubAndScrubLogRedactIndentedContinuation(t *testing.T) {
	r := &redactor{}

	// collectConfig path: a config-shaped key with its value on an indented
	// next line redacts the value and preserves the key, so triage sees that
	// a credential was configured there without leaking it.
	cfg := "password:\n  hunter2secret\n"
	got := r.scrub(cfg)
	if strings.Contains(got, "hunter2secret") {
		t.Fatalf("indented config.toml value survived scrub:\n in: %q\nout: %q", cfg, got)
	}
	if !strings.Contains(got, "password:") {
		t.Fatalf("key half absorbed by scrub:\n in: %q\nout: %q", cfg, got)
	}
	if !strings.Contains(got, secretMarker) {
		t.Fatalf("expected the indented value redacted:\n in: %q\nout: %q", cfg, got)
	}

	// collectLog path: the same shape in the daemon log tail. A key ending
	// one line whose value is pasted on an indented next line redacts the
	// value and leaves the following unrelated line intact.
	log := "2026-01-01 set token:\n  abcdefghijkl1234\n2026-01-01 daemon started\n"
	gotLog := r.scrubLog(log)
	if strings.Contains(gotLog, "abcdefghijkl1234") {
		t.Fatalf("indented log-tail value survived scrubLog:\n in: %q\nout: %q", log, gotLog)
	}
	if !strings.Contains(gotLog, "2026-01-01 daemon started") {
		t.Fatalf("unrelated following line absorbed by scrubLog:\n in: %q\nout: %q", log, gotLog)
	}
	if !strings.Contains(gotLog, "set token:") {
		t.Fatalf("key half absorbed by scrubLog:\n in: %q\nout: %q", log, gotLog)
	}
}

// TestScrubAndScrubLogRedactUnindentedQuotedJSON locks the JSON shape the indent
// gate alone misses, at the two vulnerable surfaces. JSON permits insignificant
// whitespace around `:`, so a value can sit on the next line at column zero;
// credentialKeyPattern's separator keeps the cross-line case indented
// (`\r?\n[ \t]+`), so this shape is recovered by keyValueSecret's value half,
// not the separator. The pre-narrowing `\s*` redacted it; losing it is the
// leaking side for a scrubber. An empty redactor isolates the credscrub pass.
func TestScrubAndScrubLogRedactUnindentedQuotedJSON(t *testing.T) {
	r := &redactor{}

	// collectConfig path: config-shaped text a user pasted can carry a JSON
	// credential with its value on the next line, unindented. The value
	// redacts and the key survives.
	cfg := "{\"password\":\n\"hunter2secret\"}"
	got := r.scrub(cfg)
	if strings.Contains(got, "hunter2secret") {
		t.Fatalf("unindented JSON value survived scrub:\n in: %q\nout: %q", cfg, got)
	}
	if !strings.Contains(got, secretMarker) {
		t.Fatalf("expected the unindented JSON value redacted:\n in: %q\nout: %q", cfg, got)
	}
	if !strings.Contains(got, `"password":`) {
		t.Fatalf("key half absorbed by scrub:\n in: %q\nout: %q", cfg, got)
	}

	// collectLog path: the same shape in the daemon log tail. A daemon log
	// line ending in `token:` whose next line is an unindented quoted value
	// redacts the value and leaves a following unrelated line intact.
	log := "2026-01-01 set token:\n\"abcdefghijkl1234\"\n2026-01-01 daemon started\n"
	gotLog := r.scrubLog(log)
	if strings.Contains(gotLog, "abcdefghijkl1234") {
		t.Fatalf("unindented quoted log-tail value survived scrubLog:\n in: %q\nout: %q", log, gotLog)
	}
	if !strings.Contains(gotLog, "2026-01-01 daemon started") {
		t.Fatalf("unrelated following line absorbed by scrubLog:\n in: %q\nout: %q", log, gotLog)
	}
	if !strings.Contains(gotLog, "set token:") {
		t.Fatalf("key half absorbed by scrubLog:\n in: %q\nout: %q", log, gotLog)
	}

	// No-regression half: a column-0 BARE next line is the unrelated-line
	// shape the cross-line guard exists to keep, and the quote-gated value
	// alternative must not reach it (a bare SHA has no opening `"`).
	logSHA := "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login\n2026-01-01 daemon started\n"
	if got := r.scrubLog(logSHA); got != logSHA {
		t.Fatalf("scrubLog redacted triage context off the bare line after a credential keyword:\n in: %q\nout: %q", logSHA, got)
	}
}

// TestScrubAndScrubLogRedactNewlineBeforeJSONColon is the end-to-end lock at the
// two vulnerable surfaces for the JSON-key-with-newline-before-`:` shape the
// credscrub pass recovers (the unit half is in internal/credscrub). JSON
// permits insignificant whitespace — including a newline — between a property
// name and `:`, so a machine-serialized `{"password"\n:\n"hunter2secret"}`
// placed a newline before the colon and the credential leaked. An empty
// redactor isolates the credscrub pass from path/username/title sweeps.
func TestScrubAndScrubLogRedactNewlineBeforeJSONColon(t *testing.T) {
	r := &redactor{}

	// collectConfig path: config-shaped text a user pasted can carry a JSON key
	// with a newline before `:` AND a newline before the quoted value. The value
	// redacts with the JSON line structure preserved and the key survives.
	cfg := "{\"password\"\n:\n\"hunter2secret\"}"
	wantCfg := "{\"password\"\n:\n\"" + secretMarker + "\"}"
	got := r.scrub(cfg)
	if got != wantCfg {
		t.Fatalf("newline-before-JSON-colon did not redact with structure preserved:\n in: %q\n want: %q\n out: %q", cfg, wantCfg, got)
	}
	if !strings.Contains(got, `"password"`) {
		t.Fatalf("key half absorbed by scrub:\n in: %q\n out: %q", cfg, got)
	}

	// collectLog path: the same shape in the daemon log tail — a daemon line
	// ending in a quoted credential key (`"token"`) with a newline before the
	// colon and a quoted value on the next line — redacts with structure
	// preserved, and an unrelated following line survives.
	log := "2026-01-01 \"token\"\n:\n\"abcdefghijkl1234\"\n2026-01-01 daemon started\n"
	wantLog := "2026-01-01 \"token\"\n:\n\"" + secretMarker + "\"\n2026-01-01 daemon started\n"
	gotLog := r.scrubLog(log)
	if gotLog != wantLog {
		t.Fatalf("newline-before-JSON-colon did not redact with structure preserved over the log tail:\n in: %q\n want: %q\n out: %q", log, wantLog, gotLog)
	}
	if !strings.Contains(gotLog, "2026-01-01 daemon started") {
		t.Fatalf("unrelated following line absorbed by scrubLog:\n in: %q\n out: %q", log, gotLog)
	}

	// No-regression half: a BARE key with a newline before the colon is the
	// ambiguous-log-line shape the cross-newline guard exists for; only the
	// symmetrically-quoted JSON-key widening reaches across it, so a bare key
	// survives unchanged. (The bare `token:` followed by a 40-char SHA at the
	// left margin is pinned verbatim in TestScrubAndScrubLogDoNotCrossNewline.)
	bare := "password\n:hunter2secret"
	if got := r.scrub(bare); got != bare {
		t.Fatalf("quoted-key widening reached a bare-key newline before colon:\n in: %q\n out: %q", bare, got)
	}
}

// TestScrubAndScrubLogDoNotCrossNewlineOnCredentialKey is the end-to-end lock at
// the two vulnerable surfaces, for the credentialKeyPattern class of cross-
// newline defect. collectConfig hands the whole config.toml to r.scrub
// (bugreport.go), and collectLog hands the daemon log tail to r.scrubLog
// (redact.go), both as genuine multi-line text blobs in a single call.
// credentialKeyPattern's separator used to be `\s*[:=]\s*`, and `\s` matches
// newlines, so a line ending in a bare credential keyword (`token:`, `auth:`,
// `secret:`, `password:`) + `:`/`=` reached across the newline and redacted the
// leading run of the next, unrelated line. An empty redactor isolates the
// credscrub pass from the path/username/title sweeps; this mirrors
// TestScrubAndScrubLogDoNotCrossNewlineOnSchemeWord, the same two-surface lock
// the file already keeps for the authScheme separator.
func TestScrubAndScrubLogDoNotCrossNewlineOnCredentialKey(t *testing.T) {
	r := &redactor{}

	// collectConfig path: a comment ending in a bare credential keyword, then a
	// TOML key on the next line, stays unchanged — the TOML key it documents
	// survives, instead of being absorbed as a "value" across the newline.
	cfg := "[network]\n# uses a token\nrequire_secret = true\n"
	if got := r.scrub(cfg); got != cfg {
		t.Fatalf("scrub crossed a newline over config.toml text with a credential keyword:\n in: %q\nout: %q", cfg, got)
	}

	// The same shape on `=`: a comment ending in a credential keyword followed
	// by a TOML key whose own value redacts. The comment AND the next-line key
	// name survive; only the same-line value is redacted, in place, by the
	// intended same-line keyValueSecret pass.
	cfgSecret := "# fall back to a secret\nhttp_token = \"x\"\n"
	got := r.scrub(cfgSecret)
	if !strings.Contains(got, "# fall back to a secret") {
		t.Fatalf("comment ending in 'secret' absorbed across the newline:\n in: %q\nout: %q", cfgSecret, got)
	}
	if !strings.Contains(got, "http_token") {
		t.Fatalf("TOML key name 'http_token' absorbed across the newline:\n in: %q\nout: %q", cfgSecret, got)
	}
	if !strings.Contains(got, `http_token = "`+secretMarker+`"`) {
		t.Fatalf("expected the value redacted in place, leaving the key name:\n in: %q\nout: %q", cfgSecret, got)
	}

	// collectLog path: a daemon log line ending in bare `token:` followed by a
	// normal timestamped line stays unchanged, so the next line's date prefix
	// survives. The timestamp 2026-01-01 is 10 chars of [0-9-], in the bare
	// value class, which is exactly what let `\s*` absorb it before.
	log := "2026-01-01 set token:\n2026-01-01 daemon started\n"
	if got := r.scrubLog(log); got != log {
		t.Fatalf("scrubLog crossed a newline over the daemon log tail with a credential keyword:\n in: %q\nout: %q", log, got)
	}

	// The richer composition keyedSchemeSecret extends: a log line ending in
	// `token:` whose next line is a 40-char git SHA + commit subject (the shape
	// `git log --format='%H %s'` produces) had BOTH the SHA and the subject
	// redacted off the next line before the fix. After the fix the entire
	// next line survives verbatim, and an unrelated following line survives too.
	logSHA := "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login\n2026-01-01 daemon started\n"
	if got := r.scrubLog(logSHA); got != logSHA {
		t.Fatalf("scrubLog redacted triage context off the line after a credential keyword:\n in: %q\nout: %q", logSHA, got)
	}

	// No-regression half: a genuine same-line credential within the log tail is
	// still redacted, and the following unrelated line is preserved.
	logReal := "2026-01-01 token: abcdefghijkl1234\n2026-01-01 daemon started\n"
	gotLog := r.scrubLog(logReal)
	if strings.Contains(gotLog, "abcdefghijkl1234") {
		t.Fatalf("same-line credential survived scrubLog:\n in: %q\nout: %q", logReal, gotLog)
	}
	if !strings.Contains(gotLog, "2026-01-01 daemon started") {
		t.Fatalf("unrelated following line absorbed by scrubLog:\n in: %q\nout: %q", logReal, gotLog)
	}
}

// TestScrubAndScrubLogRedactYAMLFlowMapping locks the YAML flow-mapping shape
// at the collectConfig surface. Inside a `{...}` flow mapping, indentation is
// not significant, so a value can legally begin at the left margin — the
// pre-narrowing `\s*` redacted this, and the loose form's `[ \t]*[:=]`
// separator stops at the newline, the single-linebreak value alt requires a
// `"`, and the JSON path requires a quoted key, so none redacted the bare
// value. The flow-collection-gated third alternative in credentialKeyPattern
// recovers the bare / single-quoted value at this surface without re-opening
// the bare-key cross-newline guard (a bare credential keyword without a flow
// introducer is pinned in TestScrubAndScrubLogDoNotCrossNewlineOnCredentialKey).
// The daemon log tail indents every captured output line two spaces, so a
// column-0 flow value never reaches `scrubLog`; the config path is the leak
// surface and is what this test pins.
func TestScrubAndScrubLogRedactYAMLFlowMapping(t *testing.T) {
	r := &redactor{}

	// collectConfig path: a user's pasted YAML flow mapping carrying a
	// credential whose value sits at the left margin inside the braces. The
	// value redacts and the surrounding flow braces / key survive — only the
	// value is replaced.
	cfg := "{password:\nhunter2secret}"
	got := r.scrub(cfg)
	if strings.Contains(got, "hunter2secret") {
		t.Fatalf("flow-mapping unindented value survived scrub:\n in: %q\n out: %q", cfg, got)
	}
	if !strings.Contains(got, "{password:") {
		t.Fatalf("flow introducer / key half absorbed by scrub:\n in: %q\n out: %q", cfg, got)
	}
	if !strings.Contains(got, secretMarker) {
		t.Fatalf("expected the flow-mapping value redacted:\n in: %q\n out: %q", cfg, got)
	}

	// collectConfig path: a single-quoted YAML flow value (the other
	// unindented YAML shape the indent gate and JSON path miss) also redacts,
	// preserving the surrounding quotes.
	cfgSingle := "{password:\n'hunter2secret'}"
	gotSingle := r.scrub(cfgSingle)
	if strings.Contains(gotSingle, "hunter2secret") {
		t.Fatalf("flow-mapping single-quoted value survived scrub:\n in: %q\n out: %q", cfgSingle, gotSingle)
	}
	if !strings.Contains(gotSingle, "{password:") {
		t.Fatalf("flow introducer / key half absorbed by scrub:\n in: %q\n out: %q", cfgSingle, gotSingle)
	}
	if !strings.Contains(gotSingle, secretMarker) {
		t.Fatalf("expected the flow-mapping single-quoted value redacted:\n in: %q\n out: %q", cfgSingle, gotSingle)
	}

	// collectConfig path: an embedded credential introduced by `,` — the
	// flow mapping's item separator is also a flow introducer, so the gate
	// fires for the second item too. The value redacts and the earlier
	// `{a: 1, password:\n` prefix (including the comma) survives.
	cfgComma := "{a: 1, password:\nhunter2secret}"
	gotComma := r.scrub(cfgComma)
	if strings.Contains(gotComma, "hunter2secret") {
		t.Fatalf("embedded flow-mapping value survived scrub:\n in: %q\n out: %q", cfgComma, gotComma)
	}
	if !strings.Contains(gotComma, "{a: 1, password:") {
		t.Fatalf("flow introducer / key half absorbed by scrub:\n in: %q\n out: %q", cfgComma, gotComma)
	}
	if !strings.Contains(gotComma, secretMarker) {
		t.Fatalf("expected the embedded flow-mapping value redacted:\n in: %q\n out: %q", cfgComma, gotComma)
	}

	// No-regression half: a BARE credential keyword ending a log line
	// without a flow introducer reaches only the loose form, so a column-0
	// bare next line (the daemon-log SHA + commit-subject shape) is still
	// preserved over the log tail. The flow gate must not reach it.
	logSHA := "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login\n2026-01-01 daemon started\n"
	if got := r.scrubLog(logSHA); got != logSHA {
		t.Fatalf("flow gate reached a bare key without a flow introducer over the log tail:\n in: %q\n out: %q", logSHA, got)
	}
}

// TestScrubAndScrubLogRecoversCrossNewlineEndToEnd pins the cross-newline
// recoveries the latest credscrub round restores at the two real surfaces: the
// config path (collectConfig's `r.scrub` over config.toml-shape text) and the
// log path (scrubLog's tail). A whitespace-only blank line before an indented
// continuation (`password:\n  \n  hunter2secret`), a JSON key with mismatched
// quotes (`"password'` — must NOT over-redact the quoted diagnostic), a comma in
// log prose (`request failed, token:\n2026-01-01` — must NOT over-redact the
// next record), and YAML explicit-key syntax (`? password\n: hunter2secret`)
// exercise the four codex findings end-to-end on the same paths users hit.
func TestScrubAndScrubLogRecoversCrossNewlineEndToEnd(t *testing.T) {
	r := &redactor{}

	// Whitespace-only blank line before the indented continuation: the
	// config path redacts the value and keeps the key half.
	cfgBlank := "password:\n  \n  hunter2secret"
	if got := r.scrub(cfgBlank); strings.Contains(got, "hunter2secret") {
		t.Fatalf("ws-only blank-line continuation survived scrub:\n in: %q\n out: %q", cfgBlank, got)
	} else if !strings.Contains(got, "password:") || !strings.Contains(got, secretMarker) {
		t.Fatalf("ws-only blank-line continuation not redacted with prefix:\n in: %q\n out: %q", cfgBlank, got)
	}

	// Mismatched JSON quotes (the pre-narrowing `["']...["']` made the JSON
	// form reach across the cross-newline colon and redact an unrelated
	// quoted diagnostic; the narrower `"..."` does not). The diagnostic
	// survives end-to-end.
	cfgMismatched := "\"password'\n:\n\"build failed: see log\""
	if got := r.scrub(cfgMismatched); strings.Contains(got, secretMarker) {
		t.Fatalf("mismatched-quoted JSON-shaped log fragment over-redacted:\n in: %q\n out: %q", cfgMismatched, got)
	} else if !strings.Contains(got, "build failed: see log") {
		t.Fatalf("diagnostic under mismatched JSON quotes lost:\n in: %q\n out: %q", cfgMismatched, got)
	}

	// Comma in log prose (no enclosing `{`): the next record survives.
	logProse := "request failed, token:\n2026-01-01 daemon started\n"
	if got := r.scrubLog(logProse); strings.Contains(got, secretMarker) {
		t.Fatalf("flow gate reached a comma in log prose over the log tail:\n in: %q\n out: %q", logProse, got)
	}

	// YAML explicit-key syntax: the colon on the next line is reached end-
	// to-end and the `? password` key marker survives.
	cfgExplicit := "? password\n: hunter2secret"
	if got := r.scrub(cfgExplicit); strings.Contains(got, "hunter2secret") {
		t.Fatalf("YAML explicit-key value survived scrub:\n in: %q\n out: %q", cfgExplicit, got)
	} else if !strings.Contains(got, "? password") || !strings.Contains(got, secretMarker) {
		t.Fatalf("YAML explicit-key not redacted with prefix:\n in: %q\n out: %q", cfgExplicit, got)
	}

	// No-regression half (the cross-newline guard the PR exists for): a
	// column-0 next line after a credential-key-on-the-previous-line stays
	// preserved end-to-end over the log tail. The comma-in-prose case
	// reaches the log surface too; preserve the same example's record.
	if got := r.scrubLog("checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login\n2026-01-01 daemon started\n"); !strings.Contains(got, "fix-login") {
		t.Fatalf("left-margin bare token over-redacted over the log tail:\n in: %q\n out: %q", "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login", got)
	}
}
