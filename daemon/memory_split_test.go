package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemorySplitLineIncludesCgroupOnlyWhenReadable(t *testing.T) {
	withCgroup := memorySplitLine(170<<20, 850<<20, 169<<20, 608<<20, true)
	for _, want := range []string{"go_heap=178257920", "go_total=891289600", "cgroup_anon=177209344", "cgroup_file=637534208"} {
		if !strings.Contains(withCgroup, want) {
			t.Errorf("line %q missing %q", withCgroup, want)
		}
	}
	without := memorySplitLine(170<<20, 850<<20, 0, 0, false)
	if strings.Contains(without, "cgroup_") {
		t.Errorf("cgroup-less line %q must not carry cgroup fields", without)
	}
	for _, want := range []string{"go_heap=", "go_total="} {
		if !strings.Contains(without, want) {
			t.Errorf("line %q missing %q", without, want)
		}
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

func TestCgroupV2AnonFileReadsOwnHierarchy(t *testing.T) {
	dir := t.TempDir()
	statDir := filepath.Join(dir, "user.slice", "x.service")
	if err := os.MkdirAll(statDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statDir, "memory.stat"),
		[]byte("anon 1234\nfile 5678\n"), 0o644); err != nil {
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

	anon, file, ok := cgroupV2AnonFile()
	if !ok || anon != 1234 || file != 5678 {
		t.Fatalf("cgroupV2AnonFile = %d, %d, %v; want 1234, 5678, true", anon, file, ok)
	}
}

func TestCgroupV2AnonFileQuietWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	oldCgroup, oldRoot := cgroupSelfCgroupFile, cgroupV2Root
	cgroupSelfCgroupFile, cgroupV2Root = filepath.Join(dir, "missing"), dir
	t.Cleanup(func() { cgroupSelfCgroupFile, cgroupV2Root = oldCgroup, oldRoot })
	if _, _, ok := cgroupV2AnonFile(); ok {
		t.Fatal("no procfs must report not-ok, not fake counters")
	}

	// cgroup v1: entries carry hierarchy ids, never the "0::" unified line.
	v1 := filepath.Join(dir, "v1.cgroup")
	if err := os.WriteFile(v1, []byte("10:memory:/user.slice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cgroupSelfCgroupFile = v1
	if _, _, ok := cgroupV2AnonFile(); ok {
		t.Fatal("cgroup v1 must report not-ok")
	}
}
