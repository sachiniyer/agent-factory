//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RowMatch is the caller's verdict on one bound filesystem path the kernel's
// unix-socket table spells (#7695): certain — it names the queried socket;
// possible — the comparison cannot decide (a deleted component of either
// spelling may have been an alias, #7562); none — provably a different socket.
// Two further verdicts defer the answer to the row's HOLDER, which the sweep
// names only after attribution: mismatch — the caller's own filesystem view
// proves a different socket, but the holder may have bound it in a different
// mount view, so the holder decides (#7695 R1); relative — the spelling is
// not absolute, so the caller's filesystem cannot place it and the holder's
// working directory may (#7695 R2).
type RowMatch int

const (
	RowMatchNone RowMatch = iota
	RowMatchPossible
	RowMatchCertain
	RowMatchMismatch
	RowMatchRelative
)

// PendingRow is a bound-path table row the match deferred to its holder:
// the holder's filesystem view may place a spelling af's proved elsewhere
// (PendingMismatch), or the holder's working directory may place a relative
// spelling af cannot (PendingRelative). The sweep names its holders the way
// it names any row's — an fd-table scan — and leaves the verdict to the
// caller: a holder af cannot name, or whose view cannot be established, is
// a possible owner, never a rule-out.
type PendingRow struct {
	Inode    string
	Spelling string
	Kind     PendingKind
	Holders  []int // attributed holders; empty = unattributed
}

// PendingKind is what a pending row waits on.
type PendingKind int

const (
	// PendingMismatch: af's filesystem proves the spelling names a different
	// socket; it may be ruled out only when every attributed holder provably
	// shares af's mount namespace and root.
	PendingMismatch PendingKind = iota
	// PendingRelative: the spelling is relative; a holder's working directory
	// may resolve it to the queried path.
	PendingRelative
)

// UnixSocketSweep is what the kernel's own unix-socket tables say about which
// same-uid processes hold a socket bound to a queried path (#7695). It
// answers the socket-absent probe's ownership question without consulting a
// task's title: a socket bound to a filesystem path is a row in the socket
// table of the network namespace it was bound in — with its bound spelling
// and inode, surviving an unlink of the file — so the tables themselves,
// grouped by netns across every live same-uid process, decide who could hold
// it. A process is ruled out by the complete table of its netns: any socket
// it can hold appears there whether or not its fd table is readable —
// measured on Linux 6.17, a non-dumpable same-uid process's
// /proc/<pid>/net/unix reads in full even though /proc/<pid>/fd and
// /proc/<pid>/exe refuse. No process becomes a possible owner without a
// positive tie to the path.
type UnixSocketSweep struct {
	// Bound maps a pid to the kernel's own spelling of a socket it holds
	// whose path is certainly the queried one — a positive tie.
	Bound map[int]string
	// Uncertain maps a pid to a bound spelling whose match the caller could
	// neither prove nor disprove — a possible tie, never a certain one.
	Uncertain map[int]string
	// UnattributedBound counts certain-match rows whose holder no same-uid
	// pid could be named for: an other-uid, non-dumpable, out-of-pidns,
	// SCM_RIGHTS-buffered, or raced-away holder. A tie whose holder cannot
	// be named.
	UnattributedBound int
	// UnattributedUncertain is UnattributedBound for possible-match rows.
	UnattributedUncertain int
	// Uncovered lists live same-uid pids whose network namespace could not
	// be determined and whose own socket table could not be read — the
	// processes the negative cannot be proved for. It does not include
	// pids proved to have exited (a zombie holds no sockets: readProc's
	// Z/X answer, or the kernel's ESRCH however the read met it) or kernel
	// threads (no userspace sockets, and same-uid only to an af that runs
	// as root).
	Uncovered []int
	// Unprobed is true when the sweep itself could not run at all.
	Unprobed bool

	// Pending lists the rows match deferred to holder evidence — a spelling
	// af's own filesystem proved elsewhere, or one it cannot place — each
	// with the holders attribution named. The caller decides each one;
	// nothing here has already counted it.
	Pending []PendingRow
	// ScanComplete is false when the fd-table attribution stopped on the
	// sweep budget before every same-uid pid was scanned: the holder lists
	// are then partial, and a pending row may not be ruled out — the
	// binder nobody reached could be the one whose view differs.
	ScanComplete bool

	// SameUID counts the live same-uid pids enumerated; Netns the distinct
	// covered network namespaces; Gone the enumerated pids proved to have
	// exited mid-sweep; Rows the bound-path rows match was asked about.
	SameUID int
	Netns   int
	Gone    int
	Rows    int
}

// socketSweepBudget bounds the whole sweep, so the fd-table attribution walk
// it ends with gets whatever the enumeration and table parses left over. The
// path that pays the fd scan is already the rare one — it runs only when a
// bound row exists — and its answer degrades to unattributed, never to
// unclaimed, when the budget is spent.
const socketSweepBudget = 45 * time.Millisecond

// socketSweepScanWorkers bounds the fd-scan parallelism: the work is pure
// syscall latency, so a small pool covers the 800-pid case a serial walk
// (~85 ms measured at 275 pids, ~29 ms) could not.
const socketSweepScanWorkers = 8

// matchedRow is one bound-path table row the caller's match accepted, keyed
// by socket inode. kind is the match's own verdict: certain and possible
// rows are decided; mismatch and relative rows wait on the holder evidence
// Pending carries.
type matchedRow struct {
	kind     rowKind
	spelling string
}

// rowKind is what a matched row already knows.
type rowKind int

const (
	rowPossible rowKind = iota
	rowCertain
	rowMismatch
	rowRelative
)

// netnsKey is the identity of a process's network namespace: the
// (device, inode) pair of its /proc/<pid>/net/unix file. /proc/<pid>/ns/net
// would name the same namespace but is unreadable where this stat succeeds
// (measured on Linux 6.17 for a non-dumpable same-uid process), so it is not
// the key.
type netnsKey struct{ dev, ino uint64 }

// UnixSocketOwnerSweep censuses which same-uid processes hold a unix socket
// bound to a queried path, from the kernel's socket tables (#7695). match is
// asked once per bound-path table row — every row with a Path column,
// '@'-led ones included, since a verbatim filesystem spelling "@name" is
// indistinguishable from an abstract address and only the match can decide.
//
// The determinate "nobody holds it" — an empty result with no Uncovered and
// Unprobed false — rests on coverage: every netns a live same-uid process
// can occupy produced a complete parse. A live pid whose netns cannot be
// named and whose own table cannot be read is uncovered; one proved gone,
// or a kernel thread, can hold nothing either way.
//
// Attribution scans every live same-uid pid's fd table rather than only the
// pids grouped under a matched row's netns: an SCM_RIGHTS-passed descriptor
// lets a process in one netns hold a socket bound in another, and the scan
// is the only witness to that.
func UnixSocketOwnerSweep(match func(boundPath string) RowMatch) UnixSocketSweep {
	s := UnixSocketSweep{Bound: map[int]string{}, Uncertain: map[int]string{}}
	start := time.Now()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		s.Unprobed = true
		return s
	}
	uid := os.Getuid()
	groups := map[netnsKey][]int{}
	// loosePids are live same-uid pids with no netns key: ones whose table
	// read directly (covered — each entry in looseTables) and ones it could
	// not (uncovered). Attribution scans them all: each could hold a matched
	// inode in a netns the sweep cannot name for it.
	var loosePids []int
	var looseTables []looseTable
	for _, e := range ents {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid <= 0 {
			continue
		}
		procDir := "/proc/" + e.Name()
		fi, serr := os.Stat(procDir)
		if serr != nil {
			continue // vanished before its uid could be read: holds nothing now
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != uid {
			continue
		}
		s.SameUID++ // every same-uid entry the census had to classify
		nfi, nerr := os.Stat(procDir + "/net/unix")
		if nerr != nil {
			if data, rerr := os.ReadFile(procDir + "/net/unix"); rerr == nil {
				// Covered: the table read even though its key could not be
				// statted. The netns cannot be named, so the rows live under
				// a per-pid key and attribution scans every unplaced pid.
				loosePids = append(loosePids, pid)
				looseTables = append(looseTables, looseTable{data: data, pids: []int{pid}})
				continue
			}
			p, lerr := readProc(pid)
			switch {
			case errors.Is(lerr, ErrProcessExited),
				errors.Is(lerr, syscall.ESRCH),
				errors.Is(lerr, os.ErrNotExist) && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH):
				// A zombie's net directory is empty and it holds nothing:
				// proved gone, ruled out like a pid the table read watched
				// exit. The kernel's ESRCH is the same proof however the
				// /proc read met it — ENOENT still wants kill(pid,0) to
				// agree, since hidepid can fake an absent entry (#7695 R3).
				s.Gone++
			case lerr != nil:
				s.Uncovered = append(s.Uncovered, pid)
			case p.KernelThread:
				// Covered by fiat: a kernel thread binds no unix sockets.
			default:
				s.Uncovered = append(s.Uncovered, pid)
				loosePids = append(loosePids, pid)
			}
			continue
		}
		nst, ok := nfi.Sys().(*syscall.Stat_t)
		if !ok {
			s.Uncovered = append(s.Uncovered, pid)
			loosePids = append(loosePids, pid)
			continue
		}
		key := netnsKey{uint64(nst.Dev), nst.Ino}
		groups[key] = append(groups[key], pid)
	}

	// matchedInodes collects every table row match accepted, by socket
	// inode (sockfs inodes are unique system-wide, so two netns spellings
	// for one socket cannot collide). Attribution runs below.
	matchedInodes := map[string]matchedRow{}
	for _, pids := range groups {
		var data []byte
		var rerr error
		for _, pid := range pids {
			data, rerr = os.ReadFile(fmt.Sprintf("/proc/%d/net/unix", pid))
			if rerr == nil {
				break
			}
			if !errors.Is(rerr, os.ErrNotExist) && !errors.Is(rerr, syscall.ESRCH) {
				break // a real failure; ENOENT/ESRCH mean this member died — try the next
			}
		}
		if rerr != nil {
			s.Uncovered = append(s.Uncovered, pids...)
			loosePids = append(loosePids, pids...)
			continue
		}
		s.Netns++
		s.addRows(data, pids, match, matchedInodes)
	}
	for _, lt := range looseTables {
		s.Netns++
		s.addRows(lt.data, lt.pids, match, matchedInodes)
	}
	if len(matchedInodes) == 0 {
		return s
	}

	// Attribution: scan every live same-uid pid's fd table for
	// socket:[inode] links naming the matched inodes. A pid that holds one
	// is Bound when its row was certain, Uncertain when merely possible —
	// Bound wins when both. A row no fd table names stays unattributed.
	// Mismatch and relative rows are not bucketed here at all: they leave
	// with their holders in Pending, for the caller to decide.
	deadline := start.Add(socketSweepBudget)
	var scanPids []int
	for _, pids := range groups {
		scanPids = append(scanPids, pids...)
	}
	scanPids = append(scanPids, loosePids...)
	certain, possible, holders, complete := socketInodeHolders(scanPids, matchedInodes, deadline)
	s.ScanComplete = complete
	for pid, spelling := range certain {
		s.Bound[pid] = spelling
	}
	for pid, spelling := range possible {
		if _, ok := s.Bound[pid]; !ok {
			s.Uncertain[pid] = spelling
		}
	}
	for ino, row := range matchedInodes {
		if row.kind == rowMismatch {
			s.Pending = append(s.Pending, PendingRow{
				Inode: ino, Spelling: row.spelling, Kind: PendingMismatch, Holders: holders[ino],
			})
			continue
		}
		if row.kind == rowRelative {
			s.Pending = append(s.Pending, PendingRow{
				Inode: ino, Spelling: row.spelling, Kind: PendingRelative, Holders: holders[ino],
			})
			continue
		}
		if len(holders[ino]) > 0 {
			continue
		}
		if row.kind == rowCertain {
			s.UnattributedBound++
		} else {
			s.UnattributedUncertain++
		}
	}
	return s
}

// looseTable is a socket table read for a pid whose netns key could not be
// statted, with the pids the table was read for (and so covers).
type looseTable struct {
	data []byte
	pids []int
}

// addRows parses one socket table and adds each row match accepted to the
// sweep's matched-inode set. A table that fails to parse is "could not
// tell", never "no bound rows": its pids are uncovered.
func (s *UnixSocketSweep) addRows(data []byte, pids []int, match func(string) RowMatch, matched map[string]matchedRow) {
	rows, err := parseNetUnixBoundRows(data)
	if err != nil {
		s.Uncovered = append(s.Uncovered, pids...)
		return
	}
	for _, row := range rows {
		s.Rows++
		switch m := match(row.path); m {
		case RowMatchNone:
		case RowMatchMismatch:
			matched[row.inode] = matchedRow{kind: rowMismatch, spelling: row.path}
		case RowMatchRelative:
			matched[row.inode] = matchedRow{kind: rowRelative, spelling: row.path}
		case RowMatchCertain:
			matched[row.inode] = matchedRow{kind: rowCertain, spelling: row.path}
		default:
			matched[row.inode] = matchedRow{kind: rowPossible, spelling: row.path}
		}
	}
}

// netUnixBoundRow is one filesystem-bound row of a /proc/<pid>/net/unix
// table: `Num RefCount Protocol Flags Type St Inode [Path]`.
type netUnixBoundRow struct {
	inode     string
	path      string
	listening bool
}

// parseNetUnixBoundRows returns every row whose Path column is non-empty.
// Bound rows of every state count — a bound socket that never listened
// still holds the path — with the __SO_ACCEPTCON flag kept for logging.
// '@'-led rows are kept too: an abstract socket's printed "@name" is
// indistinguishable from a filesystem path spelled "@name" verbatim, and a
// filesystem row that is missed is a bound owner nobody counted — the
// false negative the sweep exists to prevent (#7695). The caller's match
// decides every row; an abstract name passes it only for a path spelled
// the same, which is as possible a tie as it is a certain one. Anything
// not shaped like the table is an error, so a format change reads
// "could not tell".
func parseNetUnixBoundRows(data []byte) ([]netUnixBoundRow, error) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Num") {
		return nil, errors.New("/proc/net/unix: unexpected header")
	}
	var rows []netUnixBoundRow
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields, rest := leadingFields(line, 7)
		if len(fields) < 7 {
			return nil, fmt.Errorf("/proc/net/unix: short row %q", line)
		}
		if rest == "" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("/proc/net/unix: bad flags in row %q", line)
		}
		rows = append(rows, netUnixBoundRow{
			inode:     fields[6],
			path:      rest,
			listening: flags&soAcceptConnLinux != 0,
		})
	}
	return rows, nil
}

// socketInodeHolders scans /proc/<pid>/fd for socket:[inode] links naming
// rows' inodes, returning the pids holding a certain-match inode, the pids
// holding only a possible-match one, and the holder lists every matched
// inode built — the list is what Pending reports for a row the match
// deferred. complete is true only when every pid's fd table was walked; a
// truncated scan names fewer holders and a pending row is never ruled out
// on it. The walk is chunked and deadline-bound like listenerHolders': an
// fd that raced away is skipped, and an unreadable fd table or a spent
// budget just names fewer holders — never unproves a bound row, which
// stands on the table alone.
func socketInodeHolders(pids []int, rows map[string]matchedRow, deadline time.Time) (certain, possible map[int]string, holders map[string][]int, complete bool) {
	certain, possible, holders = map[int]string{}, map[int]string{}, map[string][]int{}
	complete = true
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan int, len(pids))
	for _, pid := range pids {
		work <- pid
	}
	close(work)
	worker := func() {
		defer wg.Done()
		for pid := range work {
			if time.Now().After(deadline) {
				mu.Lock()
				complete = false
				mu.Unlock()
				return
			}
			scanPidSocketInodes(pid, rows, deadline, func(inode string, row matchedRow) {
				mu.Lock()
				if !slices.Contains(holders[inode], pid) {
					holders[inode] = append(holders[inode], pid)
				}
				if row.kind == rowCertain {
					certain[pid] = row.spelling
				} else if row.kind == rowPossible {
					if _, ok := certain[pid]; !ok {
						possible[pid] = row.spelling
					}
				}
				mu.Unlock()
			})
		}
	}
	for range min(socketSweepScanWorkers, len(pids)) {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return certain, possible, holders, complete
}

// scanPidSocketInodes readlinks one pid's fd table, chunked, reporting each
// held inode rows names. Every matched inode is reported, never just the
// first: one process holds every socket to the same path — a listener and
// each connection accepted on it share the bound spelling — and an unmarked
// inode counts as an unattributed bound row, a false "holder nobody could
// name" over a socket whose holder was found.
func scanPidSocketInodes(pid int, rows map[string]matchedRow, deadline time.Time, found func(inode string, row matchedRow)) {
	fdDir, err := os.Open(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return
	}
	defer fdDir.Close()
	for {
		if time.Now().After(deadline) {
			return
		}
		fds, rerr := fdDir.ReadDir(256)
		for _, fd := range fds {
			target, lerr := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if lerr != nil {
				continue // the fd raced away — not a blind spot
			}
			ino, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			ino, ok = strings.CutSuffix(ino, "]")
			if !ok {
				continue
			}
			if row, ok := rows[ino]; ok {
				found(ino, row)
			}
		}
		if errors.Is(rerr, io.EOF) || rerr != nil {
			return
		}
	}
}
