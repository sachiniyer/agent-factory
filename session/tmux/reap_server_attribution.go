package tmux

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// ServerIdentity records which tmux server a session's pane was last resolved
// against (#7469): the socket the server answered on, the server's (pid,
// StartID) process instance, the session's $N id, and the pid-namespace the
// record was made in. The vanished-session sweep compares a detached marked
// carrier against exactly this: a carrier whose last tmux server is provably
// NOT this one is descended from a foreign server, which #7469 forbids
// signalling.
//
// Socket is stored as #{socket_path} reported it — canonicalization happens
// at compare time, against the carrier's own spelling, so a path that only
// made sense to the recording process is still this record's truth.
type ServerIdentity struct {
	Socket    string
	PID       int
	StartID   uint64
	SessionID string
	PIDNS     string
}

// valid reports whether the identity can compare a carrier at all. Socket and
// PID are the minimum an env or pty channel can name a different server
// against; StartID and SessionID strengthen the compares but the record is
// still usable without them. An empty record — sessions launched before this
// existed, stamps tmux could not answer — yields undecidable everywhere,
// which is today's verdict.
func (id ServerIdentity) valid() bool { return id.Socket != "" && id.PID > 0 }

// serverVerdict is what the attribution channels learned about one carrier:
// the tmux server it last belonged to is our recorded one (own), provably a
// different one (foreign), or could not be named (undecidable — the verdict
// the carrier already had, never a new refusal).
type serverVerdict int

const (
	serverVerdictUndecidable serverVerdict = iota
	serverVerdictOwn
	serverVerdictForeign
)

// tmuxEnvVar is the environment variable tmux writes into every pane process:
// "<socket>,<server pid>,<session index>". A carrier inherits it unchanged —
// re-parenting and setsid never rewrite /proc/<pid>/environ — so it is a
// durable claim about which server spawned the carrier, until the carrier
// scrubs it. The claim is judged against the recorded identity, never
// trusted: a spoofed value can only buy the carrier today's eligible verdict,
// never membership (the own arm below changes nothing tier 2 then decides).
const tmuxEnvVar = "TMUX"

// carrierServerVerdict attributes a marked, unproven candidate to a tmux
// server through the two channels measured to survive detachment (#7469):
//
//   - its TMUX environment — names the server directly (socket, pid, session
//     index), judged against the recorded identity;
//   - its held pty slaves — every /dev/pts/N (linux) or /dev/ttysNNN (darwin)
//     fd names a pty pair whose master a LIVE, positively-identified tmux
//     server holds, and that holder is compared against the recorded one.
//
// Combine: any foreign attribution wins; else any own; else undecidable.
// Foreign is returned only on a POSITIVE mismatch — a different socket, a
// different live server pid holding the carrier's pty master, or a recycled
// server pid whose StartID no longer matches. Every comparison that cannot
// be made (missing env, unreadable fds, absent record, pid-namespace
// mismatch, a holder that is not a positively-identified server — the #3699
// re-host is exactly that) is undecidable, which the caller treats as the
// verdict the carrier already had.
func carrierServerVerdict(p proctree.Process, environ []string, own ServerIdentity,
	snap map[int]proctree.Process, owners *serverPtyOwners, selfNS string) serverVerdict {
	if !own.valid() {
		return serverVerdictUndecidable
	}
	tmuxEnv, _ := processEnvValue(environ, tmuxEnvVar)
	verdict := tmuxEnvServerVerdict(tmuxEnv, own, selfNS)
	if v := ptyFdServerVerdict(p.PID, own, snap, owners, selfNS); v == serverVerdictForeign {
		return serverVerdictForeign
	} else if verdict == serverVerdictUndecidable {
		verdict = v
	}
	return verdict
}

// tmuxEnvServerVerdict judges a TMUX env value against the recorded server.
// "socket,pid,idx" — three comma fields, a format doctor already parses for
// its own kill arm (tmuxServerDead, checks_processes.go). Anything that does
// not parse cleanly, or whose socket cannot be compared certainly, is
// undecidable — never foreign on a malformed claim.
func tmuxEnvServerVerdict(tmuxEnv string, own ServerIdentity, selfNS string) serverVerdict {
	parts := strings.Split(tmuxEnv, ",")
	if len(parts) != 3 {
		return serverVerdictUndecidable
	}
	same, comparable := sameRecordedSocket(parts[0], own.Socket)
	if !comparable {
		return serverVerdictUndecidable
	}
	if !same {
		return serverVerdictForeign
	}
	// The pid number the env records names the server in the writer's pid
	// namespace — meaningful to compare only when the record and this sweep
	// agree on which namespace that is.
	if own.PIDNS != "" && selfNS != "" && own.PIDNS != selfNS {
		return serverVerdictUndecidable
	}
	serverPID, err := strconv.Atoi(parts[1])
	if err != nil || serverPID <= 0 {
		return serverVerdictUndecidable
	}
	if serverPID != own.PID {
		// Same socket, different server pid: a different generation answers
		// on this socket, or the env names a server that is not the
		// recorded one — a foreign server either way.
		return serverVerdictForeign
	}
	// The session index corroborates the own claim only: a mismatch means
	// the carrier was spawned under a different session on our server —
	// foreign to this cohort but NOT to the server, which is the boundary
	// this verdict draws. Undecidable: the generation markers are the
	// incarnation authority, not this field.
	if own.SessionID != "" && "$"+parts[2] != own.SessionID {
		return serverVerdictUndecidable
	}
	return serverVerdictOwn
}

// sameRecordedSocket compares the carrier's env socket spelling against the
// recorded one the way the socket-binding code does (#7533): canonicalized
// kernel-walk equality or the same-directory fallback. comparable is false
// when either side cannot be made certain — a relative path, a symlink loop —
// and nothing may be ruled out from it.
func sameRecordedSocket(envSocket, recordedSocket string) (same, comparable bool) {
	if !filepath.IsAbs(envSocket) || !filepath.IsAbs(recordedSocket) {
		return false, false
	}
	want, okA := canonicalSocketPath(recordedSocket)
	got, okB := canonicalSocketPath(envSocket)
	if !okA || !okB {
		return false, false
	}
	if got == want || sameSocketDir(envSocket, recordedSocket) {
		return true, true
	}
	return false, true
}

// ptyFdServerVerdict attributes the carrier through the pty slaves it holds.
// Each index names a pty pair; the map — built once per sweep from the live
// table — says which positively-identified tmux server holds its master. A
// slave no server masters (the bg-pty-host re-host shape #3699 needs, a
// user's screen, an sshd session) yields no verdict, and a server that could
// not be read contributes nothing it cannot prove.
func ptyFdServerVerdict(pid int, own ServerIdentity, snap map[int]proctree.Process,
	owners *serverPtyOwners, selfNS string) serverVerdict {
	if owners == nil {
		return serverVerdictUndecidable
	}
	indices, err := ptySlaveIndices(pid)
	if err != nil || len(indices) == 0 {
		return serverVerdictUndecidable
	}
	m := owners.forSnapshot(snap)
	verdict := serverVerdictUndecidable
	for _, key := range indices {
		holder, ok := m[key]
		if !ok {
			continue
		}
		switch v := compareHeldServer(snap, holder, own, selfNS); v {
		case serverVerdictForeign:
			return serverVerdictForeign
		case serverVerdictOwn:
			verdict = serverVerdictOwn
		}
	}
	return verdict
}

// compareHeldServer judges the LIVE positively-identified server holding the
// carrier's pty master against the recorded identity. Pid equality alone is
// not the own verdict — the pid may be recycled to a different server — so
// the recorded StartID gates it where recorded; and a pid-namespace mismatch
// makes the numeric compare meaningless, never foreign.
func compareHeldServer(snap map[int]proctree.Process, holder int, own ServerIdentity, selfNS string) serverVerdict {
	if own.PIDNS != "" && selfNS != "" && own.PIDNS != selfNS {
		return serverVerdictUndecidable
	}
	if holder != own.PID {
		return serverVerdictForeign
	}
	held, ok := snap[holder]
	if !ok {
		return serverVerdictUndecidable
	}
	if own.StartID != 0 && held.StartID != own.StartID {
		// The pid is ours by number but the instance is not: the recorded
		// server died and the pid was handed to another server.
		return serverVerdictForeign
	}
	return serverVerdictOwn
}

// serverPtyOwners is the once-per-sweep pty-index → server-pid map the pty
// channel consults. It is built lazily — only when a detached-shape carrier
// reaches the verdict — from the same process-table snapshot the sweep judges
// against, so the attribution cannot disagree with the ancestry that saw the
// same table. Only POSITIVELY identified tmux servers contribute: an
// unreadable or fork-child tmux process is never a server's name here, which
// is the safe direction for the foreign verdict it feeds.
type serverPtyOwners struct {
	built  bool
	byIdx  map[proctree.PtyIndex]int
	server func(pid int) bool
}

func (o *serverPtyOwners) forSnapshot(snap map[int]proctree.Process) map[proctree.PtyIndex]int {
	if o.built {
		return o.byIdx
	}
	o.built = true
	o.byIdx = make(map[proctree.PtyIndex]int)
	server := o.server
	if server == nil {
		server = tmuxServerIdentified
	}
	for pid, proc := range snap {
		// Cheap comm prefilter before the identity check reads the process:
		// a snapshot holds thousands of entries and nearly none are tmux. A
		// staged server predicate (tests) decides on its own.
		if (o.server == nil && !strings.HasPrefix(proc.Comm, "tmux")) || !server(pid) {
			continue
		}
		indices, err := ptyMasterIndices(pid)
		if err != nil {
			// A server whose fds cannot be read names nothing; the carrier
			// it might have mastered stays undecidable.
			continue
		}
		for _, key := range indices {
			if _, taken := o.byIdx[key]; !taken {
				o.byIdx[key] = pid
			}
		}
	}
	return o.byIdx
}

// serverVerdictReason names WHICH evidence produced the foreign verdict, for
// the warning a swept carrier leaves in the log (#7469): the TMUX env's
// socket naming another server, or a pty slave whose master a different live
// server holds.
func serverVerdictReason(environ []string, own ServerIdentity) string {
	tmuxEnv, _ := processEnvValue(environ, tmuxEnvVar)
	parts := strings.Split(tmuxEnv, ",")
	if len(parts) == 3 {
		if same, comparable := sameRecordedSocket(parts[0], own.Socket); comparable && !same {
			return "TMUX environment (socket " + parts[0] + ")"
		}
		if serverPID, err := strconv.Atoi(parts[1]); err == nil && serverPID != own.PID {
			return "TMUX environment (server pid " + parts[1] + ")"
		}
	}
	return "pty descriptors"
}

// ptySlaveIndices and ptyMasterIndices are package vars so a test can stage
// the kernel reads the verdict consumes — the real ones read /proc fd/fdinfo
// on linux and proc_pidfdinfo vnodes on darwin (internal/proctree/ptyindex*).
var (
	ptySlaveIndices  = proctree.PtySlaveIndices
	ptyMasterIndices = proctree.PtyMasterIndices
)
