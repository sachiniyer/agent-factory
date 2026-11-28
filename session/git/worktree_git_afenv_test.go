package git

import (
	"slices"
	"testing"
)

// #7891 R1/N2: when the docker runtime marks the session (AF_GIT_CONFIG set
// to the af-owned file), the agent-server's own git — the setup fetch, the
// archive push, worktree_push — reads only config files af created, and
// admits a trust-family name ONLY when the operator forwarded it with a
// host value (the AF_GIT_FORWARDED set). A claimed-but-hostless passthrough
// name would otherwise deliver the image's own value — GIT_SSL_NO_VERIFY=1
// measured against the archive push — so it is dropped outright. Without
// the marker the environment passes through byte-for-byte.
func TestAfGitScopedEnvironmentLocksDownWhenMarked(t *testing.T) {
	in := []string{
		"HOME=/af-home",
		"GIT_CONFIG_GLOBAL=/image/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/image-gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"XDG_CONFIG_HOME=/image/xdg",
		"GIT_SSL_NO_VERIFY=1", // image value, admitted by a hostless claim
		"GIT_CONFIG_COUNT=2",  // image's env-config count
		"GIT_CONFIG_KEY_0=http.sslVerify",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_SSL_CAPATH=/operator/certs.d", // forwarded WITH a value — the operator's claim
	}
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_SSL_CAPATH")
	got := afGitScopedEnvironment(in)

	// Every unforwarded trust name is dropped: the image's config roots,
	// the image's verification-off flag, and the image's env-config pairs.
	for _, name := range []string{
		"GIT_CONFIG_GLOBAL=/image/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/image-gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"XDG_CONFIG_HOME=/image/xdg",
		"GIT_SSL_NO_VERIFY=1",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.sslVerify",
		"GIT_CONFIG_VALUE_0=false",
	} {
		if slices.Contains(got, name) {
			t.Fatalf("an unforwarded trust name must not reach af's git (N2): %v", got)
		}
	}
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/af-home/.af-gitconfig",
		"GIT_SSL_CAPATH=/operator/certs.d",
		"HOME=/af-home",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("af's git env must keep %q: %v", want, got)
		}
	}
}

// #7891 N1: the per-slot rule — a forwarded route name exempts only its own
// slot. GIT_CONFIG_GLOBAL claimed keeps the operator's file while the
// system slot stays af's; GIT_CONFIG_SYSTEM claimed keeps the file and is
// not defeated by an af-asserted NOSYSTEM=1.
func TestAfGitScopedEnvironmentHonoursClaimedSlots(t *testing.T) {
	in := []string{
		"GIT_CONFIG_GLOBAL=/operator/global.gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"XDG_CONFIG_HOME=/image/xdg",
	}
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_GLOBAL")
	got := afGitScopedEnvironment(in)
	for _, want := range []string{
		"GIT_CONFIG_GLOBAL=/operator/global.gitconfig", // the claimed slot
		"GIT_CONFIG_NOSYSTEM=1",                        // system slot stays af's
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("the claimed global slot must keep the operator's file, unclaimed slots stay af's — missing %q: %v", want, got)
		}
	}
	for _, unwanted := range []string{
		"GIT_CONFIG_GLOBAL=/af-home/.af-gitconfig",
		"XDG_CONFIG_HOME=/image/xdg",
	} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("unclaimed slots must take af's roots, claimed ones the operator's: %v", got)
		}
	}

	// A claimed system file must not be defeated by an af NOSYSTEM=1 — and
	// the ambient GIT_CONFIG_NOSYSTEM in the environ is dropped (it is not
	// forwarded), so the operator's file is what git's system slot reads.
	in = []string{
		"GIT_CONFIG_SYSTEM=/operator/gitconfig",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_SYSTEM")
	got = afGitScopedEnvironment(in)
	if !slices.Contains(got, "GIT_CONFIG_SYSTEM=/operator/gitconfig") {
		t.Fatalf("the claimed system slot must keep the operator's file: %v", got)
	}
	for _, unwanted := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=0"} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("an unclaimed NOSYSTEM must not defeat the claimed system file: %v", got)
		}
	}
	if !slices.Contains(got, "GIT_CONFIG_GLOBAL=/af-home/.af-gitconfig") {
		t.Fatalf("the unclaimed global slot still takes af's file: %v", got)
	}

	// A claimed XDG_CONFIG_HOME with the global slot unclaimed: af's git
	// must read the claimed file — GIT_CONFIG_GLOBAL replaces the XDG
	// global, so pointing it at af's own file would silently shadow the
	// operator's claim. The redirect delivers it instead (Codex on N1).
	in = []string{"XDG_CONFIG_HOME=/operator/xdg"}
	t.Setenv(afGitForwardedEnvName, "XDG_CONFIG_HOME")
	got = afGitScopedEnvironment(in)
	if !slices.Contains(got, "GIT_CONFIG_GLOBAL=/operator/xdg/git/config") {
		t.Fatalf("an XDG-only claim must redirect af's global at the claimed XDG file: %v", got)
	}
	for _, unwanted := range []string{"GIT_CONFIG_GLOBAL=/af-home/.af-gitconfig", "GIT_CONFIG_GLOBAL=/image/.gitconfig"} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("the claimed XDG slot must not be shadowed: %v", got)
		}
	}
}

// Codex on #7891: af-blanked `NAME=` entries (the account loader's
// empty value for a hostless pair claim) reach AF_GIT_FORWARDED as bare
// names, so a forwarded GIT_CONFIG_COUNT + KEY_0 + af-blanked VALUE_0=
// stays a complete, legal pair — the empty value is admitted, never an
// image value. But a count whose required pairs genuinely did not
// arrive is neutralized after the filter, matching the container
// boundary's GIT_CONFIG_COUNT= pin.
func TestAfGitScopedEnvironmentPreservesAfBlankedPairValues(t *testing.T) {
	in := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=", // af-blanked empty value for the hostless claim
	}
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0,GIT_CONFIG_VALUE_0")
	got := afGitScopedEnvironment(in)
	for _, want := range in {
		if !slices.Contains(got, want) {
			t.Fatalf("a complete forwarded config pair must survive — missing %q: %v", want, got)
		}
	}
	for _, name := range got {
		if name == "GIT_CONFIG_COUNT=" {
			t.Fatalf("the complete pair must not be neutralized: %v", got)
		}
	}

	// A count whose pair never arrived (hostless, no loader blank) is
	// pinned empty — a live count over a missing pair is a fatal git
	// error, not a claim.
	in = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.sslVerify"}
	t.Setenv(afGitForwardedEnvName, "GIT_CONFIG_COUNT,GIT_CONFIG_KEY_0")
	got = afGitScopedEnvironment(in)
	if !slices.Contains(got, "GIT_CONFIG_COUNT=") {
		t.Fatalf("an incomplete forwarded count must be neutralized: %v", got)
	}
}

func TestAfGitScopedEnvironmentUnmarkedIsIdentity(t *testing.T) {
	in := []string{"GIT_CONFIG_GLOBAL=/image/.gitconfig", "XDG_CONFIG_HOME=/x"}
	if got := afGitScopedEnvironment(in); !slices.Equal(got, in) {
		t.Fatalf("without the marker env the environment must pass through unchanged: %v", got)
	}
}
