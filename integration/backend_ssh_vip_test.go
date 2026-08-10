package integration_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #3122: an L4 load-balancer VIP is ONE ADDRESS THAT IS NOT ONE MACHINE.
//
// #3086/#3118 fixed DNS multiplicity — one name resolving to several addresses —
// by pinning every transport step to one resolved address. A balancer defeats
// that without violating it: it selects a backend PER TCP CONNECTION, and af
// opens one connection per step, so every step dials the same pinned address and
// still reaches different machines. The pin is satisfied; the invariant is not.
//
// THIS TEST FAILS ON THE COMMIT BEFORE ITS FIX, and for the right reason: the
// workspace is created on one container and the reap runs on the other, whose
// `rm -rf` succeeds having removed nothing and reports success, leaving the real
// workspace behind. That is #3086's original symptom with #3118 in place.
//
// The two backends deliberately SHARE a host key — the image bakes one with
// `ssh-keygen -A` at build time, exactly as a real balanced fleet is configured.
// That sharing is what makes the split silent instead of a verification error,
// and it is also why identifying the machine by its host key cannot work here.
func TestSSHBackendBehindLoadBalancerStaysOnOneMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend integration; skipped under -short — see #2052")
	}
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	defer session.SetSSHSelfBinaryForTest(afBin)()
	defer session.SetSSHRelayBinaryForTest(afBin)()

	image := buildSSHDRoundTripImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "ssh vip\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	keyDir := t.TempDir()
	privKey := filepath.Join(keyDir, "id_ed25519")
	runExternal(t, "", "ssh-keygen", "-t", "ed25519", "-N", "", "-f", privKey, "-C", "af-ssh-vip")
	pubKey := privKey + ".pub"

	// TWO backends, from ONE image, so they share host keys the way a balanced
	// fleet does. Each has its own filesystem, which is what makes a wrong-machine
	// reap observable at all.
	var backends []string
	var names []string
	for i, suffix := range []string{"a", "b"} {
		cname := fmt.Sprintf("af-ssh-vip-%d-%s", os.Getpid(), suffix)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
		runExternal(t, "", "docker", "run", "-d", "--name", cname,
			"-p", "127.0.0.1::22",
			"-v", bare+":/repo.git:ro",
			"-v", pubKey+":/authorized_keys:ro",
			image)
		port := dockerPublishedHostPort(t, cname, "22")
		waitForSSH(t, port)
		// Pre-seed on BOTH what af's own configureGit sets on ONE. Without this the
		// split shows up earlier and less usefully: `git config --global` lands on one
		// backend and the clone on the other, which fails with "dubious ownership"
		// before any workspace exists to leak. That is a real second symptom of this
		// bug — provisioning behind a VIP is unreliable, not merely leaky — but it
		// aborts the run before the assertion this test exists for. Seeding it isolates
		// the reap-side split.
		runExternal(t, "", "docker", "exec", cname, "git", "config", "--global", "--add", "safe.directory", "*")
		backends = append(backends, "127.0.0.1:"+port)
		names = append(names, cname)
		t.Logf("backend %d: container %s at 127.0.0.1:%s", i, cname, port)
	}

	vip := startRoundRobinVIP(t, backends)
	t.Logf("VIP %s -> %v (a new backend per TCP connection)", vip, backends)

	// known_hosts is seeded through the VIP, which is what an operator would do:
	// the fleet's shared key answers whichever backend the scan reaches.
	_, vipPort, err := net.SplitHostPort(vip)
	if err != nil {
		t.Fatalf("split vip: %v", err)
	}
	knownHosts := writeKnownHostsForContainer(t, vipPort)
	writeSSHRepoConfig(t, repo, vip, "root", privKey, knownHosts)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "ssh-vip",
		Path:    repo,
		Program: "cat",
		Backend: session.BackendSSH,
	})
	if err != nil {
		// Unfixed, this is where the run ends, and the message names the mechanism:
		// every step opens its own TCP connection, the balancer hands consecutive
		// steps different machines, and a step cannot see what the previous one
		// created — `mktemp -d` lands on one backend and the af-binary stream on the
		// other, which reports "nonexistent directory".
		//
		// So behind a strict round-robin VIP the session does not merely leak, it
		// cannot be built at all; the leak is what is left over afterwards. Both are
		// #3122, and the leftover check below is reported either way because a failed
		// provision still had a workspace to clean up.
		t.Fatalf("provisioning behind an L4 VIP failed: %v\nworkspaces left behind: %v",
			err, containersWithSessionDirs(t, names))
	}

	// Exactly one machine must hold the workspace. (Two would mean provisioning
	// itself split, which would be an even worse shape of the same bug.)
	holders := containersWithSessionDirs(t, names)
	if len(holders) != 1 {
		_ = inst.Kill()
		t.Fatalf("expected the workspace on exactly ONE backend, found it on %v", holders)
	}
	t.Logf("workspace provisioned on %s", holders[0])

	// The reap. On the unfixed code this opens a fresh connection, the balancer
	// hands it the OTHER backend, and `rm -rf` succeeds there having removed
	// nothing.
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill (the reap) failed: %v", err)
	}

	if left := containersWithSessionDirs(t, names); len(left) != 0 {
		t.Fatalf("WORKSPACE LEAKED on %v: the reap reported success but ran on a different backend than the "+
			"one holding the workspace. An L4 balancer picks a backend per TCP CONNECTION, so pinning the "+
			"ADDRESS (#3118) is satisfied while the session still splits across MACHINES (#3122)", left)
	}
}

// containersWithSessionDirs names the containers that still hold a per-session
// directory, which is the whole observable of this test: it is how a leak on the
// machine the reap did not reach becomes visible.
func containersWithSessionDirs(t *testing.T, names []string) []string {
	t.Helper()
	var holders []string
	for _, c := range names {
		if dirs := sshdSessionDirs(t, c); len(dirs) > 0 {
			holders = append(holders, c)
		}
	}
	return holders
}

// startRoundRobinVIP is a minimal L4 balancer: accept, choose the next backend,
// splice. Per-connection backend selection IS the mechanism under test, so this
// models the property that matters rather than a particular product.
//
// In-process rather than another container: it needs no image, it cannot outlive
// the test, and its selection is deterministic, so a failure here is the code
// under test rather than a scheduler.
func startRoundRobinVIP(t *testing.T, backends []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the VIP: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var n uint64
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			target := backends[int(atomic.AddUint64(&n, 1)-1)%len(backends)]
			go func(c net.Conn, target string) {
				defer func() { _ = c.Close() }()
				u, dialErr := net.Dial("tcp", target)
				if dialErr != nil {
					return
				}
				defer func() { _ = u.Close() }()
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(u, c)
					if tc, ok := u.(*net.TCPConn); ok {
						_ = tc.CloseWrite()
					}
					close(done)
				}()
				_, _ = io.Copy(c, u)
				<-done
			}(c, target)
		}
	}()
	return ln.Addr().String()
}
