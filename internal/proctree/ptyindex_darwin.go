//go:build darwin

package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Darwin decodes each held pty through proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO),
// the same kernel read openFilePaths already performs — plus the vnode's
// vst_rdev, which for a /dev/ptmx master carries the pty index in its minor
// (measured on the macOS runner, tmux 3.4: master's rdev 0x0f000000 = major 15
// minor 0 against its /dev/ttys000 slave's 0x10000000 = major 16 minor 0; the
// shared minor is the index #7469 needs). The slave side keeps its name's
// numeric tail — /dev/ttysNNN — the same convention /dev/pts/N uses on linux.
//
// Offsets inside vnode_fdinfowithpath, from Apple's headers: proc_fileinfo is
// 24 bytes, then vinfo_stat — vst_dev u32 at 24, vst_rdev u32 at 140 — then
// vnode_info's vi_type i32 at 160, vip_path at 176 (fdinfopath.go). vi_type
// VCHR is 4, measured on both ptmx and ttys vnodes.
const (
	vnodeFdInfoRdevOffset = 140
	vnodeFdInfoViTypeOff  = 160
	vnodeFdInfoViTypeVCHR = 4
	darwinPtmxPath        = "/dev/ptmx"
	darwinDevTMinorMask   = 0xffffff // makedev(major<<24 | minor)
)

// ptyFdReadBudget bounds ONE process's fd walk. Every vnode decode goes
// through vn_stat → VNOP_GETATTR, which BLOCKS on a hung NFS, SMB or macFUSE
// mount — and a carrier or tmux server holding a single fd into such a mount
// would otherwise wedge the sweep's attribution read forever (#7469 R2). No
// getattr-free discriminator exists for another process's vnodes — the fd
// list says only "vnode", and both VNODEINFO and VNODEPATHINFO getattr — so
// the deadline is the mechanism: the whole walk runs on a worker goroutine
// (the same idiom exeCheckShared uses for its kernel reads), an expired
// budget reports "could not see" — undecidable, never foreign or own — and
// the abandoned worker is the only thing left on the mount. Package var so
// the deadline test can shrink it.
var ptyFdReadBudget = 2 * time.Second

// errPtyFdTimeout is the expired-budget answer: the fd table could not be
// READ in bounded time — "could not see", never "holds none".
var errPtyFdTimeout = errors.New("pty fd read exceeded its deadline")

// ptySlaveIndices returns the identity of every /dev/ttysNNN vnode pid
// holds. devfs is single-instance — there is no second devpts to mint a
// colliding index — so the Dev half is 0 and the index alone qualifies.
func ptySlaveIndices(pid int) ([]PtyIndex, error) {
	fds, err := ptyFdIndices(pid, true, func(path string, viType int32, rdev uint32) (PtyIndex, bool) {
		if viType == vnodeFdInfoViTypeVCHR && strings.HasPrefix(path, "/dev/ttys") {
			if idx, ok := ptySlaveIndexFromPath(path); ok {
				return PtyIndex{Index: idx}, true
			}
		}
		return PtyIndex{}, false
	})
	if err != nil {
		return nil, err
	}
	out := make([]PtyIndex, len(fds))
	for i, fi := range fds {
		out[i] = fi.PtyIndex
	}
	return out, nil
}

// ptyMasterFdIndices returns the identity of every /dev/ptmx vnode pid
// holds — the vnode's rdev minor, since the name cannot say which pty it
// masters — paired with the descriptor number, so a cached ownership claim
// re-verifies one fd instead of re-walking the table (#7581 item 2).
func ptyMasterFdIndices(pid int) ([]PtyFdIndex, error) {
	return ptyFdIndices(pid, false, func(path string, viType int32, rdev uint32) (PtyIndex, bool) {
		if viType == vnodeFdInfoViTypeVCHR && path == darwinPtmxPath {
			return PtyIndex{Index: int(rdev & darwinDevTMinorMask)}, true
		}
		return PtyIndex{}, false
	})
}

// ptyVnodeKey scopes a shared single-fd decode to what it reads: the
// process instance — (pid, StartID), so a recycled pid can never join
// another process's flight — and the descriptor. It serves the master's
// re-verify only; a wedged vnode read must hold one worker however many
// carriers check the same recorded fd (Codex on #7581).
type ptyVnodeKey struct {
	pid     int
	startID uint64
	fd      int
}

// ptyVnodeFlight is ONE shared single-fd decode — the same flight shape
// the table walks use, keyed on the descriptor the recorded ownership
// claims re-verify.
type ptyVnodeFlight struct {
	done chan struct{}
	idx  PtyIndex
	ok   bool
}

type ptyVnodeFlightSet struct {
	mu       sync.Mutex
	inFlight map[ptyVnodeKey]*ptyVnodeFlight
}

var ptyVnodeFlights = ptyVnodeFlightSet{inFlight: map[ptyVnodeKey]*ptyVnodeFlight{}}

func (s *ptyVnodeFlightSet) begin(key ptyVnodeKey) (f *ptyVnodeFlight, leader bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.inFlight[key]; ok {
		return f, false
	}
	f = &ptyVnodeFlight{done: make(chan struct{})}
	s.inFlight[key] = f
	return f, true
}

func (s *ptyVnodeFlightSet) finish(key ptyVnodeKey, f *ptyVnodeFlight) {
	s.mu.Lock()
	delete(s.inFlight, key)
	s.mu.Unlock()
	close(f.done)
}

// inFlightCount is the test seam for the memory bound: under a wedged fd
// read it stays 1 per descriptor however many waiters expire.
func (s *ptyVnodeFlightSet) inFlightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

// ptyMasterFdIndex re-verifies ONE recorded descriptor — the single-fd
// check behind the sweep's ownership claims (#7581 item 2). The decode is
// the same getattr-prone VNODEPATHINFO the walk performs, so it runs under
// the same ptyFdReadBudget flight AND the in-flight set: a wedged read
// contributes nothing — never a silent "does not hold" — and however many
// carriers match the same cached ownership entry, the descriptor holds at
// most one blocked worker (Codex on #7581).
func ptyMasterFdIndex(pid, fd int) (PtyIndex, bool) {
	// Key the flight on the process INSTANCE, exactly like the table
	// walks: a pid recycled between snapshot and read must never share
	// another instance's worker.
	var startID uint64
	if p, err := Lookup(pid); err == nil {
		startID = p.StartID
	}
	key := ptyVnodeKey{pid: pid, startID: startID, fd: fd}
	f, leader := ptyVnodeFlights.begin(key)
	if leader {
		// The read is captured on THIS goroutine: an abandoned worker
		// must never read the package var lazily.
		read := ptyFdVnodeRead
		go func() {
			f.idx, f.ok = read(pid, fd)
			ptyVnodeFlights.finish(key, f)
		}()
	}
	timer := time.NewTimer(ptyFdReadBudget)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.idx, f.ok
	case <-timer.C:
		return PtyIndex{}, false
	}
}

// ptyFdVnodeRead decodes one descriptor's vnode into its pty-master
// identity — the read ptyMasterFdIndex bounds. Package var so a test can
// stage it; captured before the flight spawns.
var ptyFdVnodeRead = func(pid, fd int) (PtyIndex, bool) {
	path, viType, rdev, err := ptyFdVnode(pid, fd)
	if err != nil {
		return PtyIndex{}, false
	}
	if viType == vnodeFdInfoViTypeVCHR && path == darwinPtmxPath {
		return PtyIndex{Index: int(rdev & darwinDevTMinorMask)}, true
	}
	return PtyIndex{}, false
}

// ptyFdKey scopes a shared walk to what it reads: the process INSTANCE —
// (pid, StartID) so a recycled pid can never join another process's flight
// — and which side the walk gathers. A slave walk and a master walk of the
// same pid are different work and never share a flight.
type ptyFdKey struct {
	pid     int
	startID uint64
	slave   bool
}

// ptyFdFlight is ONE shared fd-table walk. Every carrier whose verdict
// meets the same process and side — a tmux server's table read for the
// owners map while carriers' slaves are walked — joins this record
// instead of each paying a vnode walk and, worst case, a hung-mount stall
// (#7581 item 3 — the attestFlight shape, keyed on the instance and side).
type ptyFdFlight struct {
	done    chan struct{}
	indices []PtyFdIndex
	err     error
}

type ptyFdFlightSet struct {
	mu       sync.Mutex
	inFlight map[ptyFdKey]*ptyFdFlight
}

var ptyFdFlights = ptyFdFlightSet{inFlight: map[ptyFdKey]*ptyFdFlight{}}

func (s *ptyFdFlightSet) begin(key ptyFdKey) (f *ptyFdFlight, leader bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.inFlight[key]; ok {
		return f, false
	}
	f = &ptyFdFlight{done: make(chan struct{})}
	s.inFlight[key] = f
	return f, true
}

func (s *ptyFdFlightSet) finish(key ptyFdKey, f *ptyFdFlight) {
	s.mu.Lock()
	delete(s.inFlight, key)
	s.mu.Unlock()
	close(f.done)
}

// inFlightCount is the test seam for the memory bound: under a wedged walk
// it must stay 1 per key however many waiters expire.
func (s *ptyFdFlightSet) inFlightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

// ptyFdIndices runs pid's vnode walk — slave or master per the side flag —
// under ptyFdReadBudget AND the in-flight set: concurrent walks of the same
// (pid, StartID, side) share one worker, each caller bounds its own wait,
// and a walk that cannot answer in bounded wall-clock time — a getattr
// parked on a hung mount — is "could not see", never a silent partial read
// (#7469 R2, #7581 item 3). The abandoned worker cannot be cancelled (an
// uninterruptible syscall is a one-way door), so the flight record holds
// constant state however many waiters expire.
func ptyFdIndices(pid int, slave bool, classify func(path string, viType int32, rdev uint32) (PtyIndex, bool)) ([]PtyFdIndex, error) {
	// Key the flight on the process INSTANCE: the walk reads a live fd
	// table, so a pid recycled between snapshot and read must never share
	// or inherit another instance's flight. A dead pid's walk fails fast
	// regardless — the zero StartID conflates only processes already gone.
	var startID uint64
	if p, err := Lookup(pid); err == nil {
		startID = p.StartID
	}
	key := ptyFdKey{pid: pid, startID: startID, slave: slave}
	f, leader := ptyFdFlights.begin(key)
	if leader {
		// The walk is captured on THIS goroutine: an abandoned worker
		// must never read the package var lazily — a test restoring it
		// after the deadline would race that read.
		walk := ptyFdWalk
		go func() {
			f.indices, f.err = walk(pid, classify)
			ptyFdFlights.finish(key, f)
		}()
	}
	timer := time.NewTimer(ptyFdReadBudget)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.indices, f.err
	case <-timer.C:
		return nil, errPtyFdTimeout
	}
}

// ptyFdWalk is the unbounded scan ptyFdIndices bounds — a package var so the
// deadline test can stage a walk that never answers. A refused or short read
// of the LIST is "could not see"; a single fd closing mid-walk is a smaller
// open set.
var ptyFdWalk = func(pid int, classify func(path string, viType int32, rdev uint32) (PtyIndex, bool)) ([]PtyFdIndex, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return nil, err
	}
	var indices []PtyFdIndex
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		if binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:]) != procFdTypeVnode || fd < 0 {
			continue
		}
		path, viType, rdev, err := ptyFdVnode(pid, fd)
		if err != nil {
			if fdClosedSinceListing(err) {
				continue // closed since the listing
			}
			if errors.Is(err, syscall.ESRCH) {
				return nil, ErrProcessExited
			}
			return nil, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO) for fd %d: %w", fd, err)
		}
		if path == "" {
			continue
		}
		if idx, ok := classify(path, viType, rdev); ok {
			indices = append(indices, PtyFdIndex{PtyIndex: idx, Fd: fd})
		}
	}
	return indices, nil
}

// ptyFdVnode decodes ONE descriptor's vnode — the VNODEPATHINFO read the
// walk performs per fd and the re-verify performs alone. An empty path
// with no error means the vnode named nothing (not a pty either way);
// err is the kernel call's own failure.
func ptyFdVnode(pid, fd int) (path string, viType int32, rdev uint32, err error) {
	var buf [vnodeFdInfoWithPathSize]byte
	n, err := procInfoRaw(procInfoCallPIDFDInfo, pid, procPIDFdVnodePathInfo,
		uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
	if err != nil {
		return "", 0, 0, err
	}
	if int(n) != len(buf) {
		return "", 0, 0, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO) for fd %d wrote %d bytes, want %d", fd, n, len(buf))
	}
	p, ok := fdPathFromVnodeFdInfo(buf[:])
	if !ok {
		return "", 0, 0, nil
	}
	return p, int32(binary.LittleEndian.Uint32(buf[vnodeFdInfoViTypeOff:])),
		binary.LittleEndian.Uint32(buf[vnodeFdInfoRdevOffset:]), nil
}
