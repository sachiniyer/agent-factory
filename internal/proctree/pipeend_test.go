package proctree

import (
	"encoding/binary"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestPipeInodeFromLink(t *testing.T) {
	cases := []struct {
		link string
		ino  uint64
		ok   bool
	}{
		{"pipe:[12345]", 12345, true},
		{"pipe:[0]", 0, true},
		{"pipe:[]", 0, false},
		{"pipe:[12x]", 0, false},
		{"pipe:12345", 0, false},
		{"socket:[12345]", 0, false},
		{"/tmp/pipe:[12345]", 0, false},
		{"anon_inode:[eventpoll]", 0, false},
	}
	for _, c := range cases {
		ino, err := pipeInodeFromLink(c.link)
		if (err == nil) != c.ok || ino != c.ino {
			t.Errorf("pipeInodeFromLink(%q) = %d, %v; want %d, ok=%v", c.link, ino, err, c.ino, c.ok)
		}
	}
}

// The offsets are what decide a kill, so they are pinned against a buffer
// built by hand from the C layout rather than against the decoder itself.
func TestPipeHandleFromPipeFdInfo(t *testing.T) {
	buf := make([]byte, pipeFdInfoSize)
	// proc_fileinfo (24 bytes), then pipe_info: vinfo_stat (136), pipe_handle,
	// pipe_peerhandle. Fill the neighbours with distinct values so an
	// off-by-eight decode reads the wrong one and fails.
	binary.LittleEndian.PutUint64(buf[24+136-8:], 0x1111111111111111) // last qspare of vinfo_stat
	binary.LittleEndian.PutUint64(buf[24+136:], 0xfeedfacecafebeef)   // pipe_handle
	binary.LittleEndian.PutUint64(buf[24+144:], 0x2222222222222222)   // pipe_peerhandle
	h, ok := pipeHandleFromPipeFdInfo(buf)
	if !ok || h != 0xfeedfacecafebeef {
		t.Fatalf("pipeHandleFromPipeFdInfo = %#x, %v; want 0xfeedfacecafebeef", h, ok)
	}
	if _, ok := pipeHandleFromPipeFdInfo(buf[:pipeFdInfoSize-1]); ok {
		t.Error("a short buffer must not decode")
	}
	if _, ok := pipeHandleFromPipeFdInfo(make([]byte, pipeFdInfoSize)); ok {
		t.Error("a zero handle must not decode: no live struct pipe hashes to zero")
	}
}

func TestPipeEndIDRefusesANonPipe(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notapipe")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := PipeEndID(f); err == nil {
		t.Error("a regular file must not have a pipe-end identity")
	}
}

// startHolder starts `sleep 60` with stdout on the write end of a fresh pipe
// when hold is true, and on /dev/null otherwise, in a process group of its
// own. It returns the child and the identity of that write end.
func startHolder(t *testing.T, hold bool) (*exec.Cmd, uint64) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	id, err := PipeEndID(w)
	if err != nil {
		t.Fatalf("PipeEndID: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if hold {
		cmd.Stdout = w
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = w.Close()
	// Wait for the exec, so the descriptors read are sleep's and not the
	// forked copy of this test binary, which held every pipe end.
	deadline := time.Now().Add(5 * time.Second)
	for {
		argv := Argv(cmd.Process.Pid)
		if len(argv) > 0 && argv[0] == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep did not exec within 5s (argv %q)", argv)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd, id
}

func TestHoldsPipeEndSeesAChildHoldingIt(t *testing.T) {
	cmd, id := startHolder(t, true)
	held, err := HoldsPipeEnd(cmd.Process.Pid, id)
	if err != nil || !held {
		t.Fatalf("HoldsPipeEnd(child holding the write end) = %v, %v; want true", held, err)
	}
	held, err = HoldsPipeEnd(cmd.Process.Pid, id+1)
	if err != nil || held {
		t.Errorf("HoldsPipeEnd(a different identity) = %v, %v; want false", held, err)
	}
}

func TestHoldsPipeEndSeesAChildNotHoldingIt(t *testing.T) {
	cmd, id := startHolder(t, false)
	held, err := HoldsPipeEnd(cmd.Process.Pid, id)
	if err != nil || held {
		t.Fatalf("HoldsPipeEnd(child with stdout on /dev/null) = %v, %v; want false", held, err)
	}
}

func TestHoldsPipeEndOnAGoneProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	held, err := HoldsPipeEnd(cmd.Process.Pid, 1)
	if held || err == nil {
		t.Errorf("HoldsPipeEnd(reaped pid) = %v, %v; want false and an error", held, err)
	}
}

func TestGroupMembersListsTheGroup(t *testing.T) {
	cmd, _ := startHolder(t, false)
	pgid := cmd.Process.Pid
	members, err := GroupMembers(pgid)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	if len(members) != 1 || members[0].PID != pgid {
		t.Fatalf("GroupMembers(%d) = %+v, want exactly the leader", pgid, members)
	}
	if !AliveSame(members[0]) {
		t.Error("the member's identity must match the live process")
	}
	if own, err := GroupMembers(syscall.Getpgrp()); err != nil {
		t.Fatalf("GroupMembers(own group): %v", err)
	} else {
		for _, m := range own {
			if m.PID == pgid {
				t.Errorf("the child leads its own group, so it must not be listed in the test's group")
			}
		}
	}
	if members, _ := GroupMembers(0); members != nil {
		t.Errorf("GroupMembers(0) = %v; a non-positive pgid must match nothing", members)
	}
}
