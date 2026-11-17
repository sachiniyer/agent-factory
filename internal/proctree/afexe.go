package proctree

import (
	"debug/buildinfo"
	"time"
)

// afModulePath is the Go module path a kernel-attested executable must carry
// in its build info to count as an agent-factory binary (#7491). Any build of
// this repo — the installed af, a source-built agent-factory, a test binary —
// carries it; a foreign executable renamed to "af" does not, which is exactly
// the forgery `exec -a af … --daemon` mounts: it can write argv and comm but
// not the module path baked into the executable the kernel links.
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
//   - the exe path comes from the KERNEL (readlink /proc/<pid>/exe, or
//     proc_info(PROC_PIDPATHINFO) on darwin), not from argv[0] — exec -a can
//     write argv[0] but cannot write the link;
//   - the file at that path is an af build iff its embedded Go build info
//     names this module — `cp /bin/bash ./af` passes an af-named path and
//     fails here, while a real af build verifies at ANY path, including
//     another home's daemon (the #7484 R18 shape);
//   - the read is revalidated against the snapshot's StartID: the exe read
//     happens outside the snapshot, so a pid recycled between them must not
//     carry one process's verdict onto the next;
//   - the whole check is budget-bounded: an expired wait reports false.
func ExeAttestsAF(p Process) bool {
	done := make(chan bool, 1)
	go func() {
		done <- exeAttestsAF(p)
	}()
	timer := time.NewTimer(exeAttestBudget)
	defer timer.Stop()
	select {
	case ok := <-done:
		return ok
	case <-timer.C:
		return false
	}
}

// exeAttestsAF is the attestation itself, run under exeAttestBudget. A
// leaked goroutine on timeout costs one blocked read per stalled
// attestation and never lands a verdict — a timeout answers unverified.
// exePathFn is the platform kernel read, behind a package var so the budget
// test can stall it deterministically.
var exePathFn = exePath

func exeAttestsAF(p Process) bool {
	path, ok := exePathFn(p.PID)
	if !ok {
		return false
	}
	bi, err := buildinfo.ReadFile(path)
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
