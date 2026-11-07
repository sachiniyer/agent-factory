package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	// that is not ours, with a daemon.pid naming this process. A real af
	// daemon holds exactly this fd pair for its whole lifetime, and its path
	// IS the home — the identity the classification reads without trusting
	// argv or environment spelling.
	foreignHome := testguard.SocketTempDir(t)
	afDaemonHomeFixture(t, foreignHome)

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
// somewhere to live. It returns the webListeners and both home dirs.
func takeoverFixture(t *testing.T) (wl *webListeners, defHome, myHome string) {
	t.Helper()
	myHome = testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", myHome)
	defHome = testguard.SocketTempDir(t)
	oldDefaultHome := defaultHomeDir
	defaultHomeDir = func() (string, error) { return defHome, nil }
	t.Cleanup(func() { defaultHomeDir = oldDefaultHome })
	cfg := config.DefaultConfig()
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl = newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })
	return wl, defHome, myHome
}

// contestPort re-points the "default" web address at a free loopback port and
// returns its listen_addr — the seam that keeps every reconcile in a test off
// the shipped 127.0.0.1:8443 (#6625).
func contestPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = addr
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })
	return addr
}

// afDaemonHomeFixture marks home as owned by THIS process the way a live af
// daemon marks its own home: a flock'd daemon.lock plus a daemon.pid naming
// our pid — the fd+identity evidence the holder classifier proves
// daemon-hood on (a bare daemon.lock basename proves nothing).
func afDaemonHomeFixture(t *testing.T, home string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0600))
	lock := flockHomeLock(t, home)
	t.Cleanup(func() { _ = lock.Close() })
}

// flockHomeLock holds a real exclusive flock on home's daemon.lock in THIS
// process — the kernel evidence daemonLockHeld proves a live daemon on. The
// caller closes the returned file to release it.
func flockHomeLock(t *testing.T, home string) *os.File {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(home, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	// A real daemon also publishes its own pid next to the lock it holds —
	// the daemon.pid channel is what carries the live-daemon evidence where
	// /proc/locks does not exist (darwin: the named pid's fd table, read via
	// proc_pidinfo, shows this lock open).
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0600))
	return lock
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

// stubIsEnabledAnswer makes the service-manager probe's is-enabled query
// answer word, restoring the real runner on cleanup.
func stubIsEnabledAnswer(t *testing.T, word string) {
	t.Helper()
	prev := autostartProbeCommand
	t.Cleanup(func() { autostartProbeCommand = prev })
	autostartProbeCommand = func(name string, args ...string) probeResult {
		return probeResult{completed: true, output: word + "\n", exitCode: 0}
	}
}

// writeAutostartUnit drops a unit serving home into dir the way
// InstallAutostart renders it; home == "" means the unit baked no
// AGENT_FACTORY_HOME and serves the default home.
func writeAutostartUnit(t *testing.T, dir, home string) {
	t.Helper()
	unit := systemdAutostartUnit("/usr/bin/af", "/usr/bin:/bin", "/bin/sh", home)
	require.NoError(t, os.WriteFile(filepath.Join(dir, autostartUnitName), []byte(unit), 0644))
}

func TestDefaultHomeClaimRefusal(t *testing.T) {
	_, defHome, _ := takeoverFixture(t)
	req := reqFor(t, "127.0.0.1:8443")

	t.Run("a stray materialized config alone claims nothing", func(t *testing.T) {
		// THE relocated-home regression a config-based claim caused: a
		// leftover ~/.agent-factory still carries the materialized
		// listen_addr, but with no daemon and nothing set to restart one it
		// claims nothing — refusing here would strand the relocated user's
		// only daemon off its own port.
		writeDefHomeConfig(t, defHome, `"127.0.0.1:8443"`)
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a never-configured default home claims nothing", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(defHome, "config.toml")))
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a live daemon.lock claims the port — running evidence", func(t *testing.T) {
		lock := flockHomeLock(t, defHome)
		defer func() { _ = lock.Close() }()
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
		require.Contains(t, defaultHomeClaimRefusal(req), "daemon.lock")
	})
	t.Run("a live daemon narrowed away by a moved listener claims nothing", func(t *testing.T) {
		lock := flockHomeLock(t, defHome)
		defer func() { _ = lock.Close() }()
		writeDefHomeConfig(t, defHome, `"127.0.0.1:9443"`)
		require.Empty(t, defaultHomeClaimRefusal(req),
			"a running daemon on another port has nothing to lose on this one")
	})
	t.Run("a live daemon narrowed away by a disabled listener claims nothing", func(t *testing.T) {
		lock := flockHomeLock(t, defHome)
		defer func() { _ = lock.Close() }()
		writeDefHomeConfig(t, defHome, `""`)
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a live daemon on a wildcard same-port listener claims it", func(t *testing.T) {
		lock := flockHomeLock(t, defHome)
		defer func() { _ = lock.Close() }()
		writeDefHomeConfig(t, defHome, `"0.0.0.0:8443"`)
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
	})
	t.Run("a live daemon with an unreadable config stays claimed", func(t *testing.T) {
		lock := flockHomeLock(t, defHome)
		defer func() { _ = lock.Close() }()
		require.NoError(t, os.WriteFile(filepath.Join(defHome, "config.toml"),
			[]byte("this is [ not toml"), 0600))
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port",
			"unreadable intent is not disproven intent — a running daemon keeps its claim")
	})
	t.Run("a stale daemon.pid claims nothing", func(t *testing.T) {
		// daemon.lock exists but nobody holds it; the pid file names a pid
		// that cannot exist — beyond every platform's pid_max — so the
		// home-published channel has no live daemon to prove.
		f, err := os.OpenFile(filepath.Join(defHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		require.NoError(t, os.WriteFile(filepath.Join(defHome, "daemon.pid"),
			[]byte("1073741824"), 0600))
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("a live pid not attached to this home's lock claims nothing", func(t *testing.T) {
		// daemon.pid names THIS live test process, but no fd of it is open
		// on def's daemon.lock and its argv is not an af daemon's — a pid
		// file alone proves nothing about who it names.
		f, err := os.OpenFile(filepath.Join(defHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, f.Close()) // exists, but unheld and unopened by the named pid
		require.NoError(t, os.WriteFile(filepath.Join(defHome, "daemon.pid"),
			[]byte(strconv.Itoa(os.Getpid())), 0600))
		require.Empty(t, defaultHomeClaimRefusal(req))
	})
	t.Run("an enabled autostart unit serving it claims the port", func(t *testing.T) {
		// No daemon running — the lock file is absent — but the OS is set to
		// bring one back: installed unit serving this home + is-enabled.
		unitDir := withAutostartTestEnv(t, "linux")
		writeAutostartUnit(t, unitDir, defHome)
		stubIsEnabledAnswer(t, "enabled")
		require.NoError(t, os.Remove(filepath.Join(defHome, "config.toml")))
		require.NoError(t, os.Remove(filepath.Join(defHome, daemonLockFileName)))
		require.Contains(t, defaultHomeClaimRefusal(req), "claims this port")
		require.Contains(t, defaultHomeClaimRefusal(req), "autostart")
	})
	t.Run("a serving unit that is disabled claims nothing", func(t *testing.T) {
		unitDir := withAutostartTestEnv(t, "linux")
		writeAutostartUnit(t, unitDir, defHome)
		stubIsEnabledAnswer(t, "disabled")
		require.Empty(t, defaultHomeClaimRefusal(req),
			"a disabled unit will not restart anything — it is not 'comes back' evidence")
	})
	t.Run("an enabled unit serving ANOTHER home claims nothing", func(t *testing.T) {
		unitDir := withAutostartTestEnv(t, "linux")
		writeAutostartUnit(t, unitDir, testguard.SocketTempDir(t))
		stubIsEnabledAnswer(t, "enabled")
		require.Empty(t, defaultHomeClaimRefusal(req),
			"the unit restarts someone else's daemon, not this home's")
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
		afDaemonHomeFixture(t, foreignHome)
		verdict, home := classifyPortHolder(req)
		require.Equal(t, holderForeign, verdict)
		require.Equal(t, foreignHome, home)
	})

	t.Run("a bare daemon.lock basename proves nothing without a naming daemon.pid", func(t *testing.T) {
		// A non-af process can hold a file named daemon.lock; the fd is only
		// af identity when the sibling daemon.pid names the holder. Ours does
		// not — the pid file is absent — so this process is what it is: the
		// test binary, provably not an af daemon.
		foreignHome := testguard.SocketTempDir(t)
		lock, err := os.OpenFile(filepath.Join(foreignHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		defer func() { _ = lock.Close() }()
		verdict, _ := classifyPortHolder(req)
		require.Equal(t, holderNonDaemon, verdict,
			"a stray daemon.lock fd must not turn a squatter into a foreign af daemon")
	})

	t.Run("a lock fd in OUR home classifies own — the #6386 case", func(t *testing.T) {
		afDaemonHomeFixture(t, myHome)
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
// with the default home's daemon provably running and still wanting this
// port, a non-default-home daemon's reconcile REFUSES — no bind is attempted,
// the refusal lands on the lifecycle status, and it latches so a later
// reconcile cannot sneak the bind through after the claim disappears. The
// contested port is a fixture throughout — a broken claim must never be able
// to bind the shipped 8443 as its failure mode (#6625).
func TestBindRefusesOnDefaultHomeClaim(t *testing.T) {
	wl, defHome, _ := takeoverFixture(t)
	contested := contestPort(t)

	// The claim: a live daemon.lock in the default home — its daemon is
	// running — with a config.toml still wanting this very port.
	lock := flockHomeLock(t, defHome)
	writeDefHomeConfig(t, defHome, `"`+contested+`"`)

	cfg := wl.manager.Config()
	cfg.ListenAddr = contested
	_, err := wl.reconcile(cfg)
	require.NoError(t, err, "a refusal is a decision, not a bind error")
	snap := wl.manager.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound)
	require.NotEmpty(t, snap.TCPRefusalReason)
	require.Contains(t, snap.TCPRefusalReason, "network.listen_addr")

	// The claim vanishes (daemon stopped, home cleared) — a later reconcile
	// still must not bind: the latch owns this address for our lifetime.
	require.NoError(t, lock.Close())
	require.NoError(t, os.Remove(filepath.Join(defHome, "config.toml")))
	_, err = wl.reconcile(wl.manager.Config())
	require.NoError(t, err)
	require.False(t, wl.manager.lifecycle.snapshot().listeners.TCPBound,
		"a latched refusal must not bind even after the claim is gone — binding then would win the port over the other home's next daemon")
	require.NotEmpty(t, wl.manager.lifecycle.snapshot().listeners.TCPRefusalReason)
}

// TestRelocatedHomeKeepsDefaultPort is the review's regression case: the
// user's real AGENT_FACTORY_HOME was relocated and a STRAY default home is
// left behind — a materialized config.toml still naming the contested port,
// but no daemon, no lock, no unit to bring one back. A config file alone is
// not a claim: the relocated daemon must bind, or the port — and the user's
// whole web UI — is silently lost behind a refusal the TUI only whispered.
func TestRelocatedHomeKeepsDefaultPort(t *testing.T) {
	wl, defHome, _ := takeoverFixture(t)
	contested := contestPort(t)

	// The stray home: exactly what first-run materialization leaves behind.
	writeDefHomeConfig(t, defHome, `"`+contested+`"`)

	cfg := wl.manager.Config()
	cfg.ListenAddr = contested
	_, err := wl.reconcile(cfg)
	require.NoError(t, err)
	snap := wl.manager.lifecycle.snapshot().listeners
	require.True(t, snap.TCPBound,
		"a stray default home's materialized config must not take the relocated user's port")
	require.Empty(t, snap.TCPRefusalReason)
}

// TestBindProceedsWhenNoHomeClaims is the second-profile-alone half: with no
// default-home claim and the port free, a non-default-home daemon on the
// default address binds normally.
func TestBindProceedsWhenNoHomeClaims(t *testing.T) {
	wl, _, _ := takeoverFixture(t)
	free := contestPort(t)

	cfg := wl.manager.Config()
	cfg.ListenAddr = free
	_, err := wl.reconcile(cfg)
	require.NoError(t, err)
	snap := wl.manager.lifecycle.snapshot().listeners
	require.True(t, snap.TCPBound, "with no AF home claiming it, the second profile's default-addr daemon must still work")
	require.Empty(t, snap.TCPRefusalReason)
}

func TestForeignHolderRefusalSkips(t *testing.T) {
	_, _, _ = takeoverFixture(t)
	t.Run("non-default addresses never refuse", func(t *testing.T) {
		require.Empty(t, foreignHolderRefusal(reqFor(t, "127.0.0.1:9000")))
	})
}

// daemonLockHeld must never BE the contention: its answer comes from
// /proc/locks, so probing a home leaves that home's own singleton acquire
// free to succeed — a flock probe (even a microsecond LOCK_EX hold) would
// make the probed daemon's start fail "already running" in its window.
func TestDaemonLockProbeIsNonConflicting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the locks table is /proc/locks")
	}
	home := testguard.SocketTempDir(t)

	// Unheld: provably not held — and still acquirable afterwards. If the
	// probe had taken the lock to learn its state this flock would fail.
	pid, held := daemonLockHeld(home)
	require.False(t, held)
	require.Zero(t, pid)
	lock := flockHomeLock(t, home)
	defer func() { _ = lock.Close() }()

	// Held: the probe names the holder without ever holding it itself.
	pid, held = daemonLockHeld(home)
	require.True(t, held)
	require.Equal(t, os.Getpid(), pid)
}

// The #6386 recovery end to end: the contested port is held by OUR OWN home's
// previous daemon — the late-release-across-restart case. Classified own, the
// bind retries (never refuses), and when the predecessor lets go the retry
// binds it — the exact loop the foreign verdict must not break.
func TestOwnHomePredecessorReleaseThenBinds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port-holder identification reads /proc on linux")
	}
	wl, _, myHome := takeoverFixture(t)

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	contested := holder.Addr().String()
	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = contested
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	// The predecessor: OUR home's daemon owns the port — its lock fd is in
	// MY home, so the holder classifies own, not foreign.
	afDaemonHomeFixture(t, myHome)

	oldInit, oldMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = oldInit, oldMax })

	cfg := wl.manager.Config()
	cfg.ListenAddr = contested
	_, _ = wl.reconcile(cfg)
	require.False(t, wl.manager.lifecycle.snapshot().listeners.TCPBound)
	require.Empty(t, wl.manager.lifecycle.snapshot().listeners.TCPRefusalReason,
		"our own predecessor is never a refusal — it is the recovery")
	wl.retryFailedBinds()

	// The predecessor releases on shutdown — the retry must pick the port up.
	require.NoError(t, holder.Close())
	require.Eventually(t, func() bool {
		return wl.manager.lifecycle.snapshot().listeners.TCPBound
	}, 3*time.Second, 10*time.Millisecond,
		"#6386: a port held only by our own home's predecessor must come back to us")
}

// The default-home exemption end to end: OUR home IS the default home, so the
// gate never applies — a foreign daemon holding the port is just another
// EADDRINUSE the #6386 retry out-waits. This is the behaviour the property
// must leave byte-for-byte intact.
func TestDefaultHomeDaemonKeepsRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port-holder identification reads /proc on linux")
	}
	defHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", defHome)
	oldDefaultHome := defaultHomeDir
	defaultHomeDir = func() (string, error) { return defHome, nil }
	t.Cleanup(func() { defaultHomeDir = oldDefaultHome })

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	contested := holder.Addr().String()
	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = contested
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	// A FOREIGN home's daemon holds the contested port.
	foreignHome := testguard.SocketTempDir(t)
	afDaemonHomeFixture(t, foreignHome)

	cfg := config.DefaultConfig()
	cfg.ListenAddr = contested
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })

	oldInit, oldMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = oldInit, oldMax })

	_, _ = wl.reconcile(m.Config())
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound)
	require.Empty(t, m.lifecycle.snapshot().listeners.TCPRefusalReason,
		"the default home's own daemon is exempt — no refusal, just the retry")
	wl.retryFailedBinds()

	require.NoError(t, holder.Close())
	require.Eventually(t, func() bool {
		return m.lifecycle.snapshot().listeners.TCPBound
	}, 3*time.Second, 10*time.Millisecond,
		"the default home's daemon must win its port back — the retry is its documented behaviour")
}

// holderDaemonHome with a readable fd table but NO daemon.lock fd falls
// through to argv+environ — the lock-less (pre-flock or foreign-binary)
// daemon path. Our own pid is the honest fixture: its fd table reads clean,
// holds no lock, and its argv is the test binary — provably not an af daemon.
// A lock-less AF daemon's env cannot be faked for a real pid;
// daemonHomeInProcessFrame's own tests cover what that path does with one.
func TestHolderDaemonHomeFallsBackWithoutLockFD(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd tables read /proc on linux")
	}
	home, isDaemon, provable := holderDaemonHome(os.Getpid())
	require.True(t, provable)
	require.False(t, isDaemon,
		"no lock fd + non-af argv = provably not a daemon — never foreign")
	require.Empty(t, home)
}

func TestRefuseWebForeignLatch(t *testing.T) {
	wl, _, _ := takeoverFixture(t)
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

func TestLatchedRefusalHoldsOnRebind(t *testing.T) {
	wl, _, _ := takeoverFixture(t)

	// Serve a real free port first — the "daemon bound a different port
	// after refusing" state.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serving := l.Addr().String()
	require.NoError(t, l.Close())
	cfg := wl.manager.Config()
	cfg.ListenAddr = serving
	failed, err := wl.reconcile(cfg)
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Equal(t, serving, wl.webBoundAddr)

	// A refusal latched while handleless must survive into the rebind path:
	// re-applying that address reports refused and keeps the healthy
	// listener rather than binding it during the foreign daemon's gap.
	const refused = "127.0.0.1:1"
	const reason = "refusing to bind " + refused + ": held by another AF home"
	wl.mu.Lock()
	wl.foreignRefusals = map[string]string{refused: reason}
	wl.mu.Unlock()

	cfg2 := wl.manager.Config()
	cfg2.ListenAddr = refused
	failed, err = wl.reconcile(cfg2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "held by another AF home")
	require.Contains(t, failed, "network.listen_addr")
	require.Equal(t, serving, wl.webBoundAddr, "a refused rebind keeps the listener it already holds")
	require.NotNil(t, wl.webHandle)
	require.Empty(t, wl.webRefusal, "a kept listener is not the non-serving refusal state")
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

	// A dual-stack [::]:8443 socket lives in /proc/net/tcp6 yet still blocks
	// the v4 request (it owns the v4 wildcard too); [::1]:8443 owns only v6
	// space and cannot collide with 127.0.0.1.
	tcp6 := `  sl  local_address                        rem_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:20FB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 55555 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:20FB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 66666 1 0000000000000000 100 0 0 10 0
`
	inodes = map[uint64]bool{}
	parseProcNetTCP([]byte(tcp6), req, inodes)
	require.Equal(t, map[uint64]bool{55555: true}, inodes)
}

// A dual-stack [::] holder lives ONLY in /proc/net/tcp6 yet still blocks a
// v4 bind — the read must cover both tables, which a fixture /proc proves by
// carrying each family's row in its own file.
func TestProcListenInodesReadsBothTables(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "net"), 0755))
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:20FB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0
`
	tcp6 := `  sl  local_address                        rem_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:20FB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 55555 1 0000000000000000 100 0 0 10 0
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(tcp), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(tcp6), 0644))
	oldRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = oldRoot })

	inodes, complete := procListenInodes(reqFor(t, "127.0.0.1:8443"))
	require.True(t, complete)
	require.Equal(t, map[uint64]bool{12345: true, 55555: true}, inodes,
		"both tables feed the classifier — a dual-stack holder in tcp6 cannot hide from a v4 request")

	// A missing table is "could not look", not "no holder".
	require.NoError(t, os.Remove(filepath.Join(root, "net", "tcp6")))
	_, complete = procListenInodes(reqFor(t, "127.0.0.1:8443"))
	require.False(t, complete)
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
