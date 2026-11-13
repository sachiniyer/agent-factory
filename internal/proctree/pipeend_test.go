package proctree

import (
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
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
func TestPipeHandlesFromPipeFdInfo(t *testing.T) {
	buf := make([]byte, pipeFdInfoSize)
	// proc_fileinfo (24 bytes), then pipe_info: vinfo_stat (136), pipe_handle,
	// pipe_peerhandle, pipe_status. Fill the neighbours with distinct values so
	// an off-by-eight decode reads the wrong one and fails.
	binary.LittleEndian.PutUint64(buf[24+136-8:], 0x1111111111111111) // last qspare of vinfo_stat
	binary.LittleEndian.PutUint64(buf[24+136:], 0xfeedfacecafebeef)   // pipe_handle
	binary.LittleEndian.PutUint64(buf[24+144:], 0x2222222222222222)   // pipe_peerhandle
	binary.LittleEndian.PutUint64(buf[24+152:], 0x3333333333333333)   // pipe_status, rfu_1
	h, peer, ok := pipeHandlesFromPipeFdInfo(buf)
	if !ok || h != 0xfeedfacecafebeef || peer != 0x2222222222222222 {
		t.Fatalf("pipeHandlesFromPipeFdInfo = %#x, %#x, %v; want 0xfeedfacecafebeef, 0x2222222222222222", h, peer, ok)
	}
	if _, _, ok := pipeHandlesFromPipeFdInfo(buf[:pipeFdInfoSize-1]); ok {
		t.Error("a short buffer must not decode")
	}
	if _, _, ok := pipeHandlesFromPipeFdInfo(make([]byte, pipeFdInfoSize)); ok {
		t.Error("a zero handle must not decode: no live struct pipe hashes to zero")
	}
	binary.LittleEndian.PutUint64(buf[24+144:], 0)
	if _, peer, ok := pipeHandlesFromPipeFdInfo(buf); !ok || peer != 0 {
		t.Errorf("a closed peer is a real answer, zero: got %#x, %v", peer, ok)
	}
}

func TestPipeEndOfRefusesWhatIsNotOnePipe(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notapipe")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := PipeEndOf(f, r); err == nil {
		t.Error("a regular file must not have a pipe-end identity")
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	defer w2.Close()
	if _, err := PipeEndOf(w, r2); err == nil && runtime.GOOS == "linux" {
		t.Error("ends of two different pipes must not pair")
	}
	if _, err := PipeEndOf(w, r); err != nil {
		t.Errorf("the two ends of one pipe must pair: %v", err)
	}
}

// startHolder starts `sleep 60` with stdout on the write end of a fresh pipe
// when hold is true, and on /dev/null otherwise, in a process group of its
// own. It returns the child and the pipe's identity, with the read end kept
// open by the test until cleanup.
func startHolder(t *testing.T, hold bool) (*exec.Cmd, PipeEnd) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	end, err := PipeEndOf(w, r)
	if err != nil {
		t.Fatalf("PipeEndOf: %v", err)
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
	return cmd, end
}

func TestHoldsPipeEndSeesAChildHoldingIt(t *testing.T) {
	cmd, end := startHolder(t, true)
	held, err := HoldsPipeEnd(cmd.Process.Pid, end)
	if err != nil || !held {
		t.Fatalf("HoldsPipeEnd(child holding the write end) = %v, %v; want true", held, err)
	}
	other := end
	other.held++
	other.kept++
	held, err = HoldsPipeEnd(cmd.Process.Pid, other)
	if err != nil || held {
		t.Errorf("HoldsPipeEnd(a different pipe) = %v, %v; want false", held, err)
	}
	if runtime.GOOS == "darwin" {
		// The held end's handle alone is not proof on darwin: an end freed and
		// reallocated at the same address hashes the same. Its peer must be the
		// end this process keeps open.
		wrongPeer := end
		wrongPeer.kept++
		held, err = HoldsPipeEnd(cmd.Process.Pid, wrongPeer)
		if err != nil || held {
			t.Errorf("HoldsPipeEnd(right handle, wrong peer) = %v, %v; want false", held, err)
		}
	}
}

func TestHoldsPipeEndSeesAChildNotHoldingIt(t *testing.T) {
	cmd, end := startHolder(t, false)
	held, err := HoldsPipeEnd(cmd.Process.Pid, end)
	if err != nil || held {
		t.Fatalf("HoldsPipeEnd(child with stdout on /dev/null) = %v, %v; want false", held, err)
	}
}

func TestHoldsPipeEndOnAGoneProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	held, err := HoldsPipeEnd(cmd.Process.Pid, PipeEnd{held: 1, kept: 2})
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
