package throwawaysandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validMarker() *Marker {
	return &Marker{Version: 1, Kind: "playtest", CreatedUnix: 1000, MaxLifetimeSeconds: 21600}
}

func TestMarkerValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Marker)
		want   bool
	}{
		{"valid minimal", func(*Marker) {}, true},
		{"valid owner", func(m *Marker) { m.OwnerPID = 42; m.OwnerStartID = 7; m.OwnerBootID = "boot"; m.OwnerNsID = "ns" }, true},
		{"bad version", func(m *Marker) { m.Version = 2 }, false},
		{"zero version", func(m *Marker) { m.Version = 0 }, false},
		{"missing created", func(m *Marker) { m.CreatedUnix = 0 }, false},
		{"negative created", func(m *Marker) { m.CreatedUnix = -5 }, false},
		{"lifetime too small", func(m *Marker) { m.MaxLifetimeSeconds = 0 }, false},
		{"lifetime too large", func(m *Marker) { m.MaxLifetimeSeconds = MaxLifetimeSeconds + 1 }, false},
		{"lifetime boundary", func(m *Marker) { m.MaxLifetimeSeconds = MaxLifetimeSeconds }, true},
		{"lifetime one", func(m *Marker) { m.MaxLifetimeSeconds = 1 }, true},
		{"negative owner", func(m *Marker) { m.OwnerPID = -1 }, false},
		{"startid without pid", func(m *Marker) { m.OwnerStartID = 9 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMarker()
			tc.mutate(m)
			if err := m.Validate(); (err == nil) != tc.want {
				t.Fatalf("Validate() err=%v, want valid=%v", err, tc.want)
			}
		})
	}
}

func TestMarkerExpiry(t *testing.T) {
	m := validMarker() // created=1000, lifetime=21600 → deadline=22600
	for _, tc := range []struct {
		name    string
		at      int64
		expired bool
	}{
		{"young", 2000, false},
		{"before deadline", 22599, false},
		{"at deadline", 22600, true},
		{"after deadline", 30000, true},
		{"before creation (clock skew)", 999, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.Expired(time.Unix(tc.at, 0)); got != tc.expired {
				t.Fatalf("Expired(%d)=%v, want %v", tc.at, got, tc.expired)
			}
		})
	}
}

func TestLoadMarker(t *testing.T) {
	home := t.TempDir()
	if _, err := LoadMarker(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing marker: err=%v, want os.ErrNotExist", err)
	}
	m := validMarker()
	m.OwnerPID, m.OwnerStartID, m.OwnerBootID = 123, 456, "b"
	m.TmuxTmpdir, m.SandboxRoot = "/tmp/sbx/tmux", "/tmp/sbx"
	if err := WriteMarker(home, m); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMarker(home)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *m {
		t.Fatalf("round trip: got %+v, want %+v", *got, *m)
	}
}

func TestLoadMarkerRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"not json", "nope"},
		{"partial json", `{"version": 1, `},
		{"wrong version", `{"version": 9, "created_unix": 1, "max_lifetime_seconds": 1}`},
		{"no created", `{"version": 1, "max_lifetime_seconds": 1}`},
		{"bad lifetime", `{"version": 1, "created_unix": 1, "max_lifetime_seconds": 999999999999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(MarkerPath(home), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMarker(home); err == nil {
				t.Fatal("invalid marker loaded without error")
			}
		})
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	dir := RegistryDir(t.TempDir(), 4242)
	if filepath.Base(dir) != registryDirPrefix+"4242" {
		t.Fatalf("registry dir %q lacks per-uid prefix", dir)
	}
	homes, malformed, err := ReadRegistryHomes(dir)
	if err != nil || len(homes) != 0 || malformed != 0 {
		t.Fatalf("empty registry: homes=%v malformed=%d err=%v", homes, malformed, err)
	}
	homeA := "/tmp/sandboxes/a"
	homeB := "/tmp/sandboxes/b"
	if err := WriteRegistryEntry(dir, homeA); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistryEntry(dir, homeB); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("registry dir mode %v, want 0700", info.Mode().Perm())
	}
	homes, malformed, err = ReadRegistryHomes(dir)
	if err != nil || malformed != 0 {
		t.Fatalf("read: malformed=%d err=%v", malformed, err)
	}
	if len(homes) != 2 || (homes[0] != homeA && homes[1] != homeA) {
		t.Fatalf("homes=%v, want %s and %s", homes, homeA, homeB)
	}
	// Re-writing the same home must not create a second entry.
	if err := WriteRegistryEntry(dir, homeA); err != nil {
		t.Fatal(err)
	}
	homes, _, err = ReadRegistryHomes(dir)
	if err != nil || len(homes) != 2 {
		t.Fatalf("re-write: homes=%v err=%v", homes, err)
	}
	if err := RemoveRegistryEntry(dir, homeA); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRegistryEntry(dir, homeA); err != nil {
		t.Fatalf("double remove should succeed: %v", err)
	}
	homes, _, err = ReadRegistryHomes(dir)
	if err != nil || len(homes) != 1 || homes[0] != homeB {
		t.Fatalf("after remove: homes=%v err=%v", homes, err)
	}
}

func TestRegistrySkipsMalformed(t *testing.T) {
	dir := RegistryDir(t.TempDir(), 1)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistryEntry(dir, "/tmp/ok-home"); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"bad.json":         "not json",
		"empty.json":       `{"version":1}`,
		"not-an-entry.txt": `{"version":1,"home":"/tmp/ignored"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	homes, malformed, err := ReadRegistryHomes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(homes) != 1 || homes[0] != "/tmp/ok-home" {
		t.Fatalf("homes=%v, want only the valid entry", homes)
	}
	if malformed != 2 {
		t.Fatalf("malformed=%d, want 2", malformed)
	}
}

// TestRegistryRefusesUntrusted pins LOW 2 of the independent review: the
// sweep may only act on a registry the sandbox owner provably controls —
// owned by this uid, owner-only mode, and never through a symlink. An
// attacker-injected entry is how a reaper gets aimed at a real home.
func TestRegistryRefusesUntrusted(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistryEntry(real, "/tmp/attacker-home"); err != nil {
		t.Fatal(err)
	}
	t.Run("loose directory mode", func(t *testing.T) {
		loose := filepath.Join(base, "loose")
		if err := os.MkdirAll(loose, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadRegistryHomes(loose); err == nil {
			t.Fatal("a world-writable registry dir was trusted")
		}
	})
	t.Run("directory is a symlink", func(t *testing.T) {
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skip("cannot create symlink fixture")
		}
		if _, _, err := ReadRegistryHomes(link); err == nil {
			t.Fatal("a symlinked registry dir was trusted")
		}
	})
	t.Run("write into symlinked dir refuses", func(t *testing.T) {
		link := filepath.Join(base, "wlink")
		if err := os.Symlink(real, link); err != nil {
			t.Skip("cannot create symlink fixture")
		}
		if err := WriteRegistryEntry(link, "/tmp/victim-home"); err == nil {
			t.Fatal("write through a symlinked registry dir was allowed")
		}
	})
	t.Run("symlinked entry is skipped", func(t *testing.T) {
		dir := filepath.Join(base, "entsym")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteRegistryEntry(dir, "/tmp/ok-home"); err != nil {
			t.Fatal(err)
		}
		forged := filepath.Join(base, "forged.json")
		if err := os.WriteFile(forged, []byte(`{"version":1,"home":"/tmp/victim"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(forged, RegistryEntryPath(dir, "/tmp/victim")); err != nil {
			t.Skip("cannot create symlink fixture")
		}
		homes, malformed, err := ReadRegistryHomes(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(homes) != 1 || homes[0] != "/tmp/ok-home" {
			t.Fatalf("homes=%v — a symlinked entry was read", homes)
		}
		if malformed != 1 {
			t.Fatalf("malformed=%d, want 1 for the symlinked entry", malformed)
		}
	})
}

// TestLoadMarkerRefusesSymlink pins the same trust rule on the trust anchor
// itself: a marker reachable only through a symlink is not a marker this
// user wrote.
func TestLoadMarkerRefusesSymlink(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(real, []byte(`{"version":1,"created_unix":1,"max_lifetime_seconds":60}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, MarkerPath(home)); err != nil {
		t.Skip("cannot create symlink fixture")
	}
	if m, err := LoadMarker(home); err == nil || m != nil {
		t.Fatalf("a symlinked marker loaded: %+v", m)
	}
}

// TestRootSentinelBindsToCreatedUnix pins the creation-proof contract: the
// sentinel's content IS the marker's created_unix, so a root can only
// prove the sandbox made it when the two agree.
func TestRootSentinelBindsToCreatedUnix(t *testing.T) {
	root := t.TempDir()
	if err := WriteRootSentinel(root, 1700000000); err != nil {
		t.Fatal(err)
	}
	data, err := TrustedOwnedFile(filepath.Join(root, RootSentinelName))
	if err != nil {
		t.Fatalf("the sentinel must pass the trust bar: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "1700000000" {
		t.Fatalf("sentinel content %q", got)
	}
}

// TestRecordedPIDsRoundTrip covers the write/read path the daemon and the
// teardown share: entries merge without duplicating, and a missing file
// reads as ErrNotExist.
func TestRecordedPIDsRoundTrip(t *testing.T) {
	home := t.TempDir()
	if _, err := ReadRecordedPIDs(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty home: %v", err)
	}
	for _, rec := range []RecordedPID{{PID: 10, StartID: 100}, {PID: 11, StartID: 110}, {PID: 10, StartID: 100}} {
		if err := WriteRecordedPID(home, rec.PID, rec.StartID); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := ReadRecordedPIDs(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("recorded %d entries after dedupe, want 2: %v", len(recs), recs)
	}
}

// TestRecordedPIDsRefusesUntrustedAndMalformed pins the trust bar on the
// teardown's only portable membership evidence: a symlinked or malformed
// file is never read, and a malformed file is never silently clobbered.
func TestRecordedPIDsRefusesUntrustedAndMalformed(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(t.TempDir(), "pids.json")
	if err := os.WriteFile(real, []byte(`{"version":1,"pids":[]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, RecordedPIDsPath(home)); err != nil {
		t.Skip("cannot create symlink fixture")
	}
	if _, err := ReadRecordedPIDs(home); err == nil {
		t.Fatal("a symlinked recorded-pid file was read")
	}
	if err := WriteRecordedPID(home, 1, 1); err == nil {
		t.Fatal("a symlinked recorded-pid file was written through")
	}

	home2 := t.TempDir()
	if err := os.WriteFile(RecordedPIDsPath(home2), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecordedPIDs(home2); err == nil {
		t.Fatal("a malformed recorded-pid file was read")
	}
	if err := WriteRecordedPID(home2, 1, 1); err == nil {
		t.Fatal("a malformed recorded-pid file was silently clobbered")
	}
}
