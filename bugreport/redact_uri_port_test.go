package bugreport

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// uriStartForPath is the text-level boundary predicate that decides whether a
// registered repo/worktree root appearing inside a URI (file:///srv/repo,
// vscode://host:8080/srv/repo) should be redacted by the free-text pass. It
// used strings.LastIndexByte(prefix, ':') to locate the scheme colon, which
// latched onto a port, userinfo, or IPv6 colon inside the authority and made
// the predicate reject every port-bearing URI. The fix walks back over the
// authority (which never contains "/") to the "//" marker, so the scheme
// colon is found regardless of colons in the authority. These tests pin the
// predicate directly and the full scrubber path end to end.

func TestURIStartForPathAcceptsPortBearingAuthorities(t *testing.T) {
	// start is the index of the first byte of the URI path "/srv/repo".
	const path = "/srv/repo"
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Port-free URIs the original comment documented and the bug left working.
		{name: "authority-free triple slash", in: "file:///srv/repo", want: true},
		{name: "authority no port", in: "vscode://file/srv/repo", want: true},
		{name: "authority empty host no port", in: "file://host/srv/repo", want: true},
		{name: "no-authority scheme:/path", in: "scheme:/srv/repo", want: true},
		{name: "no-authority after prose", in: "recovery: scheme:/srv/repo", want: true},
		// Port-bearing authorities the bug rejected; the scheme colon must now win.
		{name: "numeric port", in: "vscode://host:8080/srv/repo", want: true},
		{name: "numeric port file", in: "file://host:21/srv/repo", want: true},
		{name: "non-numeric port", in: "vscode://host:notaport/srv/repo", want: true},
		{name: "well-formed ipv6 port", in: "vscode://[::1]:8080/srv/repo", want: true},
		{name: "malformed ipv6 missing bracket", in: "vscode://[::1:8080/srv/repo", want: true},
		{name: "userinfo with port", in: "http://user:pass@host:8080/srv/repo", want: true},
		{name: "userinfo no port", in: "http://user@host/srv/repo", want: true},
		// A path slash already present after the authority makes the match a
		// suffix inside a longer URI path; it must never be a root boundary.
		{name: "suffix after path prefix", in: "vscode://host:8080/prefix/srv/repo", want: false},
		{name: "suffix after path prefix malformed", in: "vscode://host:notaport/prefix/srv/repo", want: false},
		{name: "four slashes path-then-authority", in: "file:////srv/repo", want: false},
		// A slash inside a query or fragment is not the URI path's first slash:
		// the authority scan must stop at "?" or "#" rather than cross it and
		// relabel fragment/query data as the repository root.
		{name: "root suffix in fragment", in: "vscode://host:8080#prefix/srv/repo", want: false},
		{name: "root suffix in fragment empty host", in: "vscode://#prefix/srv/repo", want: false},
		{name: "root suffix in query", in: "vscode://host:8080?prefix/srv/repo", want: false},
		{name: "root suffix in query no port", in: "vscode://host?prefix/srv/repo", want: false},
		// A component-leading root in a query/fragment (the delimiter is
		// immediately before the matched slash) is a real root boundary, even
		// when a malformed authority keeps url.Parse from producing a decoded
		// view: the text-level fallback must still redact it.
		{name: "component-leading root in fragment malformed authority", in: "vscode://[bad#/srv/repo", want: true},
		{name: "component-leading root in fragment empty host", in: "vscode://#/srv/repo", want: true},
		{name: "component-leading root in query malformed authority", in: "vscode://[bad?/srv/repo", want: true},
		// A direct no-authority "scheme:/path" whose colon sits at start-1 is a
		// root boundary even when an earlier "?" or "#" appears in a
		// percent-decoded view (e.g. a query value decoding to
		// "?scheme:/srv/repo"); the earlier delimiter must not disqualify it.
		{name: "direct scheme path after earlier delimiter", in: "next=?scheme:/srv/repo", want: true},
		{name: "direct scheme path after earlier fragment delimiter", in: "next=#scheme:/srv/repo", want: true},
		// Text before the scheme must not be absorbed into the scheme run.
		{name: "prose before port uri", in: "recovery failed: vscode://host:notaport/srv/repo", want: true},
		// No scheme at all.
		{name: "authority without scheme", in: "://host:8080/srv/repo", want: false},
		{name: "no colon", in: "//host/srv/repo", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := strings.Index(c.in, path)
			if start < 0 {
				t.Fatalf("fixture missing path %q: %q", path, c.in)
			}
			_, ok := uriStartForPath(c.in, start)
			if ok != c.want {
				t.Fatalf("uriStartForPath(%q, %d) = %v, want %v", c.in, start, ok, c.want)
			}
		})
	}
}

// TestURIStartForPathReturnsSchemeStart mirrors rawURIPathRange's discipline: the
// returned index is the scheme's first byte, so uriPathEndsAt's url.Parse
// slice begins at a real scheme. The scheme start must match the literal scheme
// even when the authority carries colons.
func TestURIStartForPathReturnsSchemeStart(t *testing.T) {
	cases := []struct {
		in        string
		path      string
		wantStart int
	}{
		{in: "file:///srv/repo", path: "/srv/repo", wantStart: 0},
		{in: "vscode://host:8080/srv/repo", path: "/srv/repo", wantStart: 0},
		{in: "recovery failed: vscode://host:notaport/srv/repo", path: "/srv/repo", wantStart: 17},
		{in: "scheme:/srv/repo", path: "/srv/repo", wantStart: 0},
	}
	for _, c := range cases {
		start := strings.Index(c.in, c.path)
		got, ok := uriStartForPath(c.in, start)
		if !ok {
			t.Fatalf("uriStartForPath(%q, %d) = false, want true", c.in, start)
		}
		if got != c.wantStart {
			t.Fatalf("uriStartForPath(%q, %d) schemeStart = %d, want %d", c.in, start, got, c.wantStart)
		}
	}
}

// TestScrubbersRedactRootInsideMalformedAuthorityURI is the end-to-end witness
// for the reported leak. A registered repo root embedded in a URI whose
// authority Go's url.Parse rejects (non-numeric port) shipped verbatim in
// every bundle section because the URI transform produced no decoded path view
// and the text-level boundary rejected the match. With the fix the text-level
// pass redacts the root and leaves the (non-root) authority intact.
func TestScrubbersRedactRootInsideMalformedAuthorityURI(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	const in = "recovery failed: vscode://host:notaport" + siblingLeakRepo + " could not open"
	const want = "recovery failed: vscode://host:notaport[repo:1] could not open"
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "generic", scrub: r.scrub},
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
		{name: "unstructured", scrub: r.scrubUnstructured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(in)
			if strings.Contains(got, "ConfidentialClient") {
				t.Fatalf("scrubber leaked the registered repo root inside a malformed-authority URI:\n%s", got)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

// TestScrubbersRedactRootInsideMalformedAuthorityVariants covers the other
// authority shapes url.Parse rejects (malformed IPv6) and the subpath form from
// the report's exploit scenario, for every scrubber. The path remainder below
// or after the root must survive so triage keeps the URI shape.
func TestScrubbersRedactRootInsideMalformedAuthorityVariants(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "malformed ipv6 missing bracket",
			in:   "open vscode://[::1:8080" + siblingLeakRepo + " failed",
			want: "open vscode://[::1:8080[repo:1] failed",
		},
		{
			name: "exploit subpath",
			in:   "open vscode://host:notaport" + siblingLeakRepo + "/src/main.go",
			want: "open vscode://host:notaport[repo:1]/src/main.go",
		},
	}
	for _, c := range cases {
		for _, tc := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "generic", scrub: r.scrub},
			{name: "log", scrub: r.scrubLog},
			{name: "diagnostic", scrub: r.scrubDiagnostic},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				got := tc.scrub(c.in)
				if strings.Contains(got, "ConfidentialClient") {
					t.Fatalf("scrubber leaked the registered root:\n%s", got)
				}
				if got != c.want {
					t.Errorf("scrubber output = %q, want %q", got, c.want)
				}
			})
		}
	}
}

// TestScrubbersRedactSiblingWorktreeInsideMalformedAuthorityURI covers the
// worktree-sibling layout (repo-root + "-" + title-segment) embedded in a URI
// whose authority url.Parse rejects, when the path ends at a text boundary.
// The registered sibling shape must collapse to its token + marker pair.
func TestScrubbersRedactSiblingWorktreeInsideMalformedAuthorityURI(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	const in = "lint: checking vscode://host:notaport" + siblingLeakAlternate + " could not open"
	const want = "lint: checking vscode://host:notaport[repo:1]-" + redactedMarker + " could not open"
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "generic", scrub: r.scrub},
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(in)
			if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
				t.Fatalf("scrubber leaked the sibling worktree inside a malformed-authority URI:\n%s", got)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

// TestScrubbersDoNotDoubleRedactWellFormedPortURIs pins the no-regression
// contract: well-formed port URIs were already redacted through the URI
// transform's decoded path view. The text-level pass now also matches them,
// and must produce the SAME output — the two spans cover the same source
// range with the same token, so their union is identical.
func TestScrubbersDoNotDoubleRedactWellFormedPortURIs(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "numeric port root",
			in:   "editor target vscode://host:8080" + siblingLeakRepo,
			want: "editor target vscode://host:8080[repo:1]",
		},
		{
			name: "numeric port root with query",
			in:   "editor target vscode://host:8080" + siblingLeakRepo + "?window=1",
			want: "editor target vscode://host:8080[repo:1]?window=1",
		},
		{
			name: "well-formed ipv6 port root",
			in:   "editor target vscode://[::1]:8080" + siblingLeakRepo,
			want: "editor target vscode://[::1]:8080[repo:1]",
		},
		{
			name: "userinfo port root",
			in:   "editor target http://user:pass@host:8080" + siblingLeakRepo,
			want: "editor target http://user:pass@host:8080[repo:1]",
		},
		{
			name: "numeric port sibling with fragment",
			in:   "lint: checking vscode://host:8080" + siblingLeakAlternate + "#L10",
			want: "lint: checking vscode://host:8080[repo:1]-" + redactedMarker + "#L10",
		},
	}
	for _, c := range cases {
		for _, tc := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "generic", scrub: r.scrub},
			{name: "log", scrub: r.scrubLog},
			{name: "diagnostic", scrub: r.scrubDiagnostic},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				got := tc.scrub(c.in)
				if strings.Contains(got, "ConfidentialClient") {
					t.Fatalf("scrubber leaked the root inside a well-formed port URI:\n%s", got)
				}
				if got != c.want {
					t.Errorf("scrubber output = %q, want %q", got, c.want)
				}
			})
		}
	}
}

// TestScrubbersDoNotOverRedactRootSuffixInsidePortURI pins the boundary
// invariant: a registered root that appears as a SUFFIX of a longer URI path
// (after a path separator) is not a root boundary and must not be tokenized,
// for both well-formed and malformed authorities. Over-redacting it would
// relabel an unrelated path as the registered repo.
func TestScrubbersDoNotOverRedactRootSuffixInsidePortURI(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	for _, in := range []string{
		"vscode://host:notaport/prefix" + siblingLeakRepo,
		"vscode://host:8080/prefix" + siblingLeakRepo,
	} {
		for _, tc := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "generic", scrub: r.scrub},
			{name: "log", scrub: r.scrubLog},
		} {
			t.Run(tc.name+"/"+in, func(t *testing.T) {
				got := tc.scrub(in)
				if strings.Contains(got, "[repo:1]") {
					t.Fatalf("scrubber over-redacted a non-boundary root suffix inside a port URI:\n%s", got)
				}
			})
		}
	}
}

// TestScrubbersDoNotRedactRootSuffixInsideFragmentOrQuery pins the boundary
// invariant from the Codex review of #5277: when a port-bearing URI has no
// path but its query or fragment contains the registered root as a suffix
// (e.g. "vscode://host:8080#prefix/srv/repo"), the backward authority scan must
// stop at the "?" or "#" delimiter instead of crossing it and relabeling the
// fragment/query data as the repository root.
func TestScrubbersDoNotRedactRootSuffixInsideFragmentOrQuery(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	for _, in := range []string{
		"vscode://host:8080#prefix" + siblingLeakRepo,
		"vscode://host:notaport#prefix" + siblingLeakRepo,
		"vscode://host:8080?prefix" + siblingLeakRepo,
		"vscode://host:notaport?prefix" + siblingLeakRepo,
	} {
		for _, tc := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "generic", scrub: r.scrub},
			{name: "log", scrub: r.scrubLog},
			{name: "diagnostic", scrub: r.scrubDiagnostic},
		} {
			t.Run(tc.name+"/"+in, func(t *testing.T) {
				got := tc.scrub(in)
				if strings.Contains(got, "[repo:1]") {
					t.Fatalf("scrubber redacted a fragment/query root suffix as a boundary:\n%s", got)
				}
			})
		}
	}
}

// TestScrubbersRedactComponentLeadingRootInsideFragmentOrQuery pins the Codex
// review fix on #5277: when a malformed authority keeps url.Parse from producing
// a decoded path view, a root that LEADS the fragment/query (the delimiter is
// immediately before the root, with no text prefix) is still a root boundary
// and the text-level fallback must redact it. A "#prefix/root" suffix is not a
// boundary (covered above); a "#/root" component-leading root is.
func TestScrubbersRedactComponentLeadingRootInsideFragmentOrQuery(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "malformed authority component-leading fragment",
			in:   "open vscode://[bad#" + siblingLeakRepo + " failed",
			want: "open vscode://[bad#[repo:1] failed",
		},
		{
			name: "malformed authority component-leading query",
			in:   "open vscode://[bad?" + siblingLeakRepo + " failed",
			want: "open vscode://[bad?[repo:1] failed",
		},
	}
	for _, c := range cases {
		for _, tc := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "generic", scrub: r.scrub},
			{name: "log", scrub: r.scrubLog},
			{name: "diagnostic", scrub: r.scrubDiagnostic},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				got := tc.scrub(c.in)
				if strings.Contains(got, "ConfidentialClient") {
					t.Fatalf("scrubber leaked the component-leading root inside a malformed-authority URI:\n%s", got)
				}
				if got != c.want {
					t.Errorf("scrubber output = %q, want %q", got, c.want)
				}
			})
		}
	}
}

// TestScrubLogRedactsPortURIInRealisticHookOutput simulates the concrete carrier
// from the report: a post-worktree hook's linter emits a vscode:// URI with a
// non-numeric port and the registered repo path, and the daemon logs it
// verbatim. scrubLog is the function that tail-collects agent-factory.log and
// must redact the registered directory name before it ships in the bundle.
func TestScrubLogRedactsPortURIInRealisticHookOutput(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	line := `post-worktree hook "lint.sh" failed (full output: /tmp/hook.log): exit 1` + "\n" +
		"lint: checking vscode://host:notaport" + siblingLeakRepo + "/src/main.go"
	got := r.scrubLog(line)
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("scrubLog leaked the registered repo root from hook output:\n%s", got)
	}
	if want := `post-worktree hook "lint.sh" failed (full output: /tmp/hook.log): exit 1` + "\n" +
		"lint: checking vscode://host:notaport[repo:1]/src/main.go"; got != want {
		t.Errorf("scrubLog output = %q, want %q", got, want)
	}
}
