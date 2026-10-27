package redactx

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// TestUserinfoMalformedFailCloseAndRecovery pins the fail-close contract for a
// malformed percent escape in URI userinfo — the credential carrier
// (scheme://user:pass@host) that the sibling fail-close fix in a754a7ac missed.
// url.Parse rejects the whole URI when the userinfo carries a malformed escape,
// so the url.Parse-error arm is the live path; the userinfo-extraction block
// (gated on a successful parse) is unreachable. The error arm previously
// fail-closed only the path and skipped the userinfo, so the recoverable
// percent-encoded surface shipped verbatim. The fix fail-closes the userinfo,
// gated on its own bytes containing a malformed %, mirroring the path gate and
// preserving nested-URI recovery for non-malformed parse failures.
func TestUserinfoMalformedFailCloseAndRecovery(t *testing.T) {
	const sentinel = "ZZSECRETZZ"
	const marker = "[s]"
	// rawSpelling is the recoverable percent-encoded surface of the secret:
	// one byte ('S') is escaped (%53) so the plaintext sentinel does not
	// appear in the raw text. A reader URL-decoding the output would still
	// recover the sentinel from this surface unless the range fails closed.
	const rawSpelling = "ZZ%53ECRETZZ"

	e := &Engine{
		Produce: func(text string, _ Provenance) []redactspan.Span {
			if i := strings.Index(text, sentinel); i >= 0 {
				return []redactspan.Span{{Start: i, End: i + len(sentinel), Replacement: marker, Priority: 1}}
			}
			return nil
		},
		FailClosed: redactspan.Span{Replacement: "[f]", Priority: 2},
		Fallback:   "[f]",
	}

	// Malformed userinfo: the whole userinfo region (including the recoverable
	// surface and the malformed tail) fails closed. The sentinel, its
	// percent-encoded surface, and the malformed tail must all be absent;
	// the fail-close marker must appear.
	t.Run("scrub-failcloses-malformed-userinfo", func(t *testing.T) {
		in := "http://" + rawSpelling + "%2g:pass@host/p"
		got := e.Scrub(in, ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed userinfo shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed userinfo not fail-closed: %q", got)
		}
	})

	// Direct contract assertion: sourceMappedURIComponents must NOT admit a
	// userinfo view for the malformed input and must route the userinfo
	// source range into unknown.
	t.Run("transform-failcloses-malformed-userinfo", func(t *testing.T) {
		in := "http://" + rawSpelling + "%2g:pass@host/p"
		views, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		if viewHas(views, ProvURIComponent, "ZZSECRETZZ%2g") {
			t.Fatalf("malformed userinfo admitted a view (must fail-close): %+v", views)
		}
		lo := strings.Index(in, rawSpelling)
		if !covers(unknown, lo, lo+len(rawSpelling)+len("%2g")) {
			t.Fatalf("malformed userinfo range not fail-closed: unknown=%d", len(unknown))
		}
	})

	// Malformed userinfo WITHOUT a path: the path fail-close has nothing to
	// cover, so the userinfo fail-close is the only redaction. Confirms the
	// error-arm fix fires on the userinfo independently of the path.
	t.Run("scrub-failcloses-malformed-userinfo-no-path", func(t *testing.T) {
		in := "http://" + rawSpelling + "%2g:pass@host"
		got := e.Scrub(in, ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed userinfo (no path) shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed userinfo (no path) not fail-closed: %q", got)
		}
	})

	// Well-formed userinfo: url.Parse succeeds, the userinfo block decodes a
	// view containing the sentinel, the producer redacts it. This proves
	// userinfo is an intended fail-closed credential carrier exactly like
	// the query, fragment, and opaque components; the malformed case is the
	// oversight, not an intentional pass-through.
	t.Run("wellformed-userinfo-redacts", func(t *testing.T) {
		got := e.Scrub("http://"+rawSpelling+":pass@host/p", ProvLogRecord)
		want := "http://" + marker + ":pass@host/p"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	// Recovery control: a url.Parse failure from a bad authority bracket with
	// NO userinfo (@ absent) and clean path escapes must still recover the
	// nested file:// URI. The fix's @ check does not fire.
	t.Run("bad-authority-recovery-preserved", func(t *testing.T) {
		in := "bad://[file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent"
		views, _ := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		if len(views) == 0 {
			t.Fatalf("nested-URI recovery suppressed for non-userinfo url.Parse failure: %q", in)
		}
	})

	// Recovery control: a url.Parse failure with an @ in the authority and
	// CLEAN userinfo (cl%30 decodes to cl0, no malformed %). The fix's
	// PercentDecode on the userinfo reports !malformed, so the userinfo is
	// NOT fail-closed — no spurious redaction of clean bytes.
	t.Run("clean-userinfo-in-bad-authority-not-failclosed", func(t *testing.T) {
		in := "http://cl%30@[host/p"
		_, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		lo := strings.Index(in, "cl%30")
		if covers(unknown, lo, lo+len("cl%30")) {
			t.Fatalf("clean userinfo spuriously fail-closed: unknown=%+v", unknown)
		}
	})
}

// TestHostMalformedFailClose pins the fail-close contract for a malformed
// percent escape in the URI host (reg-name) — the one authority component
// path/query/fragment/userinfo leave unaccounted for. url.Parse rejects any
// percent-encoding in the host, so the url.Parse-error arm is the live path,
// and unlike those components the host is never extracted as a view the
// producer can match, so a percent-encoded credential sitting in the host
// ships verbatim unless its own bytes fail closed. The fix fail-closes the
// host range — the authority bytes after any userinfo, before the path —
// gated on its own bytes containing a malformed %, mirroring the path and
// userinfo gates and reserving the host for non-malformed parse failures so
// nested-URI recovery is preserved.
func TestHostMalformedFailClose(t *testing.T) {
	const sentinel = "ZZSECRETZZ"
	const marker = "[s]"
	const rawSpelling = "ZZ%53ECRETZZ"
	e := &Engine{
		Produce: func(text string, _ Provenance) []redactspan.Span {
			if i := strings.Index(text, sentinel); i >= 0 {
				return []redactspan.Span{{Start: i, End: i + len(sentinel), Replacement: marker, Priority: 1}}
			}
			return nil
		},
		FailClosed: redactspan.Span{Replacement: "[f]", Priority: 2},
		Fallback:   "[f]",
	}

	// Malformed host, no userinfo: the whole host region (recoverable
	// percent-encoded surface and the malformed tail) fails closed. The
	// sentinel, its percent-encoded surface, and the malformed tail must
	// be absent; the fail-close marker must appear.
	t.Run("scrub-failcloses-malformed-host", func(t *testing.T) {
		in := "http://" + rawSpelling + "%2g.com/p"
		got := e.Scrub(in, ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed host shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed host not fail-closed: %q", got)
		}
	})

	// Malformed host WITH clean userinfo (user:pass, no %): the userinfo is
	// not fail-closed, but the malformed host still closes the credential
	// surface, so neither the sentinel nor its percent-encoded spelling nor
	// the malformed tail ships. Confirms the host gate fires independently
	// of the userinfo gate.
	t.Run("scrub-failcloses-malformed-host-with-clean-userinfo", func(t *testing.T) {
		in := "http://user:pass@" + rawSpelling + "%2g.com/p"
		got := e.Scrub(in, ProvLogRecord)
		if strings.Contains(got, sentinel) || strings.Contains(got, rawSpelling) || strings.Contains(got, "%2g") {
			t.Fatalf("malformed host with userinfo shipped recoverable bytes: %q", got)
		}
		if !strings.Contains(got, "[f]") {
			t.Fatalf("malformed host with userinfo not fail-closed: %q", got)
		}
	})

	// Direct contract assertion: sourceMappedURIComponents must route the
	// malformed host source range into unknown (fail-closed by the engine).
	t.Run("transform-failcloses-malformed-host", func(t *testing.T) {
		in := "http://" + rawSpelling + "%2g.com/p"
		_, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		lo := strings.Index(in, rawSpelling)
		if !covers(unknown, lo, lo+len(rawSpelling)+len("%2g")) {
			t.Fatalf("malformed host range not fail-closed: unknown=%+v", unknown)
		}
	})

	// Recovery control: a url.Parse failure with an @ in the authority and
	// a CLEAN host (no percent-encoding). The fix's PercentDecode on the
	// host reports !malformed, so the host is NOT fail-closed — no spurious
	// redaction of clean bytes, and nested-URI recovery is preserved.
	t.Run("clean-host-in-bad-authority-not-failclosed", func(t *testing.T) {
		in := "http://user@[host/p"
		_, unknown := sourceMappedURIComponents(Identity(in), ProvLogRecord)
		lo := strings.Index(in, "[host")
		if covers(unknown, lo, lo+len("[host")) {
			t.Fatalf("clean host spuriously fail-closed: unknown=%+v", unknown)
		}
	})
}
