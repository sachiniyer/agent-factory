package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemorySplitLineIncludesCgroupOnlyWhenReadable(t *testing.T) {
	withCgroup := memorySplitLine(170<<20, 850<<20, cgroupMemory{
		anon: 169 << 20, file: 608 << 20, statOK: true,
		peak: 1 << 30, peakOK: true,
	})
	for _, want := range []string{"go_heap=178257920", "go_total=891289600", "cgroup_anon=177209344", "cgroup_file=637534208", "cgroup_peak=1073741824"} {
		if !strings.Contains(withCgroup, want) {
			t.Errorf("line %q missing %q", withCgroup, want)
		}
	}
	without := memorySplitLine(170<<20, 850<<20, cgroupMemory{})
	if strings.Contains(without, "cgroup_") {
		t.Errorf("cgroup-less line %q must not carry cgroup fields", without)
	}
	for _, want := range []string{"go_heap=", "go_total="} {
		if !strings.Contains(without, want) {
			t.Errorf("line %q missing %q", without, want)
		}
	}
	// The peak is independent of the stat pair: a cgroup whose only readable
	// counter is memory.peak still yields it.
	peakOnly := memorySplitLine(1, 2, cgroupMemory{peak: 4096, peakOK: true})
	if !strings.Contains(peakOnly, "cgroup_peak=4096") || strings.Contains(peakOnly, "cgroup_anon") {
		t.Errorf("peak-only line %q must carry cgroup_peak and no stat pair", peakOnly)
	}
}

func TestParseMemoryStatAnonFile(t *testing.T) {
	body := "anon 170643456\nfile 608694272\nkernel_stack 507904\nfile_dirty 8192\ninactive_file 533159936\n"
	anon, file, ok := parseMemoryStatAnonFile([]byte(body))
	if !ok || anon != 170643456 || file != 608694272 {
		t.Fatalf("parseMemoryStatAnonFile = %d, %d, %v; want 170643456, 608694272, true", anon, file, ok)
	}
	// Missing one of the pair is a shape this reader does not claim.
	if _, _, ok := parseMemoryStatAnonFile([]byte("anon 1\n")); ok {
		t.Fatal("a stat without the file counter must report not-ok")
	}
	if _, _, ok := parseMemoryStatAnonFile(nil); ok {
		t.Fatal("an empty stat must report not-ok")
	}
	if _, _, ok := parseMemoryStatAnonFile([]byte("anon\nfile nope\n")); ok {
		t.Fatal("unparseable values must report not-ok")
	}
}

func TestParseMemoryPeak(t *testing.T) {
	if peak, ok := parseMemoryPeak([]byte("1233506304\n")); !ok || peak != 1233506304 {
		t.Fatalf("parseMemoryPeak = %d, %v; want 1233506304, true", peak, ok)
	}
	if _, ok := parseMemoryPeak([]byte("max\n")); ok {
		t.Fatal("a non-numeric body must report not-ok")
	}
	if _, ok := parseMemoryPeak(nil); ok {
		t.Fatal("an empty body must report not-ok")
	}
}

func TestCgroupV2MemoryReadsOwnHierarchy(t *testing.T) {
	dir := t.TempDir()
	statDir := filepath.Join(dir, "user.slice", "x.service")
	if err := os.MkdirAll(statDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statDir, "memory.stat"),
		[]byte("anon 1234\nfile 5678\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statDir, "memory.peak"),
		[]byte("9012\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cgroupFile := filepath.Join(dir, "self.cgroup")
	if err := os.WriteFile(cgroupFile,
		[]byte("10:memory:/legacy\n0::/user.slice/x.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCgroup, oldRoot := cgroupSelfCgroupFile, cgroupV2Root
	cgroupSelfCgroupFile, cgroupV2Root = cgroupFile, dir
	t.Cleanup(func() { cgroupSelfCgroupFile, cgroupV2Root = oldCgroup, oldRoot })

	cg := cgroupV2Memory()
	if !cg.statOK || cg.anon != 1234 || cg.file != 5678 {
		t.Fatalf("stat pair = %d, %d, %v; want 1234, 5678, true", cg.anon, cg.file, cg.statOK)
	}
	if !cg.peakOK || cg.peak != 9012 {
		t.Fatalf("peak = %d, %v; want 9012, true", cg.peak, cg.peakOK)
	}
}

func TestCgroupV2MemoryPeakAbsentKeepsStatPair(t *testing.T) {
	dir := t.TempDir()
	statDir := filepath.Join(dir, "x.service")
	if err := os.MkdirAll(statDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// memory.stat only — the pre-5.19 shape, where memory.peak does not exist.
	if err := os.WriteFile(filepath.Join(statDir, "memory.stat"),
		[]byte("anon 1234\nfile 5678\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cgroupFile := filepath.Join(dir, "self.cgroup")
	if err := os.WriteFile(cgroupFile, []byte("0::/x.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCgroup, oldRoot := cgroupSelfCgroupFile, cgroupV2Root
	cgroupSelfCgroupFile, cgroupV2Root = cgroupFile, dir
	t.Cleanup(func() { cgroupSelfCgroupFile, cgroupV2Root = oldCgroup, oldRoot })

	cg := cgroupV2Memory()
	if !cg.statOK || cg.anon != 1234 || cg.file != 5678 {
		t.Fatalf("stat pair = %d, %d, %v; want 1234, 5678, true", cg.anon, cg.file, cg.statOK)
	}
	if cg.peakOK || cg.peak != 0 {
		t.Fatalf("absent peak = %d, %v; want 0, false — never a fake zero", cg.peak, cg.peakOK)
	}
}

func TestCgroupV2MemoryQuietWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	oldCgroup, oldRoot := cgroupSelfCgroupFile, cgroupV2Root
	cgroupSelfCgroupFile, cgroupV2Root = filepath.Join(dir, "missing"), dir
	t.Cleanup(func() { cgroupSelfCgroupFile, cgroupV2Root = oldCgroup, oldRoot })
	if cg := cgroupV2Memory(); cg.statOK || cg.peakOK {
		t.Fatal("no procfs must report not-ok on both halves, not fake counters")
	}

	// cgroup v1: entries carry hierarchy ids, never the "0::" unified line.
	v1 := filepath.Join(dir, "v1.cgroup")
	if err := os.WriteFile(v1, []byte("10:memory:/user.slice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cgroupSelfCgroupFile = v1
	if cg := cgroupV2Memory(); cg.statOK || cg.peakOK {
		t.Fatal("cgroup v1 must report not-ok on both halves")
	}
}
