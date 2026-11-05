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

// scopedPID writes a recorded-pid entry scoped to the boot/namespace pair
// the test's throwawayScopeIDsFn stub returns — recorded pairs are only
// selectable inside the scope they were written in.
func scopedPID(t *testing.T, home string, pid int, startID uint64, boot, ns string) {
	t.Helper()
	err := throwawaysandbox.WriteRecordedPID(home, throwawaysandbox.RecordedPID{
		PID: pid, StartID: startID, BootID: boot, NSID: ns,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sandboxTreeFixture builds the shape the scaffolding creates: a fresh
// root holding the AF home and the private tmux dir, stamped with the
// creation sentinel bound to createdUnix. It returns the root, the
// canonical home (what the reaper compares against), and the tmux dir.
func sandboxTreeFixture(t *testing.T, createdUnix int64) (root, canonHome, tmuxDir string) {
	t.Helper()
	root = t.TempDir()
	home := filepath.Join(root, "af-home")
	tmuxDir = filepath.Join(root, "tmux")
	for _, d := range []string{home, tmuxDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := throwawaysandbox.WriteRootSentinel(root, createdUnix); err != nil {
		t.Fatal(err)
	}
	canonHome, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}
	return root, canonHome, tmuxDir
}

// TestThrowawayPrivateTmuxDirRequiresCreatedRoot pins the round-2 review:
// the tmux dir is valid only when the recorded sandbox root carries proof
// the sandbox itself made the tree — a sentinel whose content is THIS
// marker's created_unix. There is no denylist: $HOME/.cache is refused
// because no sentinel proves this sandbox created it, not because the
// path is on a list.
func TestThrowawayPrivateTmuxDirRequiresCreatedRoot(t *testing.T) {
	created := int64(1700000000)
	root, canonHome, tmuxDir := sandboxTreeFixture(t, created)
	marker := func(rootDir, tmux string, cu int64) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: cu, MaxLifetimeSeconds: 60, TmuxTmpdir: tmux, SandboxRoot: rootDir}
	}
	if dir, refuse := throwawayPrivateTmuxDir(canonHome, marker(root, tmuxDir, created)); dir == "" {
		t.Fatalf("a sandbox-created dir was refused: %q", refuse)
	}
	t.Run("root=$HOME/.cache is refused", func(t *testing.T) {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skip(err)
		}
		cache := filepath.Join(userHome, ".cache")
		if dir, refuse := throwawayPrivateTmuxDir(canonHome, marker(cache, filepath.Join(cache, "tmux"), created)); dir != "" {
			t.Fatalf("$HOME/.cache accepted as a sandbox root: %q", dir)
		} else if refuse == "" {
			t.Fatal("$HOME/.cache was silently ignored rather than refused")
		}
	})
	t.Run("a fresh dir with no sentinel is refused", func(t *testing.T) {
		bare := t.TempDir()
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(bare, filepath.Join(bare, "tmux"), created)); dir != "" {
			t.Fatalf("a sentinel-free root was accepted: %q", dir)
		}
	})
	t.Run("a sentinel for a different instance is refused", func(t *testing.T) {
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(root, tmuxDir, created+1)); dir != "" {
			t.Fatalf("a sentinel bound to another marker was accepted: %q", dir)
		}
	})
	t.Run("a symlinked sentinel is refused", func(t *testing.T) {
		other := t.TempDir()
		realSentinel := filepath.Join(other, "sentinel")
		if err := os.WriteFile(realSentinel, []byte("1700000000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		root2 := t.TempDir()
		if err := os.Symlink(realSentinel, filepath.Join(root2, throwawaysandbox.RootSentinelName)); err != nil {
			t.Skip("cannot create symlink fixture")
		}
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(root2, filepath.Join(root2, "tmux"), created)); dir != "" {
			t.Fatalf("a symlinked sentinel was accepted: %q", dir)
		}
	})
	t.Run("a symlinked root is refused", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "rootlink")
		if err := os.Symlink(root, link); err != nil {
			t.Skip("cannot create symlink fixture")
		}
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(link, tmuxDir, created)); dir != "" {
			t.Fatalf("a symlinked sandbox root was accepted: %q", dir)
		}
	})
	t.Run("a file recorded as the root is refused", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(f, tmuxDir, created)); dir != "" {
			t.Fatalf("a non-directory sandbox root was accepted: %q", dir)
		}
	})
	t.Run("home outside the proven root is refused", func(t *testing.T) {
		other := t.TempDir()
		if err := throwawaysandbox.WriteRootSentinel(other, created); err != nil {
			t.Fatal(err)
		}
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(other, filepath.Join(other, "tmux"), created)); dir != "" {
			t.Fatalf("a proven root that does not contain the home was accepted: %q", dir)
		}
	})
	t.Run("tmux dir outside the proven root is refused", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "tmux")
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(root, outside, created)); dir != "" {
			t.Fatalf("a tmux dir outside the proven root was accepted: %q", dir)
		}
	})
	t.Run("tmux dir equal to the root is refused", func(t *testing.T) {
		if dir, _ := throwawayPrivateTmuxDir(canonHome, marker(root, root, created)); dir != "" {
			t.Fatalf("the root itself was accepted as a tmux dir: %q", dir)
		}
	})
	t.Run("unrecorded metadata is a no-op", func(t *testing.T) {
		if dir, refuse := throwawayPrivateTmuxDir(canonHome, &throwawaysandbox.Marker{Version: 1, CreatedUnix: created, MaxLifetimeSeconds: 60}); dir != "" || refuse != "" {
			t.Fatalf("empty metadata returned dir=%q refuse=%q", dir, refuse)
		}
	})
}

// TestThrowawayMembersServerTreeAndRecorded pins the structural model the
// round-2 review requires: membership is proven by the private tmux
// server's own kernel-reported socket — never by environment — or by a
// (pid, start id) the sandbox recorded at spawn time. Descent counts only
// UNDER the proven server; everything else is left alone.
func TestThrowawayMembersServerTreeAndRecorded(t *testing.T) {
	created := time.Now().Add(-time.Minute).Unix()
	root, canonHome, tmuxDir := sandboxTreeFixture(t, created)
	m := &throwawaysandbox.Marker{
		Version: 1, CreatedUnix: created, MaxLifetimeSeconds: 21600,
		TmuxTmpdir: tmuxDir, SandboxRoot: root,
	}
	t0 := time.Unix(created, 0)
	self := os.Getpid()
	snap := map[int]proctree.Process{
		40:   {PID: 40, PPID: 1, SID: 40, StartID: 400, StartedAt: t0},  // shared default server
		41:   {PID: 41, PPID: 40, SID: 41, StartID: 410, StartedAt: t0}, // production pane under it
		70:   {PID: 70, PPID: 1, SID: 70, StartID: 700, StartedAt: t0},  // private server
		71:   {PID: 71, PPID: 70, SID: 71, StartID: 710, StartedAt: t0}, // pane session leader
		72:   {PID: 72, PPID: 71, SID: 71, StartID: 720, StartedAt: t0}, // grandchild in the pane
		73:   {PID: 73, PPID: 1, SID: 71, StartID: 730, StartedAt: t0},  // reparented stray, pane session
		80:   {PID: 80, PPID: 1, SID: 80, StartID: 800, StartedAt: t0},  // foreign same-uid process
		90:   {PID: 90, PPID: 1, SID: 90, StartID: 900, StartedAt: t0},  // recorded daemon
		91:   {PID: 91, PPID: 1, SID: 91, StartID: 910, StartedAt: t0},  // recorded pid, recycled instance
		self: {PID: self, PPID: 1, SID: 99, StartID: 1, StartedAt: t0},
	}
	old := throwawaySeams()
	defer throwawayRestoreSeams(old)
	throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
	scopedPID(t, canonHome, 90, 900, "b", "n")
	scopedPID(t, canonHome, 91, 910, "b", "n")
	scopedPID(t, canonHome, 92, 920, "b", "n")
	throwawaySnapshotFn = func() (map[int]proctree.Process, error) { return snap, nil }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawaySameRootFn = func(int) bool { return true }
	throwawayIsTmuxServerFn = func(pid int) bool { return pid == 40 || pid == 70 }
	throwawayUnixSocketPathsFn = func(pid int) ([]string, bool) {
		switch pid {
		case 40:
			return []string{"/tmp/tmux-" + strconv.Itoa(os.Getuid()) + "/default"}, true
		case 70:
			return []string{filepath.Join(tmuxDir, "drive")}, true
		}
		return nil, true
	}
	throwawayLookupFn = func(pid int) (proctree.Process, error) {
		if pid == 91 {
			return proctree.Process{PID: 91, StartID: 999}, nil // recycled: not the recorded instance
		}
		p, ok := snap[pid]
		if !ok {
			return proctree.Process{}, os.ErrNotExist
		}
		return p, nil
	}
	got := map[int]bool{}
	for _, p := range throwawayMemberProcesses(canonHome, m) {
		got[p.PID] = true
	}
	want := map[int]bool{
		70: true, 71: true, 72: true, 73: true, // the proven server and everything under it
		90:   true,  // the recorded daemon
		40:   false, // the shared server is never a member
		41:   false, // nor the production pane under it
		80:   false, // never a member
		91:   false, // recorded but recycled — a different instance owns the pid
		92:   false, // recorded but dead
		self: false, // the reaper excludes itself
	}
	for pid, w := range want {
		if got[pid] != w {
			t.Fatalf("pid %d member=%v, want %v (all: %v)", pid, got[pid], w, got)
		}
	}
}

// TestSharedServerPaneSurvivesTeardown is the round-2 MEDIUM regression: a
// production-style pane on the shared default server — the process whose
// inherited AGENT_FACTORY_HOME the old env-binding selected — must never
// be a member. The marker records NO private tmux dir, so only recorded
// pids could ever be selected, and there are none.
func TestSharedServerPaneSurvivesTeardown(t *testing.T) {
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
	t0 := time.Now()
	snap := map[int]proctree.Process{
		40: {PID: 40, PPID: 1, SID: 40, StartID: 400, StartedAt: t0},  // shared default server
		41: {PID: 41, PPID: 40, SID: 41, StartID: 410, StartedAt: t0}, // its pane
	}
	old := throwawaySeams()
	defer throwawayRestoreSeams(old)
	throwawaySnapshotFn = func() (map[int]proctree.Process, error) { return snap, nil }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawaySameRootFn = func(int) bool { return true }
	throwawayIsTmuxServerFn = func(int) bool { return true }
	throwawayUnixSocketPathsFn = func(int) ([]string, bool) {
		return []string{"/tmp/tmux-" + strconv.Itoa(os.Getuid()) + "/default"}, true
	}
	throwawayLookupFn = func(pid int) (proctree.Process, error) { return snap[pid], nil }
	if got := throwawayMemberProcesses(canonHome, m); len(got) != 0 {
		t.Fatalf("shared-server processes were selected for teardown: %v", got)
	}
}

// TestThrowawayMembersNoSocketEvidence is the round-2 off-Linux
// regression: where the kernel cannot report a process's sockets, a tmux
// server can never be proven private — so only recorded (pid, start id)
// pairs may ever be selected. ok=false must fail CLOSED.
func TestThrowawayMembersNoSocketEvidence(t *testing.T) {
	created := time.Now().Add(-time.Minute).Unix()
	root, canonHome, tmuxDir := sandboxTreeFixture(t, created)
	m := &throwawaysandbox.Marker{
		Version: 1, CreatedUnix: created, MaxLifetimeSeconds: 21600,
		TmuxTmpdir: tmuxDir, SandboxRoot: root,
	}
	t0 := time.Unix(created, 0)
	snap := map[int]proctree.Process{
		70: {PID: 70, PPID: 1, SID: 70, StartID: 700, StartedAt: t0}, // private server — unprovable here
		71: {PID: 71, PPID: 70, SID: 71, StartID: 710, StartedAt: t0},
		90: {PID: 90, PPID: 1, SID: 90, StartID: 900, StartedAt: t0}, // recorded daemon
	}
	old := throwawaySeams()
	defer throwawayRestoreSeams(old)
	throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
	scopedPID(t, canonHome, 90, 900, "b", "n")
	throwawaySnapshotFn = func() (map[int]proctree.Process, error) { return snap, nil }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawaySameRootFn = func(int) bool { return true }
	throwawayIsTmuxServerFn = func(pid int) bool { return pid == 70 }
	throwawayUnixSocketPathsFn = func(int) ([]string, bool) { return nil, false }
	throwawayLookupFn = func(pid int) (proctree.Process, error) { return snap[pid], nil }
	got := map[int]bool{}
	for _, p := range throwawayMemberProcesses(canonHome, m) {
		got[p.PID] = true
	}
	if got[70] || got[71] {
		t.Fatalf("a server was proven without socket evidence: %v", got)
	}
	if !got[90] {
		t.Fatal("the recorded daemon was not selected")
	}
}

// TestThrowawayRecordedMembersRequireScope pins the lift-item rule: a
// recorded (pid, start id) is tied to the boot and PID namespace it was
// written in — proctree's rule for persisted kill handles. An unscoped
// record, a record from another namespace (where a lookup hit would be a
// coincidental collision), or a strong-boot-mismatched record is never
// selected; only a pidns-fallback mismatch still falls through to the
// live StartID check.
func TestThrowawayRecordedMembersRequireScope(t *testing.T) {
	home := t.TempDir()
	snap := map[int]proctree.Process{
		90: {PID: 90, PPID: 1, SID: 90, StartID: 900},
	}
	old := throwawaySeams()
	defer throwawayRestoreSeams(old)
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	throwawaySameRootFn = func(int) bool { return true }
	throwawayLookupFn = func(pid int) (proctree.Process, error) {
		p, ok := snap[pid]
		if !ok {
			return proctree.Process{}, os.ErrNotExist
		}
		return p, nil
	}
	selected := func() bool {
		for _, p := range throwawayRecordedMembers(home) {
			if p.PID == 90 {
				return true
			}
		}
		return false
	}
	write := func(rec throwawaysandbox.RecordedPID) {
		if err := throwawaysandbox.WriteRecordedPID(home, rec); err != nil {
			t.Fatal(err)
		}
	}
	matching := throwawaysandbox.RecordedPID{PID: 90, StartID: 900, BootID: "b", NSID: "n"}

	write(matching)
	throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
	if !selected() {
		t.Fatal("a record inside its own scope was not selected")
	}
	if err := os.Remove(throwawaysandbox.RecordedPIDsPath(home)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		rec     throwawaysandbox.RecordedPID
		curBoot string
		curNs   string
	}{
		{"other pid namespace", throwawaysandbox.RecordedPID{PID: 90, StartID: 900, BootID: "b", NSID: "other-ns"}, "b", "n"},
		{"strong boot mismatch", throwawaysandbox.RecordedPID{PID: 90, StartID: 900, BootID: "old-boot", NSID: "n"}, "b", "n"},
		{"unscoped record", throwawaysandbox.RecordedPID{PID: 90, StartID: 900}, "b", "n"},
		{"unreadable current scope", matching, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.rec)
			throwawayScopeIDsFn = func() (string, string) { return tc.curBoot, tc.curNs }
			if selected() {
				t.Fatal("an out-of-scope record was selected")
			}
			if err := os.Remove(throwawaysandbox.RecordedPIDsPath(home)); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("pidns fallback record falls through to the live check", func(t *testing.T) {
		write(throwawaysandbox.RecordedPID{PID: 90, StartID: 900, BootID: "pidns:1:2", NSID: "n"})
		throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
		if !selected() {
			t.Fatal("a fallback-booted record with a matching live instance was not selected")
		}
		if err := os.Remove(throwawaysandbox.RecordedPIDsPath(home)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRecordThrowawayDaemonScopesRecords pins that the daemon writes its
// own record WITH the boot/namespace scope — an unscoped record is never
// written, because it could never be selected anyway.
func TestRecordThrowawayDaemonScopesRecords(t *testing.T) {
	home := t.TempDir()
	oldLookup, oldScope := throwawayLookupFn, throwawayScopeIDsFn
	defer func() { throwawayLookupFn, throwawayScopeIDsFn = oldLookup, oldScope }()
	throwawayLookupFn = func(pid int) (proctree.Process, error) {
		return proctree.Process{PID: pid, StartID: 4242}, nil
	}
	throwawayScopeIDsFn = func() (string, string) { return "b", "n" }
	recordThrowawayDaemon(home)
	recs, err := throwawaysandbox.ReadRecordedPIDs(home)
	if err != nil {
		t.Fatalf("the daemon record was not written: %v", err)
	}
	if len(recs) != 1 || recs[0].PID != os.Getpid() || recs[0].StartID != 4242 || recs[0].BootID != "b" || recs[0].NSID != "n" {
		t.Fatalf("daemon record %+v lacks the full scoped identity", recs)
	}
	if err := os.Remove(throwawaysandbox.RecordedPIDsPath(home)); err != nil {
		t.Fatal(err)
	}
	throwawayScopeIDsFn = func() (string, string) { return "", "n" }
	recordThrowawayDaemon(home)
	if _, err := os.Lstat(throwawaysandbox.RecordedPIDsPath(home)); !os.IsNotExist(err) {
		t.Fatal("an unscoped daemon record was written")
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

// TestKillThrowawayTmuxSocketsRequiresCreatedRoot pins that the tmux
// socket teardown runs only inside a root the sentinel proves THIS marker
// created — no sentinel, no socket kills, whatever the recorded paths say.
func TestKillThrowawayTmuxSocketsRequiresCreatedRoot(t *testing.T) {
	var killed []string
	oldKill := throwawayKillSocketFn
	throwawayKillSocketFn = func(sock string) error {
		killed = append(killed, sock)
		return nil
	}
	defer func() { throwawayKillSocketFn = oldKill }()
	created := int64(1700000000)
	root, canonHome, tmuxDir := sandboxTreeFixture(t, created)
	marker := func(rootDir, tmux string) *throwawaysandbox.Marker {
		return &throwawaysandbox.Marker{Version: 1, CreatedUnix: created, MaxLifetimeSeconds: 60, TmuxTmpdir: tmux, SandboxRoot: rootDir}
	}
	// A private dir under the proven root: the socket is killed.
	good := unixSock(t, filepath.Join(tmuxDir, "tmux-"+strconv.Itoa(os.Getuid())))
	killThrowawayTmuxSockets(canonHome, marker(root, tmuxDir))
	if len(killed) != 1 || killed[0] != good {
		t.Fatalf("killed %v, want only %s", killed, good)
	}
	killed = nil
	// Every unproven shape is refused — a shared root without a sentinel, a
	// dir outside the proven root, the root itself, and a DIFFERENT proven
	// root that does not contain this home.
	shared := unixSock(t, filepath.Join(os.TempDir(), "af-sbxtest-"+strconv.Itoa(os.Getpid())))
	killThrowawayTmuxSockets(canonHome, marker(os.TempDir(), filepath.Dir(shared)))
	if len(killed) != 0 {
		t.Fatalf("a sentinel-free root's sockets were killed: %v", killed)
	}
	outside := unixSock(t, t.TempDir())
	killThrowawayTmuxSockets(canonHome, marker(root, filepath.Dir(outside)))
	if len(killed) != 0 {
		t.Fatalf("an out-of-root marker killed sockets: %v", killed)
	}
	killThrowawayTmuxSockets(canonHome, marker(root, root))
	if len(killed) != 0 {
		t.Fatal("tmux dir equal to the sandbox root was not refused")
	}
	otherRoot, _, otherTmux := sandboxTreeFixture(t, created)
	forgedSock := unixSock(t, otherTmux)
	killThrowawayTmuxSockets(canonHome, marker(otherRoot, filepath.Dir(forgedSock)))
	if len(killed) != 0 {
		t.Fatalf("a proven root that does not contain this home killed sockets: %v", killed)
	}
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
	lookup    func(int) (proctree.Process, error)
	sameRoot  func(int) bool
	tmuxSrv   func(int) bool
	unixSocks func(int) ([]string, bool)
	scope     func() (string, string)
}

func throwawaySeams() throwawaySeamSet {
	return throwawaySeamSet{
		snapshot: throwawaySnapshotFn, uid: throwawayUIDFn, lookup: throwawayLookupFn,
		sameRoot: throwawaySameRootFn, tmuxSrv: throwawayIsTmuxServerFn,
		unixSocks: throwawayUnixSocketPathsFn, scope: throwawayScopeIDsFn,
	}
}

func throwawayRestoreSeams(s throwawaySeamSet) {
	throwawaySnapshotFn, throwawayUIDFn, throwawayLookupFn = s.snapshot, s.uid, s.lookup
	throwawaySameRootFn, throwawayIsTmuxServerFn, throwawayUnixSocketPathsFn = s.sameRoot, s.tmuxSrv, s.unixSocks
	throwawayScopeIDsFn = s.scope
}
