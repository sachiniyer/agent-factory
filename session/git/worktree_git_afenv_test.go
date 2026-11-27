package git

import (
	"slices"
	"testing"
)

// #7891 R1: when the docker runtime marks the session (AF_GIT_CONFIG set
// to the af-owned file), the agent-server's own git — the setup fetch, the
// archive push, worktree_push — reads only config files af created: the
// system file off, the global file replaced, and every redirect name
// dropped. Without the marker the environment passes through byte-for-byte
// (the host daemon, plain sessions, an operator-claimed route).
func TestAfGitScopedEnvironmentLocksDownWhenMarked(t *testing.T) {
	in := []string{
		"HOME=/af-home",
		"GIT_CONFIG_GLOBAL=/image/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/image-gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"XDG_CONFIG_HOME=/image/xdg",
		"GIT_SSL_NO_VERIFY=1",
	}
	t.Setenv(afGitConfigEnvName, "/af-home/.af-gitconfig")
	got := afGitScopedEnvironment(in)

	for _, name := range []string{
		"GIT_CONFIG_GLOBAL=/image/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/image-gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"XDG_CONFIG_HOME=/image/xdg",
	} {
		if slices.Contains(got, name) {
			t.Fatalf("a config-file redirect must be dropped under the lockdown: %v", got)
		}
	}
	if !slices.Contains(got, "GIT_CONFIG_NOSYSTEM=1") {
		t.Fatalf("system config must be off for af's git: %v", got)
	}
	if !slices.Contains(got, "GIT_CONFIG_GLOBAL=/af-home/.af-gitconfig") {
		t.Fatalf("af's git must read the af-owned global file: %v", got)
	}
	// Only the config roots change: unrelated env (including a forwarded
	// operator claim like GIT_SSL_NO_VERIFY) passes through untouched.
	if !slices.Contains(got, "HOME=/af-home") || !slices.Contains(got, "GIT_SSL_NO_VERIFY=1") {
		t.Fatalf("non-config env must pass through verbatim: %v", got)
	}
}

func TestAfGitScopedEnvironmentUnmarkedIsIdentity(t *testing.T) {
	in := []string{"GIT_CONFIG_GLOBAL=/image/.gitconfig", "XDG_CONFIG_HOME=/x"}
	if got := afGitScopedEnvironment(in); !slices.Equal(got, in) {
		t.Fatalf("without the marker env the environment must pass through unchanged: %v", got)
	}
}
