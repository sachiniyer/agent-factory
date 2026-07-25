package session

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestAgentCredentialMounts pins the contract slice 3's committed config depends
// on: only credential files that EXIST are mounted, each as a read-only bind of
// the host path onto the same relative path under the container's /root HOME, in
// agentCredentialRelPaths order. Absent candidates (here: gemini's alt filenames,
// amp, devin) are skipped, so a container never gets an empty root-owned dir
// docker would auto-create for a missing -v source.
func TestAgentCredentialMounts(t *testing.T) {
	home := "/home/tester"
	present := map[string]bool{
		filepath.Join(home, ".claude/.credentials.json"):       true,
		filepath.Join(home, ".claude.json"):                    true,
		filepath.Join(home, ".codex/auth.json"):                true,
		filepath.Join(home, ".gemini/gemini-credentials.json"): true,
		filepath.Join(home, ".config/amp/settings.json"):       false,
		filepath.Join(home, ".local/share/opencode/auth.json"): true,
	}
	got := agentCredentialMounts(home, func(p string) bool { return present[p] })

	want := []string{
		"-v", "/home/tester/.claude/.credentials.json:/root/.claude/.credentials.json:ro",
		"-v", "/home/tester/.claude.json:/root/.claude.json:ro",
		"-v", "/home/tester/.codex/auth.json:/root/.codex/auth.json:ro",
		"-v", "/home/tester/.gemini/gemini-credentials.json:/root/.gemini/gemini-credentials.json:ro",
		"-v", "/home/tester/.local/share/opencode/auth.json:/root/.local/share/opencode/auth.json:ro",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agentCredentialMounts mismatch:\n got %#v\nwant %#v", got, want)
	}
}

// TestAgentCredentialMounts_None covers the two "mount nothing" paths: no
// candidate exists, and an unresolved (empty) home. Both must return nil rather
// than a partial or panicking result — resolveAgentCredentialMounts turns nil
// into a warning, leaving provisioning to continue (possibly unauthenticated)
// rather than aborting.
func TestAgentCredentialMounts_None(t *testing.T) {
	if got := agentCredentialMounts("/home/x", func(string) bool { return false }); got != nil {
		t.Errorf("no files present: want nil, got %#v", got)
	}
	if got := agentCredentialMounts("", func(string) bool { return true }); got != nil {
		t.Errorf("empty home: want nil, got %#v", got)
	}
}

// TestAgentCredentialMounts_AllReadOnlyUnderContainerHome guards the two
// invariants that make this safe and correct regardless of which candidates the
// list gains later: every mount is read-only (:ro), and every container target
// is under dockerContainerHome (so it lands where the container's clean-env agent
// looks). A future edit that mounts read-write, or targets a host-absolute path,
// fails here.
func TestAgentCredentialMounts_AllReadOnlyUnderContainerHome(t *testing.T) {
	home := "/home/tester"
	got := agentCredentialMounts(home, func(string) bool { return true })
	if len(got) == 0 {
		t.Fatal("expected mounts when every candidate exists")
	}
	for i := 0; i < len(got); i += 2 {
		if got[i] != "-v" {
			t.Fatalf("arg %d = %q, want -v", i, got[i])
		}
		spec := got[i+1]
		if want := ":ro"; spec[len(spec)-len(want):] != want {
			t.Errorf("mount %q is not read-only", spec)
		}
		// spec is host:container:ro — the container path is the middle field.
		host, container := splitMountSpec(t, spec)
		if !filepath.IsAbs(host) {
			t.Errorf("host path %q is not absolute", host)
		}
		if want := dockerContainerHome + "/"; container[:len(want)] != want {
			t.Errorf("container target %q is not under %q", container, dockerContainerHome)
		}
	}
}

// splitMountSpec parses a host:container:ro bind spec. Host and container are
// absolute unix paths (no colons), so splitting on the last two colons is exact.
func splitMountSpec(t *testing.T, spec string) (host, container string) {
	t.Helper()
	// trailing ":ro"
	body := spec[:len(spec)-len(":ro")]
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
