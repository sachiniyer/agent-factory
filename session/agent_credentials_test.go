package session

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestAgentCredentialMounts_OnlyTheGivenAgent is the core P1-b contract: the
// helper mounts ONLY the named agent's credential file(s), never another
// agent's — even when every agent's file exists on disk. A codex session must
// not receive the Claude token.
func TestAgentCredentialMounts_OnlyTheGivenAgent(t *testing.T) {
	home := "/home/tester"
	allExist := func(string) bool { return true }

	codex := agentCredentialMounts(tmux.ProgramCodex, home, true, allExist)
	if want := []string{"-v", "/home/tester/.codex/auth.json:/root/.codex/auth.json:ro,z"}; !reflect.DeepEqual(codex, want) {
		t.Fatalf("codex session mounts:\n got %#v\nwant %#v", codex, want)
	}
	// The Claude credential must never appear for a codex session.
	for _, a := range codex {
		if a == "/home/tester/.claude/.credentials.json:/root/.claude/.credentials.json:ro,z" {
			t.Fatal("codex session was given the Claude credential")
		}
	}

	claude := agentCredentialMounts(tmux.ProgramClaude, home, true, allExist)
	if want := []string{"-v", "/home/tester/.claude/.credentials.json:/root/.claude/.credentials.json:ro,z"}; !reflect.DeepEqual(claude, want) {
		t.Fatalf("claude session mounts:\n got %#v\nwant %#v", claude, want)
	}
	// P2: ~/.claude.json is NOT a credential and must not be mounted.
	for _, a := range claude {
		if a == "/home/tester/.claude.json:/root/.claude.json:ro,z" {
			t.Fatal(".claude.json must not be mounted (it is the config/privacy blob, not a credential)")
		}
	}
}

// TestAgentCredentialMounts_OnlyExisting: for an agent with several candidate
// filenames (gemini, whose OAuth file name has varied), only the ones present on
// disk are mounted — never an empty -v source docker would auto-create.
func TestAgentCredentialMounts_OnlyExisting(t *testing.T) {
	home := "/home/tester"
	present := map[string]bool{
		filepath.Join(home, ".gemini/gemini-credentials.json"): true,
		// oauth_creds.json and google_accounts.json absent.
	}
	got := agentCredentialMounts(tmux.ProgramGemini, home, true, func(p string) bool { return present[p] })
	want := []string{"-v", "/home/tester/.gemini/gemini-credentials.json:/root/.gemini/gemini-credentials.json:ro,z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini mounts (only existing):\n got %#v\nwant %#v", got, want)
	}
}

// TestAgentCredentialMounts_None covers the "mount nothing" paths: an unknown
// agent, no candidate present, and an unresolved (empty) home. All return nil.
func TestAgentCredentialMounts_None(t *testing.T) {
	if got := agentCredentialMounts("aider", "/home/x", true, func(string) bool { return true }); got != nil {
		t.Errorf("aider (env-only, no file): want nil, got %#v", got)
	}
	if got := agentCredentialMounts(tmux.ProgramCodex, "/home/x", true, func(string) bool { return false }); got != nil {
		t.Errorf("no files present: want nil, got %#v", got)
	}
	if got := agentCredentialMounts(tmux.ProgramCodex, "", true, func(string) bool { return true }); got != nil {
		t.Errorf("empty home: want nil, got %#v", got)
	}
}

// TestAgentCredentialMounts_AllReadOnlyUnderContainerHome guards the invariants
// that hold across every agent and any future candidate: each mount is read-only
// and SELinux-relabeled (:ro,z), the host path is absolute, and the container target is under
// dockerContainerHome (so it lands where the container's clean-env agent looks).
func TestAgentCredentialMounts_AllReadOnlyUnderContainerHome(t *testing.T) {
	home := "/home/tester"
	// Both modes: the read-only + absolute-host + under-container-home invariants
	// hold whether or not this host earned the SELinux relabel.
	for _, relabel := range []bool{false, true} {
		wantMode := dockerCredentialMountMode
		if relabel {
			wantMode = dockerCredentialMountModeRelabeled
		}
		for agent := range agentCredentialFiles {
			got := agentCredentialMounts(agent, home, relabel, func(string) bool { return true })
			for i := 0; i < len(got); i += 2 {
				if got[i] != "-v" {
					t.Fatalf("relabel=%v agent %q arg %d = %q, want -v", relabel, agent, i, got[i])
				}
				spec := got[i+1]
				if want := ":" + wantMode; spec[len(spec)-len(want):] != want {
					t.Errorf("relabel=%v agent %q mount %q does not end in the expected mode %q", relabel, agent, spec, wantMode)
				}
				if !strings.HasPrefix(wantMode, "ro") {
					t.Fatalf("mode %q is not read-only", wantMode)
				}
				host, container := splitMountSpec(t, spec, wantMode)
				if !filepath.IsAbs(host) {
					t.Errorf("relabel=%v agent %q host path %q is not absolute", relabel, agent, host)
				}
				if want := dockerContainerHome + "/"; container[:len(want)] != want {
					t.Errorf("relabel=%v agent %q container target %q is not under %q", relabel, agent, container, dockerContainerHome)
				}
			}
		}
	}
}

// splitMountSpec parses a host:container:<mode> bind spec. Host and container
// are absolute unix paths (no colons), so splitting on the last two colons is
// exact.
func splitMountSpec(t *testing.T, spec, mode string) (host, container string) {
	t.Helper()
	body := spec[:len(spec)-len(":"+mode)]
	sep := -1
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == ':' {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("malformed mount spec %q", spec)
	}
	return body[:sep], body[sep+1:]
}

// TestAgentCredentialMounts_CarrySELinuxRelabel is the #3451 regression: every
// credential mount must carry the SHARED SELinux relabel as well as :ro.
//
// Without it the mount is not a broken mount but a DENIED read. On an
// SELinux-enforcing host — the Fedora/RHEL/CentOS default — the host file keeps
// its user_home_t label, container policy refuses the open, and because this
// path is deliberately fail-open the session starts UNAUTHENTICATED with no
// error at all. That is the worst shape a failure can take here, so the relabel
// is pinned per-agent rather than only in aggregate.
func TestAgentCredentialMounts_CarrySELinuxRelabel(t *testing.T) {
	home := "/home/tester"
	for _, tt := range []struct {
		relabel bool
		mode    string
	}{
		{relabel: true, mode: "ro,z"},
		// Gated OFF, the mount must fall back to plain :ro and must NOT keep a
		// stray comma or label — an over-eager mode string would relabel the
		// operator's credential file on a host that never asked for it.
		{relabel: false, mode: "ro"},
	} {
		for agent, rels := range agentCredentialFiles {
			got := agentCredentialMounts(agent, home, tt.relabel, func(string) bool { return true })
			want := make([]string, 0, len(rels)*2)
			for _, rel := range rels {
				want = append(want, "-v", home+"/"+rel+":"+dockerContainerHome+"/"+rel+":"+tt.mode)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("relabel=%v agent %q mounts:\n got %#v\nwant %#v", tt.relabel, agent, got, want)
			}
		}
	}
}

// TestAgentCredentialMounts_RelabelIsSharedNotPrivate pins z over Z, which is a
// correctness choice and not a style one. Z assigns an SVirt category pair
// unique to ONE container, so the second concurrent session mounting the same
// credential file would relabel it out from under the first. Every af session
// running the same agent shares that one host file, so the label has to be the
// shared one — the same reason dockerAccountMount picks z.
func TestAgentCredentialMounts_RelabelIsSharedNotPrivate(t *testing.T) {
	got := agentCredentialMounts(tmux.ProgramCodex, "/home/tester", true, func(string) bool { return true })
	for i := 1; i < len(got); i += 2 {
		mode := got[i][strings.LastIndex(got[i], ":")+1:]
		if strings.Contains(mode, "Z") {
			t.Errorf("mount %q uses the PRIVATE relabel Z; concurrent sessions share this host file, so it must be z", got[i])
		}
		if !strings.Contains(mode, "z") {
			t.Errorf("mount %q carries no SELinux relabel; it is unreadable on an SELinux-enforcing host", got[i])
		}
		if !strings.Contains(mode, "ro") {
			t.Errorf("mount %q is not read-only", got[i])
		}
	}
}

// TestAgentCredentialMounts_PassTheAccountRunArgsGuard checks the relabel does
// not disturb the account boundary hardened in #3400/#3401/#3402. The mode now
// contains a COMMA, which is exactly the delimiter that guard's CSV reader
// splits a --mount value on, so both directions are worth pinning:
//
//   - a real credential mount spec is still accepted (no false refusal), and
//   - a spec aimed at a protected path is still REFUSED even when it hides
//     behind the same ",z" mode (no new bypass).
func TestAgentCredentialMounts_PassTheAccountRunArgsGuard(t *testing.T) {
	for _, relabel := range []bool{false, true} {
		for agent := range agentCredentialFiles {
			mounts := agentCredentialMounts(agent, "/home/tester", relabel, func(string) bool { return true })
			if err := validateAccountDockerRunArgs(mounts, "codex"); err != nil {
				t.Errorf("relabel=%v agent %q credential mounts were refused by the account guard: %v\nargs: %#v", relabel, agent, err, mounts)
			}
		}
	}
	for _, args := range [][]string{
		{"-v", "/evil:" + dockerAccountHome + ":ro,z"},
		{"-v", "/evil:" + dockerAccountHome + "/.config:ro,z"},
		{"-v", "/evil:" + dockerAccountRuntimeHome + ":ro,z"},
		{"-tv", "/evil:" + dockerAccountHome + ":ro,z"},
		{"--mount", "type=bind,src=/evil,dst=" + dockerAccountHome + ",readonly"},
	} {
		if err := validateAccountDockerRunArgs(args, "codex"); err == nil {
			t.Errorf("a protected-path mount was ACCEPTED behind a relabel mode: %#v", args)
		}
	}
}

// TestSelinuxRelabelForHost_FollowsTheEnforceFile pins the gate itself: the
// relabel is applied on an SELinux-ENFORCING host and omitted elsewhere.
//
// Permissive (enforce=0) deliberately reads as "no relabel": SELinux logs the
// denial there rather than acting on it, so the credential reads fine unlabeled
// and the operator's host file is left untouched.
func TestSelinuxRelabelForHost_FollowsTheEnforceFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxEnforcing short-circuits to false off linux")
	}
	for _, tt := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "enforcing", content: "1\n", want: true},
		{name: "permissive", content: "0\n", want: false},
		{name: "enforcing without a trailing newline", content: "1", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			enforce := filepath.Join(dir, "enforce")
			if err := os.WriteFile(enforce, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			setSELinuxEnforcePaths(t, enforce)
			if got := selinuxRelabelForHost(); got != tt.want {
				t.Errorf("selinuxRelabelForHost() with enforce=%q = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestSelinuxRelabelForHost_AbsentSELinuxDoesNotRelabel covers the ordinary
// non-SELinux host — no enforce file anywhere, no relabel, and no error.
func TestSelinuxRelabelForHost_AbsentSELinuxDoesNotRelabel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxEnforcing short-circuits to false off linux")
	}
	setSELinuxEnforcePaths(t, filepath.Join(t.TempDir(), "absent"))
	if selinuxRelabelForHost() {
		t.Error("a host with no SELinux enforce file must not relabel")
	}
}

// TestSelinuxRelabelForHost_ProbeFailureStillRelabels is the load-bearing one,
// and the reason the gate goes through selinuxRelabelForHost rather than
// calling hostSELinuxEnforcing inline.
//
// The two failure directions are NOT symmetric. Applying z where SELinux is off
// is a no-op — Docker ignores the flag — while omitting it on an enforcing host
// reinstates #3451 exactly: a credential the container cannot read, on a path
// that is deliberately fail-open, so the session starts silently
// unauthenticated. An unreadable or nonsense /sys must therefore resolve to
// "relabel", never to "skip it".
func TestSelinuxRelabelForHost_ProbeFailureStillRelabels(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxEnforcing short-circuits to false off linux")
	}
	t.Run("unreadable enforce path", func(t *testing.T) {
		// A directory where a file is expected: ReadFile fails with something
		// that is not os.ErrNotExist, so the probe reports an error.
		dir := t.TempDir()
		enforce := filepath.Join(dir, "enforce")
		if err := os.Mkdir(enforce, 0o755); err != nil {
			t.Fatal(err)
		}
		setSELinuxEnforcePaths(t, enforce)
		if _, err := hostSELinuxEnforcing(); err == nil {
			t.Fatal("expected the probe to fail on a directory; the test fixture proves nothing otherwise")
		}
		if !selinuxRelabelForHost() {
			t.Error("a FAILED SELinux probe dropped the relabel; on an enforcing host that is #3451 all over again")
		}
	})
	t.Run("unexpected enforce value", func(t *testing.T) {
		dir := t.TempDir()
		enforce := filepath.Join(dir, "enforce")
		if err := os.WriteFile(enforce, []byte("banana\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		setSELinuxEnforcePaths(t, enforce)
		if _, err := hostSELinuxEnforcing(); err == nil {
			t.Fatal("expected the probe to reject a nonsense value")
		}
		if !selinuxRelabelForHost() {
			t.Error("an unreadable SELinux mode dropped the relabel; it must fail toward relabeling")
		}
	})
}

// setSELinuxEnforcePaths points the probe at fixtures for one test and restores
// the real kernel paths afterwards.
func setSELinuxEnforcePaths(t *testing.T, paths ...string) {
	t.Helper()
	original := selinuxEnforcePaths
	selinuxEnforcePaths = paths
	t.Cleanup(func() { selinuxEnforcePaths = original })
}

// forceCredentialMountMode points the SELinux probe at an "enforcing" fixture so
// the argv tests pin the RELABELED mode actually reaching `docker run`, rather
// than whatever mode this particular build host happens to earn — a green run on
// a non-SELinux box would otherwise prove nothing about the relabel. It returns
// the mode to expect: on non-linux the probe short-circuits to false whatever
// the fixture says, so the plain read-only mode is correct there.
func forceCredentialMountMode(t *testing.T) string {
	t.Helper()
	enforce := filepath.Join(t.TempDir(), "enforce")
	if err := os.WriteFile(enforce, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setSELinuxEnforcePaths(t, enforce)
	if selinuxRelabelForHost() {
		return dockerCredentialMountModeRelabeled
	}
	return dockerCredentialMountMode
}
