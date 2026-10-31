//go:build !windows

package sessionenv

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startBroker serves a broker on a temp socket. lookup stands in for tmux; every
// claim in these tests comes from THIS process, so its pid is the peer pid.
func startBroker(t *testing.T, lookup PaneLookup) (*CapabilityBroker, string) {
	t.Helper()
	b := NewCapabilityBroker(lookup)
	dir, err := os.MkdirTemp("", "afcap")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go b.Serve(ln)
	return b, sock
}

func paneOf(session string, pid int) PaneLookup {
	return func(_ context.Context, pane string) (PaneInfo, error) {
		if pane != "%1" && pane != "%2" {
			return PaneInfo{}, os.ErrNotExist
		}
		return PaneInfo{Session: session, RootPID: pid}, nil
	}
}

func claimAs(t *testing.T, sock, token, pane string) (Capability, error) {
	t.Helper()
	t.Setenv("TMUX_PANE", pane)
	return ClaimCapability(sock, token)
}

func testCap() Capability {
	return Capability{Session: "s", Agent: "claude", Account: "work", Command: "claude --x", Extras: []string{"FOO"}}
}

func TestCapability_ClaimCarriesTheAdmittedValuesAndIsSingleUse(t *testing.T) {
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	token, err := b.Mint(testCap())
	require.NoError(t, err)

	got, err := claimAs(t, sock, token, "%1")
	require.NoError(t, err)
	require.Equal(t, testCap(), got, "the shim must receive exactly what the launcher admitted")

	// Replay: the same pane root (the program the shim exec'd keeps the pid)
	// asks again and is refused because the capability was used.
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "already used")
}

func TestCapability_RefusedClaimsDoNotConsume(t *testing.T) {
	var cur atomic.Pointer[PaneInfo]
	cur.Store(&PaneInfo{Session: "s", RootPID: os.Getpid() + 1}) // tmux says another pid is the pane root
	b, sock := startBroker(t, func(context.Context, string) (PaneInfo, error) { return *cur.Load(), nil })
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	// A pre-claim from a process that is not the pane root.
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "is not the root of pane")

	cur.Store(&PaneInfo{Session: "other-session", RootPID: os.Getpid()})
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "not in the launch's session")

	cur.Store(&PaneInfo{Session: "s", RootPID: os.Getpid()})
	_, err = claimAs(t, sock, token, "%1")
	require.NoError(t, err, "the real pane root can still claim after strangers failed")
}

func TestCapability_FailsClosed(t *testing.T) {
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	for name, tc := range map[string]struct{ token, pane string }{
		"unknown token": {"deadbeef", "%1"},
		"empty token":   {"", "%1"},
		"no pane":       {token, ""},
		"unknown pane":  {token, "%9"},
	} {
		_, err := claimAs(t, sock, tc.token, tc.pane)
		require.Error(t, err, name)
	}
	_, err = ClaimCapability(filepath.Join(t.TempDir(), "absent.sock"), token)
	require.ErrorContains(t, err, "unreachable")
}

func TestCapability_PerPaneClaimsOncePerPane(t *testing.T) {
	c := testCap()
	c.PerPane = true
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	token, err := b.Mint(c)
	require.NoError(t, err)
	_, err = claimAs(t, sock, token, "%1")
	require.NoError(t, err)
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "already claimed")
	_, err = claimAs(t, sock, token, "%2")
	require.NoError(t, err, "a new window runs the default command again")
}

func TestCapability_ExpiresAndIsBounded(t *testing.T) {
	b := NewCapabilityBroker(paneOf("s", 1))
	now := time.Now()
	b.now = func() time.Time { return now }
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	now = now.Add(capabilityTTL + time.Second)
	_, err = b.Claim(context.Background(), token, "%1", 1)
	require.ErrorContains(t, err, "unknown or already used")
	for i := 0; i < capabilityMaxRecords; i++ {
		_, err = b.Mint(testCap())
		require.NoError(t, err)
	}
	_, err = b.Mint(testCap())
	require.ErrorContains(t, err, "too many")
}

// A same-user process can re-bind the socket path. The shim must refuse an
// endpoint that is not this executable, and say so before sending the token.
func TestCapability_ShimRefusesAForeignServer(t *testing.T) {
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	prev := exeIdentityOf
	t.Cleanup(func() { exeIdentityOf = prev })
	// The server here is this same process, so make "the server" look foreign by
	// answering differently for the shim's own identity probe order: self first.
	calls := 0
	exeIdentityOf = func(pid int) (exeID, error) {
		calls++
		if calls == 1 {
			return exeID{path: "/usr/local/bin/af", inode: 7}, nil
		}
		return exeID{path: "/tmp/evil", inode: 9}, nil
	}
	_, err = claimAs(t, sock, token, "%1")
	require.ErrorContains(t, err, "the claim endpoint is not af")
	b.mu.Lock()
	require.Len(t, b.recs, 1, "a refused server never saw the token, so nothing was consumed")
	b.mu.Unlock()
}

func TestSameExecutableAsSelf_SwapWindow(t *testing.T) {
	prev := exeIdentityOf
	t.Cleanup(func() { exeIdentityOf = prev })
	self := os.Getpid()
	for name, tc := range map[string]struct {
		me, peer exeID
		ok       bool
	}{
		"same file":                 {exeID{"/b/af", 5, false}, exeID{"/b/af", 5, false}, true},
		"daemon predates a swap":    {exeID{"/b/af", 6, false}, exeID{"/b/af", 5, true}, true},
		"different path":            {exeID{"/b/af", 5, false}, exeID{"/tmp/evil", 5, false}, false},
		"different file, same path": {exeID{"/b/af", 6, false}, exeID{"/b/af", 5, false}, false},
		"peer has no identity":      {exeID{"/b/af", 6, false}, exeID{}, false},
		"shim has no identity":      {exeID{}, exeID{"/b/af", 5, false}, false},
	} {
		exeIdentityOf = func(pid int) (exeID, error) {
			if pid == self {
				return tc.me, nil
			}
			return tc.peer, nil
		}
		err := sameExecutableAsSelf(self + 1)
		if tc.ok {
			require.NoError(t, err, name)
		} else {
			require.Error(t, err, name)
		}
	}
}

// Measured on the CI runner for each OS: what the peer-identity primitives see.
// Logged so a platform that cannot answer is visible rather than silent.
func TestPeerIdentityPrimitivesAnswerOnThisPlatform(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "p.sock"))
	require.NoError(t, err)
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	client, err := net.Dial("unix", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server := <-accepted
	defer server.Close()

	pid, err := peerPID(client)
	require.NoError(t, err, "peer pid must be readable on %s", runtime.GOOS)
	require.Equal(t, os.Getpid(), pid)
	id, err := exeIdentityOf(pid)
	require.NoError(t, err, "executable identity must be readable on %s", runtime.GOOS)
	t.Logf("%s: peer pid=%d exe path=%q inode=%d deleted=%v", runtime.GOOS, pid, id.path, id.inode, id.deleted)
	require.NoError(t, sameExecutableAsSelf(pid))
}

func TestNormalizeExeLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc semantics")
	}
	require.Equal(t, exeID{path: "/b/af", inode: 3, deleted: true}, normalizeExeLink("/b/af (deleted)", 0, 3))
	require.Equal(t, exeID{path: "/b/af (deleted)", inode: 3}, normalizeExeLink("/b/af (deleted)", 1, 3),
		"a live file literally named 'af (deleted)' must not be read as a replaced binary")
}

// Linux only: replace a running binary the way dev-install.sh does and check
// the identity survives it, with a real process.
func TestExeIdentitySurvivesABinarySwap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/<pid>/exe is Linux-specific")
	}
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	dir := t.TempDir()
	bin := filepath.Join(dir, "af")
	data, err := os.ReadFile(sleep)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bin, data, 0o755))
	cmd := exec.Command(bin, "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	before, err := exeIdentityOf(cmd.Process.Pid)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bin+".new", data, 0o755))
	require.NoError(t, os.Rename(bin+".new", bin))
	after, err := exeIdentityOf(cmd.Process.Pid)
	require.NoError(t, err)
	require.True(t, after.deleted, "pid %s: %+v", strconv.Itoa(cmd.Process.Pid), after)
	require.Equal(t, before.path, after.path)
	require.True(t, strings.HasSuffix(after.path, "/af"))

	// The spoof: a live file literally named "af (deleted)".
	spoof := filepath.Join(dir, "af (deleted)")
	require.NoError(t, os.WriteFile(spoof, data, 0o755))
	sp := exec.Command(spoof, "30")
	require.NoError(t, sp.Start())
	t.Cleanup(func() { _ = sp.Process.Kill(); _, _ = sp.Process.Wait() })
	got, err := exeIdentityOf(sp.Process.Pid)
	require.NoError(t, err)
	require.False(t, got.deleted)
	require.Equal(t, spoof, got.path)
}

func TestCapability_PerPaneCapabilityOutlivesTheShortExpiryUntilRevoked(t *testing.T) {
	b := NewCapabilityBroker(paneOf("s", 1))
	now := time.Now()
	b.now = func() time.Time { return now }
	perPane := testCap()
	perPane.PerPane = true
	pt, err := b.Mint(perPane)
	require.NoError(t, err)
	ot, err := b.Mint(testCap()) // one-shot keeps the short expiry
	require.NoError(t, err)

	now = now.Add(24 * time.Hour) // a long-lived session opening a new window
	_, err = b.Claim(context.Background(), pt, "%1", 1)
	require.NoError(t, err, "tmux default-command runs for every new window for the whole session")
	_, err = b.Claim(context.Background(), ot, "%1", 1)
	require.ErrorContains(t, err, "unknown or already used", "a one-shot launch capability still expires")

	b.Revoke("s")
	_, err = b.Claim(context.Background(), pt, "%2", 1)
	require.ErrorContains(t, err, "unknown or already used", "ending the session revokes it")
}
