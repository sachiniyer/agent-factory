package credscrub

import (
	"strings"
	"testing"
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
