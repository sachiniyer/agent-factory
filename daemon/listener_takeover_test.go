package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSecondHomeDaemonNeverTakesDefaultPortFromForeignDaemon is the #6858
// regression: two agent-factory homes, one default web address. A daemon
// under a SECOND home (a non-default AGENT_FACTORY_HOME) with only the
// default network.listen_addr finds the port held by the first home's
// daemon — and must NEVER take it, including the moment the first daemon's
// restart releases it. Before the fix the #6386 retry loop kept re-attempting
// the bind and won the port in exactly that restart gap, leaving the real
// daemon the one stuck retrying while the user's proxy, bookmarks and tokens
// pointed at the wrong daemon.
//
// The simulation uses real kernel state end to end — no stubbed classifier:
// this test process stands in for the first daemon by holding (a) a real
// listening socket on the contested port and (b) a real, flock'd daemon.lock
// in a DIFFERENT af home — the exact fd evidence the holder classification
// keys on. The shipped default port is never bound: defaultWebListenAddr is
// re-pointed at a free loopback port for the duration (#6625), which is also
// what makes "the first daemon's restart" representable as a close().
func TestSecondHomeDaemonNeverTakesDefaultPortFromForeignDaemon(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port-holder identification reads /proc on linux")
	}
	// Our home is a throwaway that is NOT the default ~/.agent-factory — the
	// second-home posture the gate protects.
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// Point the default home at an EMPTY fixture so the default-home claim
	// check never fires here (this host's real ~/.agent-factory may exist and
	// claim the port): this test exercises the EADDRINUSE holder-classify
	// path, which needs the bind to be attempted first.
	emptyHome := testguard.SocketTempDir(t)
	oldDefaultHome := defaultHomeDir
	defaultHomeDir = func() (string, error) { return emptyHome, nil }
	t.Cleanup(func() { defaultHomeDir = oldDefaultHome })

	// The contested address: a real loopback port standing in for the shipped
	// default. The first daemon holds it for now — a real LISTEN socket in
	// this process's fd table.
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	contested := holder.Addr().String()
	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = contested
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	// The first daemon's home marker: a live, flock'd daemon.lock in a home
	// that is not ours. A real af daemon holds exactly this fd for its whole
	// lifetime, and its path IS the home — the identity the classification
	// reads without trusting argv or environment spelling.
	foreignHome := testguard.SocketTempDir(t)
	lock, err := os.OpenFile(filepath.Join(foreignHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	defer func() { _ = lock.Close() }()

	// Shrink the retry backoff so the retry loop actually runs inside the
	// test window instead of parking on its 1s→30s schedule.
	oldInit, oldMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = oldInit, oldMax })

	// The second daemon: default web address (per the re-pointed default),
	// preview disabled, under the non-default home set above.
	cfg := config.DefaultConfig()
	cfg.ListenAddr = contested
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })

	// The first bind attempt meets the foreign-held port. The retry loop is
	// what startHTTPServer arms on a failed startup bind (#6386).
	_, _ = wl.reconcile(m.Config())
	wl.retryFailedBinds()

	// The first daemon "restarts": it releases the contested port.
	require.NoError(t, holder.Close())

	snap := func() DaemonListenerStatus { return m.lifecycle.snapshot().listeners }
	// The property: for the whole settle window the second daemon must stay
	// unbound. On master the retry loop binds the freed port — the takeover —
	// within a backoff tick, so this fails fast there. With the fix the bind
	// is a terminal refusal and nothing is ever bound.
	require.Never(t, func() bool { return snap().TCPBound },
		400*time.Millisecond, 10*time.Millisecond,
		"the second-home daemon bound %s — a port another AF home's daemon held when it first tried — the #6858 takeover",
		contested)
	// The give-up must explain itself: the refusal is recorded on the
	// lifecycle status `af daemon status` renders and `af doctor` reads, and
	// it names the remedy — set [network] listen_addr to another port.
	require.NotEmpty(t, snap().TCPRefusalReason,
		"a refused takeover must say why: the lifecycle's TCPRefusalReason should name the foreign-home holder")
	require.Contains(t, snap().TCPRefusalReason, "network.listen_addr",
		"the refusal must name the fix — set [network] listen_addr to another port")
}

// takeoverFixture pins the three knobs the gate reads — our home
// (AGENT_FACTORY_HOME), the contested "default" address, and the default home
// directory — plus a *webListeners on a real Manager so bind/refusal state has
// somewhere to live. It returns the webListeners and the default-home dir a
// test may fill with a config.toml or a flock'd daemon.lock.
func takeoverFixture(t *testing.T) (*webListeners, string) {
	t.Helper()
	myHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", myHome)
	defHome := testguard.SocketTempDir(t)
	oldDefaultHome := defaultHomeDir
	defaultHomeDir = func() (string, error) { return defHome, nil }
	t.Cleanup(func() { defaultHomeDir = oldDefaultHome })
	cfg := config.DefaultConfig()
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })
	return wl, defHome
}

// reqFor resolves a listen_addr the way the gate sees it.
func reqFor(t *testing.T, addr string) *net.TCPAddr {
	t.Helper()
	req, err := net.ResolveTCPAddr("tcp", addr)
	require.NoError(t, err)
	return req
}

func TestTakeoverGateApplies(t *testing.T) {
	myHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", myHome)
	defHome := testguard.SocketTempDir(t)
	oldDefaultHome := defaultHomeDir
	defaultHomeDir = func() (string, error) { return defHome, nil }
	t.Cleanup(func() { defaultHomeDir = oldDefaultHome })

	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = "127.0.0.1:8443"
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	t.Run("applies: default addr, non-default home", func(t *testing.T) {
		require.True(t, takeoverGateApplies(reqFor(t, "127.0.0.1:8443")))
	})
	t.Run("applies: wildcard spellings that collide with the default port", func(t *testing.T) {
		require.True(t, takeoverGateApplies(reqFor(t, "0.0.0.0:8443")))
		require.True(t, takeoverGateApplies(reqFor(t, ":8443")))
	})
	t.Run("does not apply: same port on a distinct address coexists", func(t *testing.T) {
		require.False(t, takeoverGateApplies(reqFor(t, "127.0.0.2:8443")))
	})
	t.Run("does not apply: any other port is the user's own instruction", func(t *testing.T) {
		require.False(t, takeoverGateApplies(reqFor(t, "127.0.0.1:9000")))
		require.False(t, takeoverGateApplies(reqFor(t, "0.0.0.0:9000")))
	})
	t.Run("does not apply: our home IS the default home", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", defHome)
		require.False(t, takeoverGateApplies(reqFor(t, "127.0.0.1:8443")))
	})
	t.Run("does not apply: the default home is unresolvable", func(t *testing.T) {
		defaultHomeDir = func() (string, error) { return "", errors.New("no home") }
		require.False(t, takeoverGateApplies(reqFor(t, "127.0.0.1:8443")))
		defaultHomeDir = func() (string, error) { return defHome, nil }
	})
}

// writeDefHomeConfig drops a config.toml into the fixture default home.
func writeDefHomeConfig(t *testing.T, defHome, listenAddrLine string) {
	t.Helper()
	content := "schema_version = 1\n"
	if listenAddrLine != "" {
		content += "[network]\nlisten_addr = " + listenAddrLine + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(defHome, "config.toml"), []byte(content), 0600))
}

func TestDefaultHomeClaimRefusal(t *testing.T) {
	_, defHome := takeoverFixture(t)
	req := reqFor(t, "127.0.0.1:8443")

	t.Run("never-configured default home claims nothing", func(t *testing.T) {
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a materialized default config claims the port", func(t *testing.T) {
		// What every real default home looks like: first-run materialization
		// writes the default listen_addr into config.toml — the claim does not
		// need a live daemon.
		writeDefHomeConfig(t, defHome, `"127.0.0.1:8443"`)
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
	})
	t.Run("an absent key defaults to the claim", func(t *testing.T) {
		writeDefHomeConfig(t, defHome, "")
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
	})
	t.Run("an explicitly disabled listener claims nothing", func(t *testing.T) {
		writeDefHomeConfig(t, defHome, `""`)
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a moved listener on another port claims nothing", func(t *testing.T) {
		writeDefHomeConfig(t, defHome, `"127.0.0.1:9443"`)
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a wildcard listener on the same port claims it", func(t *testing.T) {
		writeDefHomeConfig(t, defHome, `"0.0.0.0:8443"`)
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
	})
	t.Run("a live daemon claims it with no config at all", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(defHome, "config.toml")))
		lock, err := os.OpenFile(filepath.Join(defHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		defer func() { _ = lock.Close() }()
		require.Contains(t, defaultHomeClaimRefusal(req), "daemon.lock")
	})
	t.Run("an unparseable config falls back to the lock", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(defHome, "config.toml"), []byte("this is [ not toml"), 0600))
		require.Empty(t, defaultHomeClaimRefusal(req), "no lock held and an unreadable config → no positive claim")
		lock, err := os.OpenFile(filepath.Join(defHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		defer func() { _ = lock.Close() }()
		require.Contains(t, defaultHomeClaimRefusal(req), "daemon.lock")
	})
}

func TestClassifyPortHolderRealProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port-holder identification reads /proc on linux")
	}
	myHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", myHome)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	req := reqFor(t, l.Addr().String())

	t.Run("our own non-daemon process classifies non-daemon", func(t *testing.T) {
		verdict, _ := classifyPortHolder(req)
		require.Equal(t, holderNonDaemon, verdict,
			"a plain process holding the port is a squatter — the retry exists for it")
	})

	t.Run("a lock fd in a foreign home classifies foreign", func(t *testing.T) {
		foreignHome := testguard.SocketTempDir(t)
		lock, err := os.OpenFile(filepath.Join(foreignHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		defer func() { _ = lock.Close() }()
		verdict, home := classifyPortHolder(req)
		require.Equal(t, holderForeign, verdict)
		require.Equal(t, foreignHome, home)
	})

	t.Run("a lock fd in OUR home classifies own — the #6386 case", func(t *testing.T) {
		lock, err := os.OpenFile(filepath.Join(myHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		defer func() { _ = lock.Close() }()
		verdict, _ := classifyPortHolder(req)
		require.Equal(t, holderOwn, verdict,
			"our own predecessor holding the port is the recovery the retry must keep")
	})

	t.Run("an unbound port finds no socket — unproven", func(t *testing.T) {
		require.NoError(t, l.Close())
		verdict, _ := classifyPortHolder(req)
		require.Equal(t, holderUnproven, verdict)
	})
}

// TestBindRefusesOnDefaultHomeClaim is the end-to-end half of the claim path:
// with the default home's config.toml claiming the default address, a
// non-default-home daemon's reconcile REFUSES — no bind is attempted, the
// refusal lands on the lifecycle status, and it latches so a later reconcile
// cannot sneak the bind through after the claim disappears.
func TestBindRefusesOnDefaultHomeClaim(t *testing.T) {
	wl, defHome := takeoverFixture(t)
	writeDefHomeConfig(t, defHome, `"127.0.0.1:8443"`)

	_, err := wl.reconcile(wl.manager.Config())
	require.NoError(t, err, "a refusal is a decision, not a bind error")
	snap := wl.manager.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound)
	require.NotEmpty(t, snap.TCPRefusalReason)
	require.Contains(t, snap.TCPRefusalReason, "network.listen_addr")

	// The claim vanishes (default home abandoned) — a reconcile on an
	// unrelated apply still must not bind: the latch owns this address for
	// the daemon's lifetime.
	require.NoError(t, os.Remove(filepath.Join(defHome, "config.toml")))
	_, err = wl.reconcile(wl.manager.Config())
	require.NoError(t, err)
	require.False(t, wl.manager.lifecycle.snapshot().listeners.TCPBound,
		"a latched refusal must not bind even after the claim is gone — binding then would win the port over the other home's next daemon")
	require.NotEmpty(t, wl.manager.lifecycle.snapshot().listeners.TCPRefusalReason)
}

// TestBindProceedsWhenNoHomeClaims is the second-profile-alone half: with no
// default-home claim and the port free, a non-default-home daemon on the
// default address binds normally. The bind uses :0 — never the shipped port —
// because defaultWebListenAddr only gates the COMPARISON.
func TestBindProceedsWhenNoHomeClaims(t *testing.T) {
	wl, _ := takeoverFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	free := l.Addr().String()
	require.NoError(t, l.Close())

	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = free
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	cfg := wl.manager.Config()
	cfg.ListenAddr = free
	_, err = wl.reconcile(cfg)
	require.NoError(t, err)
	snap := wl.manager.lifecycle.snapshot().listeners
	require.True(t, snap.TCPBound, "with no AF home claiming it, the second profile's default-addr daemon must still work")
	require.Empty(t, snap.TCPRefusalReason)
}

func TestForeignHolderRefusalSkips(t *testing.T) {
	_, _ = takeoverFixture(t)
	t.Run("non-default addresses never refuse", func(t *testing.T) {
		require.Empty(t, foreignHolderRefusal(reqFor(t, "127.0.0.1:9000")))
	})
}

func TestRefuseWebForeignLatch(t *testing.T) {
	wl, _ := takeoverFixture(t)
	const addr = "127.0.0.1:8443"
	reason := "refusing to bind " + addr + ": held by another home"
	wl.mu.Lock()
	wl.refuseWebForeignLocked(addr, reason)
	gen := wl.webGen
	snap := func() DaemonListenerStatus { return wl.manager.lifecycle.snapshot().listeners }
	require.True(t, snap().TCPConfigured, "a refused listener is still the configured one")
	require.False(t, snap().TCPBound)
	require.Equal(t, reason, snap().TCPRefusalReason)
	// Idempotent: the same refusal again neither re-bumps the generation nor
	// clears the latch.
	wl.refuseWebForeignLocked(addr, reason)
	require.Equal(t, gen, wl.webGen)
	require.Equal(t, reason, wl.webRefusal)
	// The latch remembers: a later reconcile at the same address sees it
	// refused without re-asking the kernel anything.
	require.Equal(t, reason, wl.foreignRefusals[addr])
	wl.mu.Unlock()
	require.Equal(t, reason, snap().TCPRefusalReason)
}

func TestParseProcNetTCP(t *testing.T) {
	req := reqFor(t, "127.0.0.1:8443")
	// Real /proc/net/tcp shape: header + LISTEN (0A) + established (01) rows.
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:20FB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0
   1: 00000000:20FC 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 22222 1 0000000000000000 100 0 0 10 0
   2: 0100007F:20FB 0200007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 99999 1 0000000000000000 100 0 0 10 0
   3: 0200007F:20FB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 77777 1 0000000000000000 100 0 0 10 0
`
	inodes := map[uint64]bool{}
	parseProcNetTCP([]byte(table), req, inodes)
	// 12345 = 127.0.0.1:8443 LISTEN → match. 22222 = port 8444 → no.
	// 99999 = same addr ESTABLISHED → no. 77777 = 127.0.0.2:8443 → no overlap.
	require.Equal(t, map[uint64]bool{12345: true}, inodes)

	// A wildcard request collides with EVERY same-port listener: 0.0.0.0:8443
	// covers 127.0.0.1 AND 127.0.0.2, so both LISTEN inodes match.
	wildReq := reqFor(t, "0.0.0.0:8443")
	inodes = map[uint64]bool{}
	parseProcNetTCP([]byte(table), wildReq, inodes)
	require.Equal(t, map[uint64]bool{12345: true, 77777: true}, inodes)
}

func TestProcHexIP(t *testing.T) {
	ip, ok := procHexIP("0100007F")
	require.True(t, ok)
	require.Equal(t, "127.0.0.1", ip.String())

	ip, ok = procHexIP("0000000000000000FFFF00000100007F")
	require.True(t, ok)
	require.True(t, ip.Equal(net.ParseIP("::ffff:127.0.0.1")), "v6 words decode little-endian, got %s", ip)

	_, ok = procHexIP("zz")
	require.False(t, ok)
	_, ok = procHexIP("0100")
	require.False(t, ok)
}

func TestSocketFDInode(t *testing.T) {
	ino, ok := socketFDInode("socket:[12345]")
	require.True(t, ok)
	require.Equal(t, uint64(12345), ino)
	_, ok = socketFDInode("/home/u/.agent-factory/daemon.lock")
	require.False(t, ok)
	_, ok = socketFDInode("anon_inode:[eventpoll]")
	require.False(t, ok)
}
