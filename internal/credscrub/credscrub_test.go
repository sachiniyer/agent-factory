package credscrub

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

const sentinel = "S3NT1NELVALUEDONOTLOG"

func TestScrubRemovesCredentialShapes(t *testing.T) {
	cases := []struct{ name, in string }{
		{"openai/anthropic key", "key sk-ant-" + sentinel + "abcdefghij here"},
		{"github PAT", "token ghp_" + sentinel + "abcdefghij here"},
		{"github fine-grained PAT", "token github_pat_" + sentinel + "abcdefghij"},
		{"slack token", "xoxb-" + sentinel + "-abcdef"},
		{"aws access key id", "AKIA" + strings.ToUpper(sentinel[:16])},
		{"google api key", "AIza" + sentinel + "abcdefghijklmnop1234"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIx" + sentinel + "In0.abcdefghij"},
		{"bare key=value", "SOME_API_KEY=" + sentinel},
		{"quoted key=value", `api_key = "` + sentinel + `"`},
		{"single quoted key=value", `token: '` + sentinel + `'`},
		{"authorization bearer", "Authorization: Bearer " + sentinel + "abcdef"},
		{"authorization basic", "Authorization: Basic " + sentinel + "abcdef"},
		// A scheme behind a key keyValueSecret DOES recognize. That pass takes
		// `Bearer` as the entire value — its bare class stops at the following
		// space — so with the scheme pass second the credential was left standing
		// behind a marker. `Authorization:` survives either order because the key
		// half never matches it, which is precisely why testing only that
		// spelling hid the bug.
		{"bearer behind a matching key", "auth: Bearer " + sentinel + "abcdef"},
		{"bearer behind a prefixed key", "x-auth-token=Bearer " + sentinel + "abcdef"},
		{"basic behind a matching key", "token: Basic " + sentinel + "abcdef"},
		// A scheme word authScheme does not know regenerates the stranded shape,
		// and enumerating scheme names is the losing game. Caught by the marker.
		{"unknown scheme behind a key", "auth: CustomScheme " + sentinel + "abcdefghij"},
		// The same shape as ALREADY PERSISTED to agent-factory.log by a binary
		// with the old ordering. The bug report re-bundles that tail, and by then
		// the scheme word is gone, so only the marker can key on it.
		{"already-stranded on disk", "auth: " + SecretMarker + " " + sentinel + "abcdefghij"},
		{"already-stranded, prefixed key", "x-auth-token=" + SecretMarker + " " + sentinel + "abcdefghij"},
		{"git url userinfo", "https://x-access-token:ghp_" + sentinel + "abcdefghij@github.com/o/r"},
		{"pem private key", "-----BEGIN RSA PRIVATE KEY-----\n" + sentinel + "\n-----END RSA PRIVATE KEY-----"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, sentinel) {
				t.Fatalf("credential survived Scrub: %q", got)
			}
		})
	}
}

// TestScrubRecoversShortStrandedToken pins the length floor against authScheme's.
// The table above asserts on a 21-char sentinel, so it cannot exercise a floor at
// all — an earlier version of this test passed with the floor at 16 because the
// case it added never contained the sentinel it asserted on.
func TestScrubRecoversShortStrandedToken(t *testing.T) {
	// 8 chars: exactly what authScheme treats as sensitive after `Bearer`, so a
	// line the old writer persisted as `auth: [redacted-secret] <8 chars>` has to
	// be recoverable too.
	const short = "Sh0rtTk1"
	got := Scrub("auth: " + SecretMarker + " " + short)
	if strings.Contains(got, short) {
		t.Fatalf("short stranded token survived; the recovery floor is stricter "+
			"than authScheme's, so it leaves exactly what authScheme would catch: %q", got)
	}
}

// TestScrubIsIdempotent is the #1260 lesson as a property: the bug report scrubs
// the same text repeatedly by design, and the log is now scrubbed on the way to
// disk and again when a bundle reads it back. A pass that re-wraps its own marker
// shipped 28 `[redacted-secret]]` in a real bundle.
func TestScrubIsIdempotent(t *testing.T) {
	inputs := []string{
		"SOME_API_KEY=" + sentinel,
		`api_key = "` + sentinel + `"`,
		"token ghp_" + sentinel + "abcdefghij",
		"Authorization: Bearer " + sentinel + "abcdef",
		"auth: " + SecretMarker + " " + sentinel + "abcdefghij",
		"nothing sensitive here, commit 4f2a9c1 on af_0f8fc14c_fix-login",
	}
	for _, in := range inputs {
		once := Scrub(in)
		twice := Scrub(once)
		if once != twice {
			t.Fatalf("Scrub not idempotent for %q:\n once: %q\ntwice: %q", in, once, twice)
		}
	}
}

// TestScrubRedactsValueThatMerelyBeginsWithAMarker pins the boundary that makes
// the already-redacted fast path sound. A value starting with a marker is NOT a
// marker, and treating it as one let a credential ride out behind it.
func TestScrubRedactsValueThatMerelyBeginsWithAMarker(t *testing.T) {
	got := Scrub("api_key=" + SecretMarker + sentinel)
	if strings.Contains(got, sentinel) {
		t.Fatalf("credential hiding behind a marker survived: %q", got)
	}
}

// TestScrubKeepsTriageContext guards the other direction. These patterns are
// deliberately narrow because a broad rule would destroy what a triager reads.
func TestScrubKeepsTriageContext(t *testing.T) {
	// A marker followed by short ordinary words must not be eaten by
	// strandedAfterMarker: it requires 16+ token-charset characters.
	if got := Scrub("token=" + SecretMarker + " and then it failed"); got != "token="+SecretMarker+" and then it failed" {
		t.Fatalf("stranded pass ate ordinary prose after a marker: %q", got)
	}
	// `\s` matches newlines, so a marker ending one line must not consume the
	// start of the next: over multi-line log blobs that silently deletes an
	// unrelated line.
	multi := "token=" + SecretMarker + "\nauthentication-failed-now what"
	if got := Scrub(multi); got != multi {
		t.Fatalf("stranded pass crossed a newline and ate the next line: %q", got)
	}
	wholeField := "auth: " + RedactedMarker + " authentication-failed-now"
	if got := Scrub(wholeField); got != wholeField {
		t.Fatalf("scheme recovery treated a whole-field marker as a credential marker: %q", got)
	}
	in := "worktree af_0f8fc14c_fix-login at 4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a removed; session id 01J8Z9"
	if got := Scrub(in); got != in {
		t.Fatalf("Scrub destroyed benign triage context:\n in: %q\nout: %q", in, got)
	}
}

// TestScrubAuthSchemeDoesNotCrossNewline is the authScheme analogue of the
// `multi` case in TestScrubKeepsTriageContext. Scrub runs over genuine
// multi-line text blobs from the bug-report bundle (the whole config.toml via
// bugreport.collectConfig and the daemon log tail via scrubLog), and the
// separator between the scheme word and its token used to be `\s+`, which
// matches newlines — so a line ending in bare `bearer`/`basic` absorbed the
// leading token-run of the next unrelated line. On the config path that run is
// the TOML key the prior comment documented; on the log path it is the
// timestamp prefix of the next line. The separator is now `[ \t]+`, which a
// line boundary is not, so none of these inputs cross it.
func TestScrubAuthSchemeDoesNotCrossNewline(t *testing.T) {
	// Inputs with no same-line credential, so the whole blob is a fixed point:
	// the scheme word stays stranded at end of line and the next line's leading
	// run (the TOML key / the log timestamp) survives verbatim.
	unchanged := []struct{ name, in string }{
		{"config: bearer comment + key", "[network]\n# demand a bearer\nrequire_token = true\n"},
		{"config: bearer comment + key (CRLF)", "[network]\r\n# demand a bearer\r\nrequire_token = true\r\n"},
		{"log: bearer line + timestamp", "2026-01-01 listener needs a bearer\n2026-01-01 daemon started\n"},
		{"log: basic line + timestamp", "2026-01-01 listener needs a basic\n2026-01-01 daemon started\n"},
		{"log: bearer line + timestamp (CRLF)", "2026-01-01 listener needs a bearer\r\n2026-01-01 daemon started\r\n"},
	}
	for _, tc := range unchanged {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scrub(tc.in); got != tc.in {
				t.Fatalf("authScheme crossed a newline and changed multi-line input:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}

	// The basic + http_password case independently redacts the value via
	// keyValueSecret (the intended same-line pass). The fix only has to keep
	// the comment and the key NAME from being absorbed across the newline; the
	// value still takes the marker, on its own line, and the key survives.
	t.Run("config: basic comment + key whose value redacts", func(t *testing.T) {
		in := "# fall back to basic\nhttp_password = \"x\"\n"
		got := Scrub(in)
		if !strings.Contains(got, "# fall back to basic") {
			t.Fatalf("comment ending in bare 'basic' was absorbed across the newline:\n in: %q\nout: %q", in, got)
		}
		if !strings.Contains(got, "http_password") {
			t.Fatalf("TOML key name 'http_password' was absorbed across the newline:\n in: %q\nout: %q", in, got)
		}
		if strings.Contains(got, "fall back to "+SecretMarker) {
			t.Fatalf("authScheme crossed the newline and left a straddling marker:\n in: %q\nout: %q", in, got)
		}
		if !strings.Contains(got, `http_password = "`+SecretMarker+`"`) {
			t.Fatalf("expected the value redacted in place, leaving the key name:\n in: %q\nout: %q", in, got)
		}
	})
}

// TestScrubAuthSchemeStillRedactsSameLineWithinMultiLine is the no-regression
// half of the cross-line fix: a genuine `Bearer <token>` / `Basic <token>` on
// one line within a multi-line blob is still redacted, and the marker does not
// bleed into a following unrelated line. Runs over the same multi-line text
// blobs the cross-line guard above pins, so both halves of the invariant stay
// together.
func TestScrubAuthSchemeStillRedactsSameLineWithinMultiLine(t *testing.T) {
	cases := []struct{ name, in, leak string }{
		{"bearer mid blob", "above line\nAuthorization: Bearer " + sentinel + "abcdef\n2026-01-01 below line\n", sentinel},
		{"basic mid blob", "above line\nAuthorization: Basic " + sentinel + "abcdef\n2026-01-01 below line\n", sentinel},
		// A tab is a legal HTTP scheme/token separator (RFC 7230) and `[ \t]+`
		// must still accept it just as `\s+` did.
		{"bearer tab-separated", "Authorization: Bearer\t" + sentinel + "abcdef", sentinel},
		{"basic tab-separated", "Authorization: Basic\t" + sentinel + "abcdef", sentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("same-line credential survived within multi-line input:\n in: %q\nout: %q", tc.in, got)
			}
			if !strings.Contains(got, SecretMarker) {
				t.Fatalf("expected a redaction marker for the same-line credential:\n in: %q\nout: %q", tc.in, got)
			}
			// Surrounding unrelated lines survive a multi-line scrub.
			if strings.Contains(tc.in, "above line") && !strings.Contains(got, "above line") {
				t.Fatalf("unrelated leading line was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
			if strings.Contains(tc.in, "2026-01-01 below line") && !strings.Contains(got, "2026-01-01 below line") {
				t.Fatalf("unrelated following line was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}
}

// TestScrubCredentialKeyDoesNotCrossNewline is the credentialKeyPattern analogue
// of TestScrubAuthSchemeDoesNotCrossNewline. credentialKeyPattern's separator
// used to be `\s*[:=]\s*`, and `\s` matches newlines. Scrub runs over genuine
// multi-line text blobs from the bug-report bundle (the whole config.toml via
// bugreport.collectConfig and the daemon log tail via bugreport.scrubLog), so a
// line ending in a bare credential keyword + `:`/`=` reached across the newline
// and redacted the leading run of the next, unrelated line. `keyValueSecret`
// alone redacted a bare ≥6-char next-line token; `keyedSchemeSecret` extended the
// redaction further along that next line (a 40-char git SHA and following commit
// subject, the shape `git log --format='%H %s'` produces). The separator now
// crosses the line boundary ONLY into an indented continuation
// (`\r?\n[ \t]+`); all of these inputs have the next line at the LEFT MARGIN with
// no leading whitespace, so it is not a continuation and none of them cross.
// Mirrors the precedent the file already set for authScheme and
// strandedAfterMarker so the three patterns cannot drift apart again. The
// indented-continuation half is pinned in TestScrubCredentialKeyRedactsIndentedContinuation.
func TestScrubCredentialKeyDoesNotCrossNewline(t *testing.T) {
	// Inputs with a credential keyword ending one line and no same-line
	// credential, so the whole blob is a fixed point: the keyword stays
	// stranded at end of line and the next line's leading run survives
	// verbatim. `label:` is a control that is NOT a credential keyword.
	unchanged := []struct{ name, in string }{
		{"token: + bare token", "token:\nabcdef123456"},
		{"token= + bare token", "token=\nabcdef123456"},
		{"control label:", "label:\nabcdef123456"},
		// The composition the report called out: keyedSchemeSecret inherits
		// credentialKeyPattern's cross-newline anchor and extends the redaction
		// along the next line via its own scheme+token tail, so a 40-char git
		// SHA and following commit subject (the `git log --format='%H %s'`
		// shape) were both redacted off the next line before the fix.
		{"sha + commit subject", "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login"},
		{"sha + commit subject (CRLF)", "checking token:\r\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login"},
		{"log: token line + timestamp", "2026-01-01 set token:\n2026-01-01 daemon started\n"},
		{"config: key ends line + next TOML key", "[network]\n# uses a token\nrequire_secret = true\n"},
	}
	for _, tc := range unchanged {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scrub(tc.in); got != tc.in {
				t.Fatalf("credentialKeyPattern crossed a newline and changed multi-line input:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}
}

// TestScrubCredentialKeyStillRedactsSameLineWithinMultiLine is the no-regression
// half of the cross-line fix: a genuine `<key>: <value>` / `<key> = <value>` on
// one line within a multi-line blob is still redacted (with the same whitespace
// spellings — spaces, tabs, and no whitespace around `:`/`=` — the old `\s*`
// matched on a single line), and the marker does not bleed into a following
// unrelated line. Runs over the same multi-line text blobs the cross-line guard
// above pins, so both halves of the invariant stay together.
func TestScrubCredentialKeyStillRedactsSameLineWithinMultiLine(t *testing.T) {
	cases := []struct{ name, in, leak string }{
		{"token: value mid blob", "above line\ntoken: " + sentinel + "\n2026-01-01 below line\n", sentinel},
		// No whitespace around `:`/`=` must still match on one line.
		{"token=value no whitespace", "token=" + sentinel + "abcdef", sentinel},
		// Tabs around the separator must still match — `[ \t]*` is what `\s*`
		// narrowed to, so a TOML-style `api_key\t=\t<value>` is still redacted.
		{"api_key = value (tabs)", "api_key\t=\t" + sentinel + "abcdef", sentinel},
		// A quoted value on one line within a multi-line blob is still redacted
		// and the marker does not bleed into the following unrelated line.
		{"quoted value mid blob", "above line\nsecret: \"" + sentinel + "\"\n2026-01-01 below line\n", sentinel},
		// Quoted key with a credential-word prefix (`"x-auth-token"`) on one line.
		{"quoted key value", "\"x-auth-token\": \"" + sentinel + "\"", sentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("same-line credential survived within multi-line input:\n in: %q\nout: %q", tc.in, got)
			}
			if !strings.Contains(got, SecretMarker) {
				t.Fatalf("expected a redaction marker for the same-line credential:\n in: %q\nout: %q", tc.in, got)
			}
			// Surrounding unrelated lines survive a multi-line scrub.
			if strings.Contains(tc.in, "above line") && !strings.Contains(got, "above line") {
				t.Fatalf("unrelated leading line was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
			if strings.Contains(tc.in, "2026-01-01 below line") && !strings.Contains(got, "2026-01-01 below line") {
				t.Fatalf("unrelated following line was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}
}

// TestScrubCredentialKeyRedactsIndentedContinuation locks the other half of
// the cross-line fix: a credential whose value sits on the NEXT line, INDENTED
// (`password:\n  hunter2secret`, the YAML/config shape a log tail can paste),
// is a continuation of the key and must still redact. A straight `[ \t]*` stop
// redacts nothing across a line boundary and so dropped this shape — trading
// over-redaction for under-redaction, which is the leaking side for a
// scrubber. The over-redaction cases the guard exists for — a credential
// keyword ending one line and a LEFT-MARGIN next line — are none of them
// indented, so the indent gate `\r?\n[ \t]+` recovers this shape without
// re-opening the cross-line failure (the left-margin half is pinned in
// TestScrubCredentialKeyDoesNotCrossNewline).
func TestScrubCredentialKeyRedactsIndentedContinuation(t *testing.T) {
	cases := []struct{ name, key, in, leak string }{
		// A bare value on an indented next line — the shape the straight
		// newline stop dropped (most passwords reach the scrubber this way,
		// their secrecy established solely by the neighbouring key).
		{"password: + indented bare", "password:", "password:\n  hunter2secret", "hunter2secret"},
		// A quoted value on an indented next line (the JSON/YAML shape).
		{`"auth": + indented quoted`, `"auth":`, "\"auth\":\n  \"abcdef123456\"", "abcdef123456"},
		// A value whose own shape is a known PAT — now also caught by key
		// proximity, not left to the PAT matcher alone.
		{"token: + indented PAT", "token:", "token:\n  ghp_AAAA0123456789BCDEFG", "ghp_AAAA0123456789BCDEFG"},
		// CRLF line ending between the key and the indented value.
		{"password: + indented bare (CRLF)", "password:", "password:\r\n  hunter2secret", "hunter2secret"},
		// Tabs indent the continuation just as spaces do.
		{"api_key= + indented bare (tabs)", "api_key=", "api_key=\n\t\thunter2secret", "hunter2secret"},
		// Trailing horizontal whitespace after the separator before the
		// line break — `password: \n  hunter2secret`. The leading `[ \t]*`
		// in the continuation alternative consumes it so the credential is
		// not left intact; without it neither alternative would match.
		{"password: + space then indented bare", "password:", "password: \n  hunter2secret", "hunter2secret"},
		{"password= + tab then indented bare", "password=", "password=\t\n  hunter2secret", "hunter2secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("indented-continuation credential survived:\n in: %q\nout: %q", tc.in, got)
			}
			if !strings.Contains(got, SecretMarker) {
				t.Fatalf("expected a redaction marker for the indented-continuation credential:\n in: %q\nout: %q", tc.in, got)
			}
			// The key and its separator survive — only the value is replaced
			// — so triage still sees WHERE the redaction was.
			if !strings.Contains(got, tc.key) {
				t.Fatalf("key half was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}

	// Boundary lock: a column-0 next line is NOT an indented continuation. It
	// is an unrelated line and survives, which is the half the cross-line
	// guard exists for — the indent gate must not reach it. (The full set of
	// left-margin cases lives in TestScrubCredentialKeyDoesNotCrossNewline.)
	t.Run("left-margin next line is not a continuation", func(t *testing.T) {
		in := "token:\nabcdefGHIJKL"
		if got := Scrub(in); got != in {
			t.Fatalf("indent gate reached a left-margin next line:\n in: %q\nout: %q", in, got)
		}
	})
}

// TestScrubCredentialKeyRedactsUnindentedQuotedJSON locks the JSON half the
// indent gate alone misses. JSON permits insignificant whitespace around `:`,
// so a machine-serialized `{"password":\n"hunter2secret"}` puts the value at the
// left margin (column zero), where the indented-continuation alternative
// `\r?\n[ \t]+` cannot reach it. The pre-narrowing `\s*` redacted that shape, so
// losing it is a regression to the leaking side; keyValueSecret's value half
// adds a `\r?\n"…"` alternative that recovers the unindented quoted value
// WITHOUT re-opening the left-margin bare-token guard — it fires only when the
// next line STARTS with `"`, so a column-0 bare token (the daemon-log SHA the
// cross-line guard exists to preserve) still does not match. The indented
// quoted-JSON shape stays covered by the indent gate; this is the
// column-0-only half.
func TestScrubCredentialKeyRedactsUnindentedQuotedJSON(t *testing.T) {
	cases := []struct{ name, key, in, leak string }{
		// The shape the report names: a JSON object with the value on the next
		// line at column zero (no indent), LF and CRLF.
		{`{"password": + next-line quoted`, `"password":`, `{"password":
"hunter2secret"}`, "hunter2secret"},
		{`{"password": + next-line quoted (CRLF)`, `"password":`, "{\"password\":\r\n\"hunter2secret\"}", "hunter2secret"},
		// A quoted key with an auth keyword, unindented JSON value.
		{`{"auth": + next-line quoted`, `"auth":`, `{"auth":
"abcdef123456"}`, "abcdef123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("unindented quoted JSON value survived:\n in: %q\nout: %q", tc.in, got)
			}
			if !strings.Contains(got, SecretMarker) {
				t.Fatalf("expected a redaction marker for the unindented JSON value:\n in: %q\nout: %q", tc.in, got)
			}
			// The key survives so triage sees where the redaction was.
			if !strings.Contains(got, tc.key) {
				t.Fatalf("key half was absorbed:\n in: %q\nout: %q", tc.in, got)
			}
		})
	}

	// No-regression half: a column-0 BARE next line is the unrelated-line
	// shape the cross-line guard exists for, and the quote-gated alternative
	// must not reach it — a bare token has no opening `"`.
	t.Run("left-margin bare next line is not a JSON value", func(t *testing.T) {
		in := "checking token:\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login"
		if got := Scrub(in); got != in {
			t.Fatalf("quote-gated alternative reached a left-margin bare line:\n in: %q\nout: %q", in, got)
		}
	})
}

// TestScrubCredentialKeyRedactsNewlineBeforeJSONColon locks the JSON-colon half
// of the key pattern the bare `[ \t]*[:=]` half drops. JSON permits
// insignificant whitespace — including a newline — between a property name
// and `:`, so a machine-serialized `{"password"\n:\n"hunter2secret"}` placed a
// newline before the colon and the credential reached logs and bug-report
// bundles unchanged. The key half now allows `[ \t]*(?:\r?\n[ \t]*)?` before
// `:` only when the key is SYMMETRICALLY QUOTED (the JSON-key shape); a bare
// key keeps the narrow `[ \t]*[:=]` separator, so the ambiguous bare-log-line
// shape the cross-newline guard exists for still cannot cross. Same-line
// continuation TestScrubCredentialKeyRedactsUnindentedQuotedJSON already
// covered stays the value half (`\r?\n"..."`); this is the key half.
func TestScrubCredentialKeyRedactsNewlineBeforeJSONColon(t *testing.T) {
	cases := []struct{ name, key, in, want, leak string }{
		// The shape the report names: a newline between the quoted key and `:`
		// AND between `:` and the next-line quoted JSON value. The line
		// structure (both newlines, the surrounding braces) survives; only the
		// quoted value is replaced.
		{`{"password"\n:\n"value"}`, `"password"`,
			"{\"password\"\n:\n\"hunter2secret\"}",
			"{\"password\"\n:\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		{`{"password"\n:\n"value"} (CRLF)`, `"password"`,
			"{\"password\"\r\n:\r\n\"hunter2secret\"}",
			"{\"password\"\r\n:\r\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Newline before the colon, value on the SAME line as the colon.
		{`{"password"\n: "value"}`, `"password"`,
			"{\"password\"\n: \"hunter2secret\"}",
			"{\"password\"\n: \"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Horizontal whitespace before the newline that introduces the colon.
		{`{"password" \n:\n"value"}`, `"password"`,
			"{\"password\" \n:\n\"hunter2secret\"}",
			"{\"password\" \n:\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// An auth keyword, quoted key, newline before colon, next-line value.
		{`{"auth"\n:\n"value"}`, `"auth"`,
			"{\"auth\"\n:\n\"abcdef123456\"}",
			"{\"auth\"\n:\n\"" + SecretMarker + "\"}",
			"abcdef123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("newline-before-colon JSON value survived:\n in: %q\n want: %q\n out: %q", tc.in, tc.want, got)
			}
			if got != tc.want {
				t.Fatalf("redaction did not preserve the JSON line structure:\n in: %q\n want: %q\n out: %q", tc.in, tc.want, got)
			}
			if !strings.Contains(got, tc.key) {
				t.Fatalf("key half absorbed:\n in: %q\n out: %q", tc.in, got)
			}
		})
	}

	// No-regression half: a BARE key with a newline before the colon is the
	// ambiguous-log-line shape the cross-newline guard exists for, and the
	// quoted-key widening must not reach it — a bare key has no surrounding
	// quotes, so the bare half of the separator stays narrow and does not cross.
	t.Run("bare key with newline before colon is not matched", func(t *testing.T) {
		in := "password\n:hunter2secret"
		if got := Scrub(in); got != in {
			t.Fatalf("quoted-key widening reached a bare-key newline before colon:\n in: %q\n out: %q", in, got)
		}
	})
}

// TestScrubCredentialKeyRedactsRepeatedJSONLineBreaks locks the arbitrary-
// JSON-whitespace half of the JSON-form path. JSON permits insignificant
// whitespace around `:` with no bound, so a machine-serialized object can
// place a BLANK line — more than one newline — before the colon
// (`{"password"\n\n:`) or after it (`{"password":\n\n`), and a standalone
// carriage return (`{"password":\r`) is legal JSON whitespace too. The
// JSON form of credentialKeyPattern accepts `(?:\r\n?|\n)` as a line break —
// CR, CRLF, or LF, one or many, on BOTH sides of `:` — so all of these
// redact while the introducing whitespace stays in the separator (the line
// structure survives; only the quoted value is replaced). The bare-log-line
// guard the cross-newline fix exists for is untouched: a bare key keeps the
// loose form's narrow `[ \t]*[:=]` separator, and the JSON-form whitespace
// widening applies only to a symmetrically quoted key followed by `:` — so
// a quoted key with `=` across lines (`"password"\n=\n"…"`) and a bare key
// followed across a BLANK line by a column-0 quoted value
// (`token:\n\n"…"`) are both preserved, while the single-linebreak column-0
// quote after any key (`token:\n"…"`) still redacts through keyValueSecret's
// single-linebreak value alternative.
func TestScrubCredentialKeyRedactsRepeatedJSONLineBreaks(t *testing.T) {
	cases := []struct{ name, key, in, want, leak string }{
		// The shape the report names: a BLANK line (two newlines) between the
		// quoted key and `:`, and one newline after `:` before the value. The
		// blank line and the value-introducing newline survive; only the quoted
		// value is replaced.
		{`{"password"\n\n:\n"value"}`, `"password"`,
			"{\"password\"\n\n:\n\"hunter2secret\"}",
			"{\"password\"\n\n:\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		{`{"password"\n\n:\n"value"} (CRLF)`, `"password"`,
			"{\"password\"\r\n\r\n:\r\n\"hunter2secret\"}",
			"{\"password\"\r\n\r\n:\r\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Multiple newlines AFTER the colon, before the unindented quoted value.
		{`{"password":\n\n"value"}`, `"password"`,
			"{\"password\":\n\n\"hunter2secret\"}",
			"{\"password\":\n\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Multiple newlines on BOTH sides of the colon.
		{`{"password"\n\n:\n\n"value"}`, `"password"`,
			"{\"password\"\n\n:\n\n\"hunter2secret\"}",
			"{\"password\"\n\n:\n\n\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Blank line before the colon, value on the SAME line as the colon.
		{`{"password"\n\n: "value"}`, `"password"`,
			"{\"password\"\n\n: \"hunter2secret\"}",
			"{\"password\"\n\n: \"" + SecretMarker + "\"}",
			"hunter2secret"},
		// An auth keyword, quoted key, blank line before colon, next-line value.
		{`{"auth"\n\n:\n"value"}`, `"auth"`,
			"{\"auth\"\n\n:\n\"abcdef123456\"}",
			"{\"auth\"\n\n:\n\"" + SecretMarker + "\"}",
			"abcdef123456"},
		// Standalone carriage return as JSON whitespace AFTER the colon. Go's
		// encoding/json accepts a bare `\r`; the JSON-form separator matches it.
		{`{"password":\r"value"}`, `"password"`,
			"{\"password\":\r\"hunter2secret\"}",
			"{\"password\":\r\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Standalone carriage return as JSON whitespace BEFORE the colon.
		{`{"password"\r:"value"}`, `"password"`,
			"{\"password\"\r:\"hunter2secret\"}",
			"{\"password\"\r:\"" + SecretMarker + "\"}",
			"hunter2secret"},
		// Mixed CR and LF whitespace around the colon (CRLF + standalone CR).
		{`{"password"\r\n:\r"value"}`, `"password"`,
			"{\"password\"\r\n:\r\"hunter2secret\"}",
			"{\"password\"\r\n:\r\"" + SecretMarker + "\"}",
			"hunter2secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("repeated-line-break JSON value survived:\n in: %q\n want: %q\n out: %q", tc.in, tc.want, got)
			}
			if got != tc.want {
				t.Fatalf("redaction did not preserve the JSON line structure:\n in: %q\n want: %q\n out: %q", tc.in, tc.want, got)
			}
			if !strings.Contains(got, tc.key) {
				t.Fatalf("key half absorbed:\n in: %q\n out: %q", tc.in, got)
			}
		})
	}

	// No-regression half: a BARE key with multiple newlines before the colon
	// is still the ambiguous-log-line shape the cross-newline guard exists
	// for; the JSON-form widening must not reach it, because a bare key has
	// no surrounding quotes and the loose form's separator stays `[ \t]*[:=]`.
	t.Run("bare key with blank line before colon is not matched", func(t *testing.T) {
		in := "password\n\n:hunter2secret"
		if got := Scrub(in); got != in {
			t.Fatalf("JSON-form widening reached a bare-key blank line before colon:\n in: %q\n out: %q", in, got)
		}
	})

	// No-regression half: a column-0 BARE next line after a bare key is the
	// unrelated-line shape the cross-line guard exists for; the loose form
	// cannot cross to the left margin, the bare value alternative stops at
	// the newline, and the single-linebreak value alternative requires a `"` —
	// so a bare SHA on the next line survives.
	t.Run("left-margin bare next line is not a JSON value", func(t *testing.T) {
		in := "token:\n\n4f2a9c1e8b7d6c5a4f3e2d1c0b9a8f7e6d5c4b3a fix-login"
		if got := Scrub(in); got != in {
			t.Fatalf("value alternative reached a left-margin bare line:\n in: %q\n out: %q", in, got)
		}
	})

	// No-regression half: a bare key followed across a BLANK line by a
	// column-0 QUOTED string is NOT redacted. The single-linebreak value
	// alternative crosses exactly one line break, so the blank line breaks
	// the key/value association; the unrelated quoted message survives
	// (the over-redaction across a blank line the narrowing is for).
	t.Run("bare key + blank line + column-0 quoted survives", func(t *testing.T) {
		in := "token:\n\n\"build failed: see log\""
		if got := Scrub(in); got != in {
			t.Fatalf("value alternative crossed a blank line to a column-0 quote:\n in: %q\n out: %q", in, got)
		}
	})

	// Counterpart half: a bare key + a SINGLE line break + a column-0 quoted
	// value IS redacted (the JSON/YAML/log shape a serializer or pasted config
	// produces — locked end-to-end by bugreport's
	// TestScrubAndScrubLogRedactUnindentedQuotedJSON). The single-linebreak
	// alternative fires; the blank-line half above is what distinguishes a
	// continuation from an unrelated record.
	t.Run("bare key + single line break + column-0 quoted redacts", func(t *testing.T) {
		in := "set token:\n\"abcdefghijkl1234\""
		got := Scrub(in)
		if strings.Contains(got, "abcdefghijkl1234") {
			t.Fatalf("single-linebreak column-0 quoted value survived:\n in: %q\n out: %q", in, got)
		}
		if !strings.Contains(got, "set token:") {
			t.Fatalf("key half absorbed:\n in: %q\n out: %q", in, got)
		}
	})

	// No-regression half: a quoted key with `=` across lines is not JSON
	// (JSON's only key/value delimiter is `:`), so the JSON-form whitespace
	// widening does not apply and the unrelated quoted diagnostic on the next
	// line survives. Same-line quoted `=` assignments still redact via the
	// loose form's `[:=]`.
	t.Run("quoted key with = across lines is not matched", func(t *testing.T) {
		in := "\"password\"\n=\n\"build failed\""
		if got := Scrub(in); got != in {
			t.Fatalf("JSON-form widening reached a quoted-key = across lines:\n in: %q\n out: %q", in, got)
		}
	})

	// No-regression half: a SAME-LINE quoted `=` assignment still redacts
	// (the loose form keeps `[:=]`, so `=` is a valid same-line delimiter
	// for an optional-quote key).
	t.Run("same-line quoted = assignment still redacts", func(t *testing.T) {
		in := `"api_key" = "hunter2secret"`
		got := Scrub(in)
		if strings.Contains(got, "hunter2secret") {
			t.Fatalf("same-line quoted = assignment survived:\n in: %q\n out: %q", in, got)
		}
		if !strings.Contains(got, SecretMarker) {
			t.Fatalf("expected a redaction marker for the same-line quoted = assignment:\n in: %q\n out: %q", in, got)
		}
	})
}

// TestScrubCredentialKeyPreservesJSONQuotesAcrossNewline locks the line
// structure of the column-0 JSON value. The introducing line break stays in
// the separator/prefix (the JSON form's separator consumes it, or the
// single-linebreak value alternative wraps it out of the inner capture), so
// only the quoted value is replaced — invalid `{"password":[redacted-secret]}`
// is not produced; the output stays `{"password":\n"[redacted-secret]"`. If
// the newline were captured as part of the value, the quoted-value branch in
// appendKeyValueSpans (which keys on `value[0] == '"'`) would miss, leaving a
// bare marker in place of the JSON structure.
func TestScrubCredentialKeyPreservesJSONQuotesAcrossNewline(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// The column-0 JSON shape `{"password":\n"hunter2secret"}` (LF): the
		// introducing newline AND both surrounding quotes survive.
		{"LF", "{\"password\":\n\"hunter2secret\"}",
			"{\"password\":\n\"" + SecretMarker + "\"}"},
		// CRLF form: the line ending between `:` and the value is preserved.
		{"CRLF", "{\"password\":\r\n\"hunter2secret\"}",
			"{\"password\":\r\n\"" + SecretMarker + "\"}"},
		// Trailing JSON structure and following text survive verbatim.
		{"LF with trailing brace", "{\"password\":\n\"hunter2secret\"} more",
			"{\"password\":\n\"" + SecretMarker + "\"} more"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.in)
			if got != tc.want {
				t.Fatalf("JSON quotes / line structure not preserved:\n in: %q\n want: %q\n out: %q", tc.in, tc.want, got)
			}
		})
	}
}

// BenchmarkScrubTypicalLogLine measures the cost added to every log write. The
// patterns run on the write path, so a regression here is paid by the daemon on
// every line it emits.
func BenchmarkScrubTypicalLogLine(b *testing.B) {
	line := "ERROR:2026/08/05 11:15:52 worktree_ops.go:529: failed to remove worktree /home/u/.agent-factory/worktrees/af_0f8fc14c_fix-login: exit status 128"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scrub(line)
	}
}

// BenchmarkScrubTypicalLogLineFlat is the pre-#4149 implementation — the flat
// matcher alone, no normalization stage — kept alongside the benchmark above
// so the stage's cost on a line carrying no encoding is measured, not assumed.
func BenchmarkScrubTypicalLogLineFlat(b *testing.B) {
	line := "ERROR:2026/08/05 11:15:52 worktree_ops.go:529: failed to remove worktree /home/u/.agent-factory/worktrees/af_0f8fc14c_fix-login: exit status 128"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = redactspan.Apply(line, Redactions(line), SecretMarker)
	}
}

// BenchmarkScrubEncodedLogLine measures the stage on a line that does carry
// encodings — a %q field and a URI — where the trigger gates open and the
// decoders actually run.
func BenchmarkScrubEncodedLogLine(b *testing.B) {
	line := `ERROR:2026/08/05 11:15:52 hooks.go:88: post-worktree hook "fetch https://user@example.com/path?q=1" exited 1`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scrub(line)
	}
}
