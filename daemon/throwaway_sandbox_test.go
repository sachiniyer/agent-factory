package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// markedHome writes a valid marker into a fresh temp home.
func markedHome(t *testing.T, mutate func(*throwawaysandbox.Marker)) string {
	t.Helper()
	home := t.TempDir()
	m := &throwawaysandbox.Marker{
		Version:            1,
		Kind:               "playtest",
		CreatedUnix:        time.Now().Unix(),
		MaxLifetimeSeconds: 21600,
	}
	if mutate != nil {
		mutate(m)
	}
	if err := throwawaysandbox.WriteMarker(home, m); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadThrowawayMarker(t *testing.T) {
	t.Run("unmarked home", func(t *testing.T) {
		if m, marked := loadThrowawayMarker(t.TempDir()); marked || m != nil {
			t.Fatal("unmarked home loaded as a sandbox")
		}
	})
	t.Run("marked home", func(t *testing.T) {
		home := markedHome(t, nil)
		m, marked := loadThrowawayMarker(home)
		if !marked || m == nil || m.Kind != "playtest" {
			t.Fatalf("marked=%v marker=%+v", marked, m)
		}
	})
	t.Run("corrupt marker is unmarked", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(throwawaysandbox.MarkerPath(home), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, marked := loadThrowawayMarker(home); marked || m != nil {
			t.Fatal("corrupt marker must never authorize enforcement")
		}
	})
	t.Run("default home marker refused", func(t *testing.T) {
		home := markedHome(t, nil)
		old := throwawayDefaultHomeFn
		throwawayDefaultHomeFn = func() (string, error) { return home, nil }
		defer func() { throwawayDefaultHomeFn = old }()
		if _, marked := loadThrowawayMarker(home); marked {
			t.Fatal("the real install's home accepted a throwaway marker")
		}
	})
}

// ownerDeadCase drives throwawayOwnerDead with injected scope and lookup.
func ownerDeadCase(t *testing.T, m *throwawaysandbox.Marker, lookup func(int) (proctree.Process, error), boot, ns string) bool {
	t.Helper()
	oldLookup, oldScope := throwawayLookupFn, throwawayScopeIDsFn
	throwawayLookupFn = lookup
	throwawayScopeIDsFn = func() (string, string) { return boot, ns }
	defer func() { throwawayLookupFn, throwawayScopeIDsFn = oldLookup, oldScope }()
	return throwawayOwnerDead(m)
}

func TestThrowawayOwnerDead(t *testing.T) {
	owner := &throwawaysandbox.Marker{
		Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60,
		OwnerPID: 42, OwnerStartID: 7, OwnerBootID: "boot-a", OwnerNsID: "ns-a",
	}
	alive := func(int) (proctree.Process, error) { return proctree.Process{PID: 42, StartID: 7}, nil }
	cases := []struct {
		name   string
		marker *throwawaysandbox.Marker
		lookup func(int) (proctree.Process, error)
		boot   string
		ns     string
		dead   bool
	}{
		{"no owner declared", &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60}, alive, "boot-a", "ns-a", false},
		{"owner exited", owner, func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }, "boot-a", "ns-a", true},
		{"owner missing", owner, func(int) (proctree.Process, error) { return proctree.Process{}, syscall.ESRCH }, "boot-a", "ns-a", true},
		{"lookup failure is not death", owner, func(int) (proctree.Process, error) { return proctree.Process{}, errors.New("denied") }, "boot-a", "ns-a", false},
		{"pid recycled", owner, func(int) (proctree.Process, error) { return proctree.Process{PID: 42, StartID: 99}, nil }, "boot-a", "ns-a", true},
		{"same instance alive", owner, alive, "boot-a", "ns-a", false},
		{"different boot is death", owner, alive, "boot-b", "ns-a", true},
		{"pidns fallback is not a boot proof", func() *throwawaysandbox.Marker {
			m := *owner
			m.OwnerBootID = "pidns:1:2"
			return &m
		}(), alive, "pidns:9:9", "ns-a", false},
		{"different namespace is unverifiable", owner, func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }, "boot-a", "ns-b", false},
		{"pid-only owner, alive", func() *throwawaysandbox.Marker {
			m := *owner
			m.OwnerStartID, m.OwnerBootID, m.OwnerNsID = 0, "", ""
			return &m
		}(), alive, "", "", false},
		{"pid-only owner, exited", func() *throwawaysandbox.Marker {
			m := *owner
			m.OwnerStartID, m.OwnerBootID, m.OwnerNsID = 0, "", ""
			return &m
		}(), func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerDeadCase(t, tc.marker, tc.lookup, tc.boot, tc.ns); got != tc.dead {
				t.Fatalf("ownerDead=%v, want %v", got, tc.dead)
			}
		})
	}
}

// sweepFixture builds a registry and a set of marked homes, then returns the
// teardown invocations the sweep produced.
func sweepFixture(t *testing.T, selfHome string, homes map[string]*throwawaysandbox.Marker) (reaped []string) {
	t.Helper()
	// The registry fixture must match the shape WriteRegistryEntry produces:
	// owner-only, never looser — the trust check refuses anything else.
	regDir := filepath.Join(t.TempDir(), "registry")
	if err := os.Mkdir(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldReg, oldNow, oldTeardown := throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn
	oldLookup, oldScope := throwawayLookupFn, throwawayScopeIDsFn
	throwawayRegistryDirFn = func() string { return regDir }
	throwawayNowFn = func() time.Time { return time.Unix(100000, 0) }
	throwawayTeardownFn = func(home string, m *throwawaysandbox.Marker) {
		reaped = append(reaped, home)
	}
	// Any declared owner reads as gone — deterministic orphaning without
	// guessing at a pid that cannot exist on the runner.
	throwawayLookupFn = func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }
	throwawayScopeIDsFn = func() (string, string) { return "", "" }
	defer func() {
		throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn = oldReg, oldNow, oldTeardown
		throwawayLookupFn, throwawayScopeIDsFn = oldLookup, oldScope
	}()
	for home, m := range homes {
		if m != nil {
			if err := throwawaysandbox.WriteMarker(home, m); err != nil {
				t.Fatal(err)
			}
		}
		canon, err := canonicalDir(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := throwawaysandbox.WriteRegistryEntry(regDir, canon); err != nil {
			t.Fatal(err)
		}
	}
	sweepThrowawaySandboxes(selfHome)
	return reaped
}

func TestSweepRequiresOwnMarker(t *testing.T) {
	expired := t.TempDir()
	reaped := sweepFixture(t, t.TempDir(), map[string]*throwawaysandbox.Marker{
		expired: {Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60},
	})
	if len(reaped) != 0 {
		t.Fatalf("an unmarked daemon reaped sandboxes: %v", reaped)
	}
}

func TestSweepReapsExpiredAndOrphaned(t *testing.T) {
	self := markedHome(t, func(m *throwawaysandbox.Marker) { m.CreatedUnix = 99999 })
	expired := t.TempDir()
	young := t.TempDir()
	orphaned := t.TempDir()
	reaped := sweepFixture(t, self, map[string]*throwawaysandbox.Marker{
		self:     {Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60}, // own home: never self-reaped by the sweep
		expired:  {Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60},
		young:    {Version: 1, CreatedUnix: 99900, MaxLifetimeSeconds: 21600},
		orphaned: {Version: 1, CreatedUnix: 99900, MaxLifetimeSeconds: 21600, OwnerPID: 4242, OwnerStartID: 1},
	})
	if len(reaped) != 2 {
		t.Fatalf("reaped %v, want the expired and orphaned homes only", reaped)
	}
	for _, h := range reaped {
		if h == self || h == young {
			t.Fatalf("sweep reaped %s, which it must never touch", h)
		}
	}
}

func TestSweepDropsMissingMarkerEntries(t *testing.T) {
	self := markedHome(t, nil)
	regDir := filepath.Join(t.TempDir(), "registry")
	if err := os.Mkdir(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldReg := throwawayRegistryDirFn
	throwawayRegistryDirFn = func() string { return regDir }
	defer func() { throwawayRegistryDirFn = oldReg }()
	// A registered home with no marker: the contract is void, and the stale
	// registration must not keep resurfacing.
	stale := t.TempDir()
	canon, err := canonicalDir(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := throwawaysandbox.WriteRegistryEntry(regDir, canon); err != nil {
		t.Fatal(err)
	}
	sweepThrowawaySandboxes(self)
	homes, _, err := throwawaysandbox.ReadRegistryHomes(regDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range homes {
		if h == canon {
			t.Fatal("a marker-free home stayed registered")
		}
	}
}

// TestSweepConvergesPastBound is the Codex P2 regression: the sweep bound
// must cap TEARDOWNS, not SCANS — otherwise a retained prefix (live homes,
// missing markers) starves every entry behind it forever. With more
// entries than the cap, a markerless entry anywhere in the set must still
// be dropped and an expired one still reaped, wherever the hash order puts
// them.
func TestSweepConvergesPastBound(t *testing.T) {
	self := markedHome(t, nil)
	regDir := filepath.Join(t.TempDir(), "registry")
	if err := os.Mkdir(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var reaped []string
	oldReg, oldNow, oldTeardown := throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn
	oldLookup, oldScope := throwawayLookupFn, throwawayScopeIDsFn
	throwawayRegistryDirFn = func() string { return regDir }
	throwawayNowFn = func() time.Time { return time.Unix(100000, 0) }
	throwawayTeardownFn = func(home string, m *throwawaysandbox.Marker) { reaped = append(reaped, home) }
	throwawayLookupFn = func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }
	throwawayScopeIDsFn = func() (string, string) { return "", "" }
	defer func() {
		throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn = oldReg, oldNow, oldTeardown
		throwawayLookupFn, throwawayScopeIDsFn = oldLookup, oldScope
	}()
	register := func(home string) {
		canon, err := canonicalDir(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := throwawaysandbox.WriteRegistryEntry(regDir, canon); err != nil {
			t.Fatal(err)
		}
	}
	// throwawaySweepMaxHomes+1 markerless entries: every one must be dropped,
	// so the bound cannot have stopped the scan early.
	for i := 0; i < throwawaySweepMaxHomes+1; i++ {
		register(t.TempDir())
	}
	for i := 0; i < 5; i++ {
		young := markedHome(t, func(m *throwawaysandbox.Marker) {
			m.CreatedUnix = 99900
			m.MaxLifetimeSeconds = 21600
		})
		register(young)
	}
	expired := markedHome(t, func(m *throwawaysandbox.Marker) {
		m.CreatedUnix = 1
		m.MaxLifetimeSeconds = 60
	})
	register(expired)
	sweepThrowawaySandboxes(self)
	expiredCanon, err := canonicalDir(expired)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range reaped {
		if h == expiredCanon {
			found = true
		}
	}
	if !found {
		t.Fatal("an expired entry behind the sweep bound was never examined")
	}
	homes, _, err := throwawaysandbox.ReadRegistryHomes(regDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(homes) != 5 {
		t.Fatalf("%d entries remain registered, want the 5 live homes only: %v", len(homes), homes)
	}
}

func TestProcessBoundToThrowawayHome(t *testing.T) {
	home := t.TempDir()
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	tmuxDir := filepath.Join(home, "tmux")
	canonTmux, err := canonicalDir(tmuxDir)
	if err != nil {
		t.Fatal(err)
	}
	selfUID := os.Getuid()
	envCases := []struct {
		name   string
		env    map[string]string
		bound  bool
		proven bool
	}{
		{"AGENT_FACTORY_HOME match", map[string]string{"AGENT_FACTORY_HOME": home}, true, true},
		{"AF_HOME match", map[string]string{"AF_HOME": home}, true, true},
		{"AF_HOME with symlink spelling", map[string]string{"AF_HOME": home + "/."}, true, true},
		{"different home", map[string]string{"AGENT_FACTORY_HOME": t.TempDir(), "AF_HOME": t.TempDir()}, false, true},
		{"no relevant env", map[string]string{"PATH": "/bin"}, false, true},
		{"tmux_tmpdir match", map[string]string{"TMUX_TMPDIR": tmuxDir}, true, true},
		{"TMUX socket under tmpdir", map[string]string{"TMUX": filepath.Join(tmuxDir, "tmux-x", "default") + ",1,0"}, true, true},
		{"TMUX socket elsewhere", map[string]string{"TMUX": "/elsewhere/default,1,0"}, false, true},
	}
	for _, tc := range envCases {
		t.Run(tc.name, func(t *testing.T) {
			oldUID, oldEnv := throwawayUIDFn, throwawayLookupEnvFn
			throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
			throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
				v, ok := tc.env[key]
				if !ok {
					return "", proctree.EnvAbsent
				}
				return v, proctree.EnvFound
			}
			defer func() { throwawayUIDFn, throwawayLookupEnvFn = oldUID, oldEnv }()
			bound, proven := processBoundToThrowawayHome(1234, canonHome, canonTmux)
			if bound != tc.bound || proven != tc.proven {
				t.Fatalf("bound=%v proven=%v, want %v/%v", bound, proven, tc.bound, tc.proven)
			}
		})
	}
	t.Run("unreadable env is unproven", func(t *testing.T) {
		oldUID, oldEnv := throwawayUIDFn, throwawayLookupEnvFn
		throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
		throwawayLookupEnvFn = func(int, string) (string, proctree.EnvStatus) { return "", proctree.EnvUnknown }
		defer func() { throwawayUIDFn, throwawayLookupEnvFn = oldUID, oldEnv }()
		if bound, proven := processBoundToThrowawayHome(1234, canonHome, canonTmux); bound || proven {
			t.Fatal("an unreadable environment must not be killable")
		}
	})
	t.Run("foreign uid is unproven", func(t *testing.T) {
		oldUID := throwawayUIDFn
		throwawayUIDFn = func(int) (int, bool) { return selfUID + 1, true }
		defer func() { throwawayUIDFn = oldUID }()
		if bound, proven := processBoundToThrowawayHome(1234, canonHome, canonTmux); bound || proven {
			t.Fatal("another user's process must never be bound")
		}
	})
	t.Run("missing tmux marker ignores tmux env", func(t *testing.T) {
		oldUID, oldEnv := throwawayUIDFn, throwawayLookupEnvFn
		throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
		throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
			if key == "TMUX_TMPDIR" {
				return tmuxDir, proctree.EnvFound
			}
			return "", proctree.EnvAbsent
		}
		defer func() { throwawayUIDFn, throwawayLookupEnvFn = oldUID, oldEnv }()
		if bound, _ := processBoundToThrowawayHome(1234, canonHome, ""); bound {
			t.Fatal("tmux binding applied without a recorded tmux dir")
		}
	})
}

// TestProcessBoundResolvesInTargetFrame pins the #6844 review finding: a
// relative AGENT_FACTORY_HOME spells a home only inside the TARGET process's
// cwd — the reaping daemon's own cwd is the marked home, so resolving "." in
// ours would bind every same-uid process carrying a relative value.
func TestProcessBoundResolvesInTargetFrame(t *testing.T) {
	home := t.TempDir()
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	selfUID := os.Getuid()
	cases := []struct {
		name   string
		env    map[string]string
		cwd    string
		cwdOK  bool
		want   bool
		proven bool
	}{
		{"dot in target's home cwd", map[string]string{"AGENT_FACTORY_HOME": "."}, home, true, true, true},
		{"dot in a foreign cwd", map[string]string{"AGENT_FACTORY_HOME": "."}, otherDir, true, false, true},
		{"relative basename in target cwd", map[string]string{"AGENT_FACTORY_HOME": filepath.Base(home)}, filepath.Dir(home), true, true, true},
		{"relative with unreadable cwd", map[string]string{"AGENT_FACTORY_HOME": "."}, "", false, false, true},
		{"tilde against target HOME", map[string]string{"AGENT_FACTORY_HOME": "~", "HOME": home}, "", false, true, true},
		{"tilde-path against target HOME", map[string]string{"AGENT_FACTORY_HOME": "~/" + filepath.Base(home), "HOME": filepath.Dir(home)}, "", false, true, true},
		{"tilde with absent HOME", map[string]string{"AGENT_FACTORY_HOME": "~"}, "", false, false, true},
		{"tilde-user is unresolvable", map[string]string{"AGENT_FACTORY_HOME": "~root/x"}, "", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldUID, oldEnv, oldCwd := throwawayUIDFn, throwawayLookupEnvFn, throwawayWorkingDirFn
			throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
			throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
				v, ok := tc.env[key]
				if !ok {
					return "", proctree.EnvAbsent
				}
				return v, proctree.EnvFound
			}
			throwawayWorkingDirFn = func(int) (string, bool) { return tc.cwd, tc.cwdOK }
			defer func() { throwawayUIDFn, throwawayLookupEnvFn, throwawayWorkingDirFn = oldUID, oldEnv, oldCwd }()
			bound, proven := processBoundToThrowawayHome(1234, canonHome, "")
			if bound != tc.want || proven != tc.proven {
				t.Fatalf("bound=%v proven=%v, want %v/%v", bound, proven, tc.want, tc.proven)
			}
		})
	}
}

// TestProcessBoundRefusesProcfsFrames pins LOW 1 of the independent review:
// an env path that only resolves inside the REAPER's procfs frame —
// /proc/self, /proc/<pid> magic links, or a symlink that reaches one — is
// not evidence of membership. The sigterm_fallback.go guards are reused,
// not re-implemented.
func TestProcessBoundRefusesProcfsFrames(t *testing.T) {
	home := t.TempDir()
	// /proc/self/cwd resolves in the REAPER's frame: with the test chdir'd
	// into the home, the unguarded code would bind "self cwd" to it — the
	// false-positive this test exists to refuse.
	t.Chdir(home)
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	selfUID := os.Getuid()
	link := filepath.Join(t.TempDir(), "p")
	if err := os.Symlink("/proc/self", link); err != nil {
		t.Skip("cannot create symlink fixture")
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"self cwd spelling", map[string]string{"AGENT_FACTORY_HOME": "/proc/self/cwd"}},
		{"numeric magic link", map[string]string{"AGENT_FACTORY_HOME": "/proc/1/cwd/state"}},
		{"thread-self spelling", map[string]string{"AF_HOME": "/proc/thread-self/cwd"}},
		{"dev-fd spelling", map[string]string{"AF_HOME": "/dev/fd/../cwd"}},
		{"symlink into procfs", map[string]string{"AGENT_FACTORY_HOME": filepath.Join(link, "cwd")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldUID, oldEnv := throwawayUIDFn, throwawayLookupEnvFn
			throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
			throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
				v, ok := tc.env[key]
				if !ok {
					return "", proctree.EnvAbsent
				}
				return v, proctree.EnvFound
			}
			defer func() { throwawayUIDFn, throwawayLookupEnvFn = oldUID, oldEnv }()
			if bound, _ := processBoundToThrowawayHome(1234, canonHome, ""); bound {
				t.Fatalf("%s: procfs-relative env bound a process to the home", tc.name)
			}
		})
	}
	// A process under a different root/mount namespace cannot have its paths
	// resolved in this frame at all — it is unproven, never bound.
	t.Run("different process root is unproven", func(t *testing.T) {
		old := throwawaySeams()
		defer throwawayRestoreSeams(old)
		throwawayUIDFn = func(int) (int, bool) { return selfUID, true }
		throwawaySameRootFn = func(int) bool { return false }
		throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
			if key == "AGENT_FACTORY_HOME" {
				return home, proctree.EnvFound
			}
			return "", proctree.EnvAbsent
		}
		if bound, proven := processMemberOfSandbox(1234, proctree.Process{PID: 1234, StartedAt: time.Now()}, canonHome, "", time.Now().Unix()); bound || proven {
			t.Fatal("a process in another root/mount namespace was evaluated as provable")
		}
	})
}

// TestThrowawayBoundProcessesSelectsMembers pins the independent-review
// property: only a process whose OWN evidence places it inside this sandbox
// instance is killed. Descent from a bound parent, a start before the marker
// was written, or identity that moved on are each independently refused —
// and a shared server the sandbox environment happened to spawn is never a
// member.
func TestThrowawayBoundProcessesSelectsMembers(t *testing.T) {
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	tmuxDir := filepath.Join(sandbox, "tmux")
	created := time.Now()
	m := &throwawaysandbox.Marker{
		Version: 1, CreatedUnix: created.Unix(), MaxLifetimeSeconds: 21600,
		TmuxTmpdir: tmuxDir, SandboxRoot: sandbox,
	}
	self := os.Getpid()
	snap := map[int]proctree.Process{
		10:   {PID: 10, PPID: 1, StartID: 100, StartedAt: created},
		11:   {PID: 11, PPID: 10, StartID: 110, StartedAt: created}, // own env — member
		12:   {PID: 12, PPID: 10, StartID: 120, StartedAt: created}, // cleared env: descent is not proof
		20:   {PID: 20, PPID: 1, StartID: 200, StartedAt: created},
		30:   {PID: 30, PPID: 1, StartID: 300, StartedAt: created.Add(-time.Hour)}, // predates the marker
		40:   {PID: 40, PPID: 1, StartID: 400, StartedAt: created},                 // shared tmux server
		50:   {PID: 50, PPID: 1, StartID: 500, StartedAt: created},                 // ssh ControlMaster
		60:   {PID: 60, PPID: 1, StartID: 600, StartedAt: created},                 // recycles under the scan
		70:   {PID: 70, PPID: 1, StartID: 700, StartedAt: created},                 // private tmux server: member
		self: {PID: self, PPID: 10, StartID: 1, StartedAt: created},
	}
	sshSock := filepath.Join(t.TempDir(), "master-x")
	old := throwawaySeams()
	throwawaySnapshotFn = func() (map[int]proctree.Process, error) { return snap, nil }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawayLookupEnvFn = func(pid int, key string) (string, proctree.EnvStatus) {
		if key != "AGENT_FACTORY_HOME" {
			return "", proctree.EnvAbsent
		}
		switch pid {
		case 10, 11, 30, 40, 50, 60, 70, self:
			return home, proctree.EnvFound
		}
		return "", proctree.EnvAbsent
	}
	throwawaySameRootFn = func(int) bool { return true }
	throwawayIsTmuxServerFn = func(pid int) bool { return pid == 40 || pid == 70 }
	throwawayArgvFn = func(pid int) []string {
		if pid == 40 {
			return []string{"tmux", "-S", "/tmp/tmux-1001/default", "new-session"}
		}
		if pid == 70 {
			return []string{"tmux", "-S", filepath.Join(tmuxDir, "drive"), "new-session"}
		}
		return nil
	}
	throwawayUnixSocketPathsFn = func(pid int) ([]string, bool) {
		switch pid {
		case 40:
			return []string{"/tmp/tmux-1001/default"}, true
		case 50:
			return []string{sshSock}, true
		case 70:
			return []string{filepath.Join(tmuxDir, "drive")}, true
		}
		return nil, true
	}
	throwawayLookupFn = func(pid int) (proctree.Process, error) {
		if pid == 60 {
			return proctree.Process{PID: 60, StartID: 999}, nil // recycled
		}
		return snap[pid], nil
	}
	defer throwawayRestoreSeams(old)
	bound, _ := throwawayBoundProcesses(canonHome, m)
	got := map[int]bool{}
	for _, p := range bound {
		got[p.PID] = true
	}
	want := map[int]bool{
		10: true, 11: true, // own-env members
		70:   true,  // private tmux server whose socket is inside the sandbox
		12:   false, // descent alone is not proof
		20:   false, // never bound
		30:   false, // started before the marker — a reused path cannot adopt it
		40:   false, // a shared tmux server is never a member
		50:   false, // a shared server holding a foreign socket is never a member
		60:   false, // identity recycled mid-scan
		self: false,
	}
	for pid, w := range want {
		if got[pid] != w {
			t.Fatalf("pid %d member=%v, want %v (all: %v)", pid, got[pid], w, got)
		}
	}
}

// TestSharedServerSurvivesTeardown is the independent-review regression: a
// server the sandbox environment spawned — carrying its AGENT_FACTORY_HOME —
// but whose socket is NOT the sandbox's private tmux dir must never enter
// the kill set. PID 40 below is the default tmux server; killing it would
// take every production pane with it.
func TestSharedServerSurvivesTeardown(t *testing.T) {
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	m := &throwawaysandbox.Marker{Version: 1, CreatedUnix: time.Now().Unix(), MaxLifetimeSeconds: 60}
	created := time.Now()
	snap := map[int]proctree.Process{
		40: {PID: 40, StartID: 400, StartedAt: created},
	}
	old := throwawaySeams()
	throwawaySnapshotFn = func() (map[int]proctree.Process, error) { return snap, nil }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawayLookupEnvFn = func(_ int, key string) (string, proctree.EnvStatus) {
		if key == "AGENT_FACTORY_HOME" {
			return home, proctree.EnvFound // spawned from the sandbox env — this is the hazard
		}
		return "", proctree.EnvAbsent
	}
	throwawaySameRootFn = func(int) bool { return true }
	throwawayIsTmuxServerFn = func(int) bool { return true }
	throwawayArgvFn = func(int) []string { return []string{"tmux", "new-session", "-d", "-s", "work"} }
	throwawayUnixSocketPathsFn = func(int) ([]string, bool) {
		return []string{"/tmp/tmux-1001/default"}, true // the production default socket
	}
	throwawayLookupFn = func(pid int) (proctree.Process, error) { return snap[pid], nil }
	defer throwawayRestoreSeams(old)
	bound, _ := throwawayBoundProcesses(canonHome, m)
	for _, p := range bound {
		if p.PID == 40 {
			t.Fatal("the default tmux server was selected for teardown — its production panes would die with it")
		}
	}
}

// unixSock creates a real socket file under dir so WalkDir's ModeSocket
// check sees it.
func unixSock(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "default")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return path
}

func TestKillThrowawayTmuxSocketsGuardsSharedRoots(t *testing.T) {
	var killed []string
	oldKill := throwawayKillSocketFn
	throwawayKillSocketFn = func(sock string) error {
		killed = append(killed, sock)
		return nil
	}
	defer func() { throwawayKillSocketFn = oldKill }()
	marker := func(root, tmuxDir string) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60, TmuxTmpdir: tmuxDir, SandboxRoot: root}
	}
	// A private dir under the sandbox root: the socket is killed. The marked
	// home sits inside the root, the shape our own scaffolding produces.
	sandbox := t.TempDir()
	canonSandbox, err := canonicalDir(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	good := unixSock(t, filepath.Join(sandbox, "tmux", "tmux-"+strconv.Itoa(os.Getuid())))
	killThrowawayTmuxSockets(canonSandbox, marker(sandbox, filepath.Join(sandbox, "tmux")))
	if len(killed) != 1 || killed[0] != good {
		t.Fatalf("killed %v, want only %s", killed, good)
	}
	killed = nil
	// Every out-of-root or shared-root shape is refused.
	shared := unixSock(t, filepath.Join(os.TempDir(), "af-sbxtest-"+strconv.Itoa(os.Getpid())))
	killThrowawayTmuxSockets(canonSandbox, marker(os.TempDir(), filepath.Dir(shared)))
	if len(killed) != 0 {
		t.Fatalf("a shared-root marker killed sockets: %v", killed)
	}
	outside := unixSock(t, t.TempDir())
	killThrowawayTmuxSockets(canonSandbox, marker(sandbox, filepath.Dir(outside)))
	if len(killed) != 0 {
		t.Fatalf("an out-of-root marker killed sockets: %v", killed)
	}
	killThrowawayTmuxSockets(canonSandbox, marker(sandbox, sandbox))
	if len(killed) != 0 {
		t.Fatal("tmux dir equal to the sandbox root was not refused")
	}
	// A forged marker naming a root that does not contain its own home must
	// not reach anyone else's tmux dir.
	forgedRoot := t.TempDir()
	forgedSock := unixSock(t, filepath.Join(forgedRoot, "tmux"))
	killThrowawayTmuxSockets(canonSandbox, marker(forgedRoot, filepath.Dir(forgedSock)))
	if len(killed) != 0 {
		t.Fatalf("a marker claiming an unrelated root killed sockets: %v", killed)
	}
}

func TestThrowawayPrivateTmuxDir(t *testing.T) {
	sandbox := t.TempDir()
	canonSandbox, err := canonicalDir(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	marker := func(root, tmuxDir string) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60, TmuxTmpdir: tmuxDir, SandboxRoot: root}
	}
	if dir, refuse := throwawayPrivateTmuxDir(canonSandbox, marker(sandbox, filepath.Join(sandbox, "tmux"))); dir == "" || refuse != "" {
		t.Fatalf("private dir refused: dir=%q refuse=%q", dir, refuse)
	}
	for _, tc := range []struct {
		name    string
		root    string
		tmuxDir string
	}{
		{"shared root", os.TempDir(), filepath.Join(os.TempDir(), "x")},
		{"home outside root", t.TempDir(), filepath.Join(t.TempDir(), "y")},
		{"tmux outside root", sandbox, filepath.Join(t.TempDir(), "z")},
		{"tmux equals root", sandbox, sandbox},
		{"missing tmux", sandbox, ""},
		{"missing root", "", filepath.Join(sandbox, "tmux")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if dir, _ := throwawayPrivateTmuxDir(canonSandbox, marker(tc.root, tc.tmuxDir)); dir != "" {
				t.Fatalf("untrusted dir accepted: %q", dir)
			}
		})
	}
	// A recorded shared dir must not feed process selection either: the
	// binding path consults the same proof.
	m := marker(os.TempDir(), os.TempDir())
	if dir, _ := throwawayPrivateTmuxDir(canonSandbox, m); dir != "" {
		t.Fatalf("shared tmux dir %q accepted for binding", dir)
	}
}

// TestThrowawayPrivateTmuxDirIsAnAllowlist is the independent-review
// regression: the privacy proof must not depend on the reaper's own TMPDIR.
// With TMPDIR=/var/tmp the old two-entry denylist let root=/tmp through,
// aiming tmux kill-server at the production server on this box.
func TestThrowawayPrivateTmuxDirIsAnAllowlist(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	marker := func(root, tmuxDir string) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60, TmuxTmpdir: tmuxDir, SandboxRoot: root}
	}
	for _, tmpdir := range []string{"/tmp", "/var/tmp"} {
		t.Run("TMPDIR="+tmpdir, func(t *testing.T) {
			t.Setenv("TMPDIR", tmpdir)
			t.Setenv("TMUX_TMPDIR", "")
			for _, tc := range []struct {
				name    string
				root    string
				tmuxDir string
			}{
				{"the review's repro", "/tmp", "/tmp/tmux-" + uid},
				{"root is the other tmp", "/var/tmp", "/var/tmp/x"},
				{"tmp dir is a real socket dir", sandbox, "/tmp/tmux-" + uid},
				{"tmp dir is an ancestor of a real socket dir", "/tmp", "/tmp"},
				{"tmp dir is the user tmux dir", sandbox, filepath.Join(homeDir, ".tmux")},
				{"tmp dir is the user home", sandbox, homeDir},
				{"root is the user home", homeDir, filepath.Join(homeDir, "x")},
				{"root is /", "/", "/tmux"},
			} {
				if dir, refuse := throwawayPrivateTmuxDir(canonHome, marker(tc.root, tc.tmuxDir)); dir != "" {
					t.Fatalf("%s: shared shape accepted dir=%q (refuse=%q)", tc.name, dir, refuse)
				}
			}
			// The sandbox-created shape is still the one case allowed.
			if dir, refuse := throwawayPrivateTmuxDir(canonHome, marker(sandbox, filepath.Join(sandbox, "tmux"))); dir == "" {
				t.Fatalf("sandbox-created tmux dir refused: %q", refuse)
			}
		})
	}
}

// TestThrowawayPrivateTmuxDirInheritedEnv is the Codex P2 regression: a
// marked daemon INHERITS the sandbox's private TMUX_TMPDIR, so the reaper's
// own environment must not make that dir count as shared. The exemption is
// narrow — only when the inherited dir IS the one the marker recorded —
// so an env dir the marker does not claim stays shared.
func TestThrowawayPrivateTmuxDirInheritedEnv(t *testing.T) {
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	tmuxDir := filepath.Join(sandbox, "tmux")
	marker := func(root, tmux string) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 60, TmuxTmpdir: tmux, SandboxRoot: root}
	}
	t.Run("inherited private dir is allowed", func(t *testing.T) {
		t.Setenv("TMUX_TMPDIR", tmuxDir)
		if dir, refuse := throwawayPrivateTmuxDir(canonHome, marker(sandbox, tmuxDir)); dir == "" {
			t.Fatalf("the sandbox's own inherited tmux dir refused: %q", refuse)
		}
	})
	t.Run("inherited dir the marker does not claim stays shared", func(t *testing.T) {
		// env names a DIFFERENT dir inside the same root: the exemption
		// must not leak to a dir the marker never recorded.
		other := filepath.Join(sandbox, "other")
		t.Setenv("TMUX_TMPDIR", other)
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(sandbox, tmuxDir)); dir != "" {
			t.Fatal("an env dir inside the root but not claimed exempted the root anyway")
		}
	})
	t.Run("claimed env dir outside any root still refused", func(t *testing.T) {
		// Exemption only rescues the shared check; containment still applies.
		real := filepath.Join(t.TempDir(), "real-tmux")
		t.Setenv("TMUX_TMPDIR", real)
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(sandbox, real)); dir != "" {
			t.Fatal("an env dir outside the sandbox root was exempted anyway")
		}
	})
}

func TestThrowawayWatchdogDeadlineFires(t *testing.T) {
	m := &throwawaysandbox.Marker{Version: 1, CreatedUnix: time.Now().Unix() - 10, MaxLifetimeSeconds: 5}
	fire := make(chan struct{})
	stop := armThrowawayWatchdog(m, fire)
	defer stop()
	select {
	case <-fire:
	case <-time.After(5 * time.Second):
		t.Fatal("an expired marker did not fire the watchdog")
	}
}

func TestThrowawayWatchdogOwnerDeathFires(t *testing.T) {
	oldLookup, oldScope, oldInterval := throwawayLookupFn, throwawayScopeIDsFn, throwawayCheckInterval
	throwawayLookupFn = func(int) (proctree.Process, error) { return proctree.Process{}, proctree.ErrProcessExited }
	throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
	throwawayCheckInterval = 10 * time.Millisecond
	defer func() {
		throwawayLookupFn, throwawayScopeIDsFn, throwawayCheckInterval = oldLookup, oldScope, oldInterval
	}()
	m := &throwawaysandbox.Marker{
		Version: 1, CreatedUnix: time.Now().Unix(), MaxLifetimeSeconds: 3600,
		OwnerPID: 42, OwnerStartID: 7,
	}
	fire := make(chan struct{})
	stop := armThrowawayWatchdog(m, fire)
	defer stop()
	select {
	case <-fire:
	case <-time.After(5 * time.Second):
		t.Fatal("a dead declared owner did not fire the watchdog")
	}
}

func TestThrowawayWatchdogStopsCleanly(t *testing.T) {
	m := &throwawaysandbox.Marker{Version: 1, CreatedUnix: time.Now().Unix(), MaxLifetimeSeconds: 3600}
	fire := make(chan struct{})
	stop := armThrowawayWatchdog(m, fire)
	stop()
	select {
	case <-fire:
		t.Fatal("a stopped watchdog fired")
	case <-time.After(50 * time.Millisecond):
	}
}

// throwawaySeams snapshots every test seam this file pokes so a single defer
// restores the production wiring. Add new seams here, not ad-hoc.
type throwawaySeamSet struct {
	snapshot  func() (map[int]proctree.Process, error)
	uid       func(int) (int, bool)
	env       func(int, string) (string, proctree.EnvStatus)
	cwd       func(int) (string, bool)
	lookup    func(int) (proctree.Process, error)
	sameRoot  func(int) bool
	tmuxSrv   func(int) bool
	argv      func(int) []string
	unixSocks func(int) ([]string, bool)
}

func throwawaySeams() throwawaySeamSet {
	return throwawaySeamSet{
		snapshot: throwawaySnapshotFn, uid: throwawayUIDFn, env: throwawayLookupEnvFn,
		cwd: throwawayWorkingDirFn, lookup: throwawayLookupFn, sameRoot: throwawaySameRootFn,
		tmuxSrv: throwawayIsTmuxServerFn, argv: throwawayArgvFn, unixSocks: throwawayUnixSocketPathsFn,
	}
}

func throwawayRestoreSeams(s throwawaySeamSet) {
	throwawaySnapshotFn, throwawayUIDFn, throwawayLookupEnvFn, throwawayWorkingDirFn = s.snapshot, s.uid, s.env, s.cwd
	throwawayLookupFn, throwawaySameRootFn = s.lookup, s.sameRoot
	throwawayIsTmuxServerFn, throwawayArgvFn, throwawayUnixSocketPathsFn = s.tmuxSrv, s.argv, s.unixSocks
}
