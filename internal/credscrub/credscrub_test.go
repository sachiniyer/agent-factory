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
