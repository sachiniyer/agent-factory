package execgroup

import (
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests drive resignal and sweep with a scripted membership read and a
// recording kill, so they run, and send no signal, on every platform.

// fakePgid is the group the scripted reads describe. The kill is recorded,
// never sent.
const fakePgid = 4242

func live(pid int) groupMember   { return groupMember{pid: pid} }
func zombie(pid int) groupMember { return groupMember{pid: pid, zombie: true} }

// script answers each read with the next scripted listing and fails the test
// if resignal reads past the end.
type script struct {
	t     *testing.T
	reads [][]groupMember
	n     int
	kills []int // the read count when each kill was sent
}

func (s *script) read(pgid int) ([]groupMember, error) {
	if pgid != fakePgid {
		s.t.Fatalf("read pgid %d, want %d", pgid, fakePgid)
	}
	if s.n >= len(s.reads) {
		s.t.Fatalf("resignal made read %d, past the %d scripted: it did not stop when it should have", s.n+1, len(s.reads))
	}
	s.n++
	return s.reads[s.n-1], nil
}

func (s *script) kill(pid int, sig syscall.Signal) error {
	if pid != -fakePgid || sig != syscall.SIGKILL {
		s.t.Fatalf("kill(%d, %v), want kill(%d, SIGKILL)", pid, sig, -fakePgid)
	}
	s.kills = append(s.kills, s.n)
	return nil
}

func (s *script) run() {
	resignal(fakePgid, s.read, s.kill, time.Now().Add(time.Minute))
}

func (s *script) want(t *testing.T, reads int, killsAfter ...int) {
	t.Helper()
	if s.n != reads {
		t.Errorf("resignal made %d reads, want %d", s.n, reads)
	}
	if len(s.kills) != len(killsAfter) {
		t.Fatalf("resignal killed after reads %v, want after reads %v", s.kills, killsAfter)
	}
	for i := range killsAfter {
		if s.kills[i] != killsAfter[i] {
			t.Fatalf("resignal killed after reads %v, want after reads %v", s.kills, killsAfter)
		}
	}
}

// The ordinary sweep: the first read still finds members dying from the
// first kill, so resignal kills again. Then a quiet read, and a second quiet
// read that lists nothing new, end it.
func TestResignalStopsAfterTwoQuietReads(t *testing.T) {
	s := &script{t: t, reads: [][]groupMember{
		{live(fakePgid), live(7)},
		{zombie(fakePgid), zombie(7)},
		{zombie(fakePgid)}, // 7 was reaped: nothing new, still quiet
	}}
	s.run()
	s.want(t, 3, 1)
}

// The stale read. The first read listed a forking parent P, missed the child
// C it was creating, and then read P after P had died. That one read finds
// no live member while C, which no signal reached, lives on. The confirming
// read lists C alive, and resignal must kill it rather than stop.
func TestResignalDoesNotStopOnOneQuietRead(t *testing.T) {
	const p, c = 7, 8
	s := &script{t: t, reads: [][]groupMember{
		{zombie(fakePgid), zombie(p)},
		{zombie(fakePgid), zombie(p), live(c)},
		{zombie(fakePgid), zombie(p), zombie(c)},
		{zombie(fakePgid), zombie(p), zombie(c)},
	}}
	s.run()
	s.want(t, 4, 2)
}

// Two quiet reads are not enough when the second lists a pid the first did
// not. That process was created between the two reads, so its creator may
// still have been forking when the second read took its list.
func TestResignalDoesNotStopWhenAQuietReadListsANewPid(t *testing.T) {
	const p, c, d = 7, 8, 9
	s := &script{t: t, reads: [][]groupMember{
		{zombie(fakePgid), zombie(p)},
		{zombie(fakePgid), zombie(p), zombie(c)}, // c is new: not proof
		{zombie(fakePgid), zombie(p), zombie(c), live(d)},
		{zombie(fakePgid), zombie(p), zombie(c), zombie(d)},
		{zombie(fakePgid), zombie(p), zombie(c), zombie(d)},
	}}
	s.run()
	s.want(t, 5, 3)
}

// The leader's pid listed alive after a read showed it dead names a new
// process, so the group is no longer ours: stop without signalling it.
func TestResignalStopsWhenTheLeaderPidComesBackAlive(t *testing.T) {
	s := &script{t: t, reads: [][]groupMember{
		{zombie(fakePgid), live(7)},
		{live(fakePgid)},
	}}
	s.run()
	s.want(t, 2, 1)
}

// The same when the leader was already gone, as it is for Reap after Wait.
func TestResignalStopsWhenAGoneLeaderPidComesBackAlive(t *testing.T) {
	s := &script{t: t, reads: [][]groupMember{
		{live(7)},
		{live(fakePgid), live(7)},
	}}
	s.run()
	s.want(t, 2, 1)
}

// No signal goes out once the deadline has passed, however long the read
// took: by then the pgid may name another group.
func TestResignalSendsNothingPastTheDeadline(t *testing.T) {
	s := &script{t: t, reads: [][]groupMember{{live(fakePgid)}}}
	resignal(fakePgid, s.read, s.kill, time.Now().Add(-time.Second))
	s.want(t, 1)
}

// A failed read ends the loop rather than spinning to the bound: the first
// kill has already been sent.
func TestResignalStopsOnAReadError(t *testing.T) {
	reads := 0
	resignal(fakePgid, func(int) ([]groupMember, error) {
		reads++
		return []groupMember{live(fakePgid)}, errors.New("sysctl failed")
	}, func(int, syscall.Signal) error {
		t.Fatal("resignal signalled after a failed read")
		return nil
	}, time.Now().Add(time.Minute))
	if reads != 1 {
		t.Fatalf("resignal read the group %d times after a failed read, want 1", reads)
	}
}

// A member that never dies reads the group a few dozen times before the
// deadline, not once a millisecond.
func TestResignalBacksOff(t *testing.T) {
	const bound = 300 * time.Millisecond
	reads := 0
	resignal(fakePgid, func(int) ([]groupMember, error) {
		reads++
		return []groupMember{live(fakePgid)}, nil
	}, func(int, syscall.Signal) error { return nil }, time.Now().Add(bound))
	// Pauses of 1, 2, 4 … 50ms fit about 12 reads into 300ms. A fixed 1ms
	// pause would fit about 300.
	if reads < 2 || reads > 30 {
		t.Fatalf("resignal read the group %d times in %v, want 2-30 with the pause backing off", reads, bound)
	}
}

// sweepSlack is how far past its bound sweep may return on a loaded runner.
const sweepSlack = 500 * time.Millisecond

// A membership read can block indefinitely: XNU's kern.proc.pgrp waits,
// uninterruptibly, on a member part-way through an exec. sweep must still
// return within its bound, and the parked read must send nothing when it
// finally returns.
func TestSweepReturnsWithinItsBoundWhenAReadBlocks(t *testing.T) {
	const bound = 100 * time.Millisecond
	release := make(chan struct{})
	returned := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	kills := 0
	read := func(int) ([]groupMember, error) {
		<-release
		defer once.Do(func() { close(returned) })
		return []groupMember{live(fakePgid)}, nil
	}
	kill := func(int, syscall.Signal) error {
		mu.Lock()
		kills++
		mu.Unlock()
		return nil
	}

	start := time.Now()
	sweep(fakePgid, read, kill, bound)
	took := time.Since(start)
	if took < bound || took > bound+sweepSlack {
		close(release)
		t.Fatalf("sweep returned after %v with its read blocked, want %v to %v", took, bound, bound+sweepSlack)
	}

	// Let the parked read return a live member. Its deadline has passed, so
	// resignal must return without a kill, and therefore without the second
	// read the script would need.
	close(release)
	<-returned
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if kills != 0 {
		t.Fatalf("the read that returned after the bound was followed by %d kills, want 0", kills)
	}
}

// A sweep that finishes returns at once, without waiting out its bound.
func TestSweepReturnsAsSoonAsTheGroupIsDead(t *testing.T) {
	quiet := func(int) ([]groupMember, error) { return []groupMember{zombie(fakePgid)}, nil }
	noKill := func(int, syscall.Signal) error {
		t.Error("sweep signalled a group with no live member")
		return nil
	}
	start := time.Now()
	sweep(fakePgid, quiet, noKill, time.Minute)
	if took := time.Since(start); took > sweepSlack {
		t.Fatalf("sweep of a dead group took %v, want it to return at once", took)
	}
}
