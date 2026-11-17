package proctree

import (
	"debug/buildinfo"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// afModulePath is the Go module path a kernel-attested executable must carry
// in its build info to count as an agent-factory binary (#7491). Any build of
// this repo — the installed af, a source-built agent-factory, a test binary —
// carries it. This is a guard against RENAMING, not proof of identity: a
// foreign executable renamed to "af" fails because rename cannot rewrite the
// bytes, but the path is chosen at build time — a binary purpose-built with
// our module in its go.mod carries it too (#7494 item 3). That residual cost
// floor — building or borrowing a real af binary — is the one #7484's design
// already accepts; every cheaper forgery (rename, argv, comm) now blocks.
const afModulePath = "github.com/sachiniyer/agent-factory"

// exeAttestBudget bounds the whole attestation — the kernel exe read, the
// buildinfo parse, and the StartID revalidation. A verdict that authorizes
// cleanup must never wait on an unbounded filesystem (an exe hosted on a
// stalled NFS mount is a real shape); running out means UNVERIFIED, which is
// the fail-closed direction. Package var so tests can shrink the wait.
var exeAttestBudget = 2 * time.Second

// ExeAttestsAF reports whether p's kernel-attested executable is a build of
// this repo's agent-factory module — the af-daemon identity a process cannot
// write (#7491 item 1). argv alone still protects a claimant from signals;
// this is what a claim needs before it may also authorize a cleanup verdict:
//
//   - the executable is opened on its INODE, not re-resolved by path:
//     /proc/<pid>/exe opens the running image even after the name is
//     unlinked, and on darwin the PROC_PIDPATHINFO path is re-verified
//     against the text vnode's (dev,ino) in the process's own memory map —
//     a deleted or replaced name can never stand in for the running binary
//     (#7303's reopen-by-path class, #7492);
//   - the file opened that way is an af build iff its embedded Go build
//     info names this module — `cp /bin/bash ./af` passes an af-named path
//     and fails here, while a real af build verifies at ANY path,
//     including another home's daemon (the #7484 R18 shape). The module
//     path is chosen at build time, so this attests "built as the af
//     module", a bar a rename cannot reach — not "the af project built
//     it" (#7494 item 3);
//   - the read is revalidated against the snapshot's StartID: the exe read
//     happens outside the snapshot, so a pid recycled between them must not
//     carry one process's verdict onto the next;
//   - the whole check is budget-bounded: an expired wait reports false.
func ExeAttestsAF(p Process) bool {
	// Read the seam in the CALLER's goroutine before spawning the worker:
	// a worker that outlives its budget must never read a package var a
	// test later restores — the lazy read races the restore (the same
	// shape a racing caller would hit in production, #7491).
	fn := exeOpenFn
	// Attempts on the SAME process instance fly in formation (#7494 item
	// 2): a wedged exe — a hung NFS mount, a FIFO renamed over the exe
	// path — holds at most ONE worker, goroutine and fd no matter how many
	// candidates its pid reaches in one sweep or the next. Each caller
	// still bounds its own wait: an expired budget reports false and the
	// shared read keeps the worker it already paid.
	key := strconv.Itoa(p.PID) + ":" + strconv.FormatUint(p.StartID, 10)
	done := attestFlights.DoChan(key, func() (any, error) {
		return exeAttestsAF(fn, p), nil
	})
	timer := time.NewTimer(exeAttestBudget)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.Val.(bool)
	case <-timer.C:
		return false
	}
}

// attestFlights single-flights the attestation read by (pid, StartID) so a
// stalled executable leaks ONE worker rather than one per attempt (#7494
// item 2). In-flight attempts share a result; a finished attempt is not
// cached here — cross-attempt dedup is the caller's ExeAttestations scope,
// so a wedged-mount verdict can recover on the next sweep.
var attestFlights singleflight.Group

// ExeAttestations memoizes ExeAttestsAF per (pid, StartID) for the scope of
// ONE sweep: a vanished-session reap classifies the same claimant about nine
// times per candidate (the ancestry walk and the candidate loop both meet
// it), and each attempt used to pay the full kernel read, buildinfo parse
// and StartID revalidation — with the full stall budget when the read
// wedges (#7494 item 2). The verdict is instance-stable — a pid re-exec'd
// is a new StartID and re-attests — so a memo by instance is honest, and
// scoping it to the sweep means a wedged-mount false is retried next sweep
// rather than cached forever. A nil *ExeAttestations attests directly, so
// callers without a sweep scope pay nothing extra.
type ExeAttestations struct {
	mu   sync.Mutex
	seen map[attestKey]bool
}

type attestKey struct {
	pid     int
	startID uint64
}

// NewExeAttestations returns the per-sweep attestation memo.
func NewExeAttestations() *ExeAttestations {
	return &ExeAttestations{seen: map[attestKey]bool{}}
}

// AttestsAF is ExeAttestsAF deduplicated per (pid, StartID) for this scope.
func (a *ExeAttestations) AttestsAF(p Process) bool {
	if a == nil {
		return ExeAttestsAF(p)
	}
	k := attestKey{pid: p.PID, startID: p.StartID}
	a.mu.Lock()
	v, ok := a.seen[k]
	a.mu.Unlock()
	if ok {
		return v
	}
	v = ExeAttestsAF(p)
	a.mu.Lock()
	a.seen[k] = v
	a.mu.Unlock()
	return v
}

// exeAttestsAF is the attestation itself, run under exeAttestBudget. A
// leaked goroutine on timeout costs one blocked read per stalled
// attestation and never lands a verdict — a timeout answers unverified.
// exeOpenFn is the platform kernel read, behind a package var so the budget
// test can stall it deterministically; ExeAttestsAF snapshots it before
// spawning so an abandoned read can never see a later write.
var exeOpenFn = exeOpen

func exeAttestsAF(fn func(pid int) (*os.File, bool), p Process) bool {
	f, ok := fn(p.PID)
	if !ok {
		return false
	}
	defer f.Close()
	bi, err := buildinfo.Read(f)
	if err != nil || bi.Main.Path != afModulePath {
		return false
	}
	// Revalidate the instance AFTER the read: the kernel answered for the
	// pid number, and the verdict is only honest for the instance the
	// snapshot saw — a recycled pid returns false like any other unverified
	// claim.
	cur, err := Lookup(p.PID)
	return err == nil && cur.StartID == p.StartID
}
