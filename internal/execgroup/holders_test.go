package execgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// scripted is a holderScanner that answers from a list, one answer per scan,
// repeating the last, and records every signal sent.
type scripted struct {
	mu      sync.Mutex
	answers [][]proctree.Process
	errAt   int // scan index that fails, or -1
	scans   int
	sent    []int
}

func (s *scripted) scan(pgid int, ends []proctree.PipeEnd) ([]proctree.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.scans
	s.scans++
	if i == s.errAt {
		return nil, errors.New("scripted scan failure")
	}
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	return s.answers[i], nil
}

func (s *scripted) signal(p proctree.Process, sig syscall.Signal, _ time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig != syscall.SIGKILL {
		panic("reapPipeHolders sends only SIGKILL")
	}
	s.sent = append(s.sent, p.PID)
	return true
}

func proc(pid int) proctree.Process { return proctree.Process{PID: pid, StartID: uint64(pid)} }

// Two quiet scans in a row end the reap, and a quiet group is never signalled.
func TestReapPipeHoldersStopsAfterTwoQuietScans(t *testing.T) {
	s := &scripted{answers: [][]proctree.Process{nil}, errAt: -1}
	reapPipeHoldersWith(100, nil, s.scan, s.signal, time.Now().Add(time.Minute))
	if s.scans != 2 {
		t.Errorf("scans = %d, want 2: one quiet scan and its confirmation", s.scans)
	}
	if len(s.sent) != 0 {
		t.Errorf("signalled %v with no holder found", s.sent)
	}
}

// A holder found by a later scan is signalled too: on darwin the first read
// can miss a child a holder was forking, and the child holds the pipe as well.
func TestReapPipeHoldersRescansAfterEachKill(t *testing.T) {
	s := &scripted{answers: [][]proctree.Process{
		{proc(7)},
		nil,
		{proc(8)}, // the missed child, listed by the confirming scan
		nil,
		nil,
	}, errAt: -1}
	reapPipeHoldersWith(100, nil, s.scan, s.signal, time.Now().Add(time.Minute))
	if len(s.sent) != 2 || s.sent[0] != 7 || s.sent[1] != 8 {
		t.Errorf("signalled %v, want [7 8]: each holder found, in the order found", s.sent)
	}
	if s.scans != 5 {
		t.Errorf("scans = %d, want 5", s.scans)
	}
}

// A scan that fails proves nothing, so nothing is signalled on it.
func TestReapPipeHoldersSendsNothingOnAFailedScan(t *testing.T) {
	s := &scripted{answers: [][]proctree.Process{{proc(7)}}, errAt: 0}
	reapPipeHoldersWith(100, nil, s.scan, s.signal, time.Now().Add(time.Minute))
	if len(s.sent) != 0 {
		t.Errorf("signalled %v after a failed scan", s.sent)
	}
}

// No signal goes out once the deadline has passed, and the reap returns by
// the deadline even when a scan blocks well past it.
func TestReapPipeHoldersHonoursItsDeadline(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var sent []int
	scan := func(int, []proctree.PipeEnd) ([]proctree.Process, error) {
		<-release
		return []proctree.Process{proc(7)}, nil
	}
	signal := func(p proctree.Process, _ syscall.Signal, _ time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, p.PID)
		return true
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	start := time.Now()
	reapPipeHoldersWith(100, nil, scan, signal, deadline)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("returned after %v; a blocked scan must not hold the reap past its deadline", took)
	}
	close(release) // the scan now answers, late
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 0 {
		t.Errorf("signalled %v on an answer that arrived after the deadline", sent)
	}
}

// The production signaller's own rules: it never signals pid 1 or itself, and
// sends nothing when the identity check answers after the deadline.
func TestSignalIfSameRefusesWhatItCannotProve(t *testing.T) {
	if signalIfSame(proc(1), syscall.Signal(0), time.Now().Add(time.Minute)) {
		t.Error("signalIfSame signalled pid 1")
	}
	self := proctree.Process{PID: os.Getpid()}
	if signalIfSame(self, syscall.Signal(0), time.Now().Add(time.Minute)) {
		t.Error("signalIfSame signalled this process")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	live, err := proctree.Lookup(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if signalIfSame(live, syscall.Signal(0), time.Now()) {
		t.Error("signalIfSame sent a signal with its deadline already passed")
	}
	stale := live
	stale.StartID++
	if signalIfSame(stale, syscall.Signal(0), time.Now().Add(time.Minute)) {
		t.Error("signalIfSame signalled a pid whose start time no longer matches")
	}
	if !signalIfSame(live, syscall.Signal(0), time.Now().Add(time.Minute)) {
		t.Error("signalIfSame refused the same live instance before its deadline")
	}
}
