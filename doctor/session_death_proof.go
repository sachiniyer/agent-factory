package doctor

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Everything `af doctor` can prove about a marked process's session DEATH —
// separated from tmux_sessions.go for the file-length lint (#1145), but the
// grouping is the real point: this is the only place absence is promoted to
// death, and it spends that promotion on a SIGKILL. Two evidence channels
// feed it — the process's own TMUX env, corroborated against a closed list of
// proof shapes, and the daemon's server-state record, corroborated by a union
// listing over every socket generation it names. Anything short of a proof
// shape is reported, never killed (#6339).

// sessionDeathVerdict is what doctor can prove about a marked process's
// session once that session is absent from the union listing — the standard
// the orphaned-process kill arm must meet before it arms a kill.
type sessionDeathVerdict int

const (
	// deathProven: the session is dead on the server that hosted it AND every
	// socket generation the daemon has used is accounted for — absence reads
	// as death, so the kill arm may fire.
	deathProven sessionDeathVerdict = iota
	// deathUnproven: doctor cannot corroborate the session's death — the
	// record could not be proven current, the socket hosting it could not be
	// listed, or no evidence names the server it lived on. The process is
	// reported, never killed.
	deathUnproven
	// deathLiveElsewhere: the session is absent from the union listing but
	// live on the socket the process's own TMUX env names — verified by
	// listing THAT socket and finding it there. socket carries where.
	deathLiveElsewhere
)

// recordedServerCurrent reports whether the daemon's server-state record was
// written by the daemon generation currently serving this home — the
// precondition for trusting the record's socket set as complete (#6339).
//
// The record is stamped with its author's pid (ServerState.DaemonPID). When a
// daemon IS serving this home, the stamped pid must be one of the live daemon
// pids: a mismatch — or no stamp at all — means a previous generation left
// the record behind, and a record a dead generation wrote may name a socket
// the live daemon no longer uses. When NO daemon is live, a stamped record is
// still the last word a daemon generation left — its socket set is the best
// accounting there is, so it stands; an UNSTAMPED record predates generation
// binding entirely and cannot be proven to account for every socket the
// daemon ran, so it does not corroborate a kill on its own.
func (c *scanContext) recordedServerCurrent() bool {
	st, ok, err := c.recordedServer()
	if err != nil || !ok {
		return false
	}
	daemons := activeHomeDaemons(c)
	if len(daemons) == 0 {
		return st.DaemonPID != 0
	}
	for _, d := range daemons {
		if st.DaemonPID == d.PID {
			return true
		}
	}
	return false
}

// sessionDeathProof decides whether the orphaned-process arm may treat a
// marked process's session as DEAD — absent from the union listing — and arm
// its kill. The union covers every socket the record names, but absence there
// is still not proof by itself: the record may not be current (a restart that
// adopted a server used to leave the previous generation's socket recorded,
// #6339), and a live session hosted on a socket outside the record's set
// reads exactly like a dead one here.
//
// So the proof asks the process itself first: tmux stamps every pane process
// with TMUX="socketPath,serverPID,sessionIdx", naming the server its session
// lived on. The env value is corroborated against a CLOSED LIST of proof
// shapes (see tmuxEnvDeathProof) — an uncertain state is reported, never
// spent on a kill. Only when the process carries no usable TMUX env does
// the proof fall back to the record: it corroborates a kill only when the
// record is current AND every socket it names — recorded plus prior
// generations — listed the session absent, which is exactly what absence from
// the union map means.
func (c *scanContext) sessionDeathProof(p proctree.Process, name string) (sessionDeathVerdict, string) {
	tmuxEnv, status := proctree.LookupEnv(p.PID, "TMUX")
	if status == proctree.EnvFound && tmuxEnv != "" {
		if verdict, socket, decided := c.tmuxEnvDeathProof(p, name, tmuxEnv); decided {
			return verdict, socket
		}
	}
	_, kind := c.recordedSocket()
	if kind != recordedSocketPinned {
		return deathUnproven, ""
	}
	if !c.recordedServerCurrent() {
		return deathUnproven, ""
	}
	// The union listing succeeded — this arm only runs after it did — so it
	// has already listed every socket the record names. A name still absent
	// is absent from every socket generation the daemon has used.
	live, err := c.tmuxSessions()
	if err != nil {
		return deathUnproven, ""
	}
	if sockets, found := live[name]; found {
		return deathLiveElsewhere, sockets[0]
	}
	// A record whose socket history hit its cap has DROPPED a generation that
	// may still host the session; absence from the sockets it retains is not
	// complete coverage, so the union cannot corroborate a kill.
	if st, ok, rerr := c.recordedServer(); rerr != nil || !ok || st.HistoryTruncated {
		return deathUnproven, ""
	}
	return deathProven, ""
}

// stampedServerState is the verdict on the server pid a TMUX env value stamps.
// The verdict is only ever computed when the stamped number is provably in
// doctor's own pid namespace — outside it the number names a different
// allocation domain entirely and is never consulted.
type stampedServerState int

const (
	// stampedServerUnknown means the pid could not be verified either way —
	// a denied /proc read, a raced identity check, a snapshot gap. Unknown is
	// never death evidence.
	stampedServerUnknown stampedServerState = iota
	// stampedServerLive means a verifiably tmux process holds the pid — the
	// same (pid, start-stamp) instance the snapshot recorded is still running,
	// or a fresh /proc read shows a tmux there.
	stampedServerLive
	// stampedServerDead means the pid is provably gone — the stamped instance
	// exited — or replaced, the pid now verifiably naming a different process.
	// The server is dead and the session died with it.
	stampedServerDead
)

// tmuxEnvDeathProof is the CLOSED LIST of death-proof shapes a process's own
// TMUX env ("socketPath,serverPID,sessionIdx") can yield. A kill arms only on:
//
//	(A) the stamped server pid is verifiably ALIVE in this pid namespace — a
//	    tmux process still holds it — AND the socket answers AND is answered
//	    by that very pid AND the session is absent. The answerer check is
//	    load-bearing: a socket path can outlive or be rebound by a different
//	    server than the one that created it — an orphaned tmux server keeps
//	    its sessions alive after its socket file is unlinked, and a later
//	    server can bind the freed path — so a listing's "absent" answer is
//	    evidence only about the server that gave it.
//	(B) the stamped server pid is proven GONE or REPLACED in this namespace —
//	    the process exited or the pid now names a different process — so the
//	    server is dead and the session died with it.
//
// EVERYTHING ELSE IS UNPROVEN — reported, never killed: a pid whose /proc
// read could not confirm it gone, a marked process outside doctor's pid
// namespace (whose stamped number refers to a different allocation domain),
// a socket that cannot be listed, a socket answered by any process but the
// stamped one, and a "no server running" answer — an orphaned server keeps
// its sessions while refusing every connection.
//
// Liveness is different: a session the socket DOES list is positive evidence
// — reported live elsewhere, routed to the escaped arm — because "I saw it"
// is a fact, while "I didn't see it" is only ever evidence inside the list.
func (c *scanContext) tmuxEnvDeathProof(p proctree.Process, name, tmuxEnv string) (sessionDeathVerdict, string, bool) {
	procSocket, rest, _ := strings.Cut(tmuxEnv, ",")
	serverPID, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, ",", 2)[0]))
	procSocket = strings.TrimSpace(procSocket)
	if procSocket == "" {
		return deathUnproven, "", false
	}
	if serverPID > 0 && c.samePIDNamespace(p.PID) {
		switch c.stampedServerVerdict(serverPID) {
		case stampedServerDead:
			// (B) — the server the env names is provably gone or replaced in
			// this namespace; the session died with it, whatever the socket
			// now answers.
			return deathProven, "", true
		case stampedServerLive:
			l := c.socketProbe(procSocket)
			switch {
			case l.err != nil || l.serverPID != serverPID:
				// No listing, or a listing from a stranger: the socket path
				// is not bound to the verified server, so its answer — or
				// its silence — proves nothing about the stamped server's
				// sessions.
				return deathUnproven, "", true
			case slices.Contains(l.names, name):
				return deathLiveElsewhere, procSocket, true
			default:
				// (A) — the verified server answered its own socket and the
				// session is absent from it.
				return deathProven, "", true
			}
		}
		// stampedServerUnknown — the pid shapes proved nothing; fall through
		// to the listing's positive half.
	}
	// Off the closed list — the pid either could not be placed in this
	// namespace or its verdict was unknown. The socket's answer still gets one
	// look, but only its positive half: a session it lists is a live session.
	if l := c.socketProbe(procSocket); l.err == nil && slices.Contains(l.names, name) {
		return deathLiveElsewhere, procSocket, true
	}
	return deathUnproven, "", true
}

// stampedServerVerdict reports whether the pid a TMUX env value stamps is
// verifiably the tmux server it named (live), verifiably not that server
// anymore (dead — exited or recycled), or unverifiable. Callers must have
// established the stamped number is in doctor's pid namespace first; the
// verdict means nothing otherwise.
//
// "Verifiably" uses proctree's instance identity, not bare presence: a pid in
// the snapshot is re-validated through SameIdentity — the (pid, start-stamp)
// pair, so a pid recycled between the snapshot and this read is caught — and a
// pid absent from the snapshot is looked up in /proc directly, where only a
// definitive no-such-process is gone; every other read failure is unknown.
func (c *scanContext) stampedServerVerdict(serverPID int) stampedServerState {
	if entry, inSnap := c.snap[serverPID]; inSnap {
		same, err := proctree.SameIdentity(entry)
		switch {
		case err != nil:
			return stampedServerUnknown
		case !same:
			// The instance the snapshot recorded at this pid exited or was
			// recycled — the stamped server is not there anymore.
			return stampedServerDead
		case !strings.HasPrefix(entry.Comm, "tmux"):
			// The pid verifiably names a live process that is not tmux: the
			// stamped server was replaced outright.
			return stampedServerDead
		default:
			return stampedServerLive
		}
	}
	cur, err := proctree.Lookup(serverPID)
	switch {
	case err == nil:
		if strings.HasPrefix(cur.Comm, "tmux") {
			// A live tmux holds the stamped pid despite the snapshot gap —
			// verify its socket answer like any live server.
			return stampedServerLive
		}
		return stampedServerDead
	case errors.Is(err, proctree.ErrProcessExited) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH):
		// /proc answered definitively: no process instance owns this pid.
		return stampedServerDead
	default:
		// The read failed without proving absence — a denied or partial
		// /proc answers "could not tell", not "gone".
		return stampedServerUnknown
	}
}

// samePIDNamespace reports whether pid lives in doctor's own PID namespace —
// the condition under which the serverPID a process's TMUX env stamps is a
// number in the SAME allocation domain as doctor's snapshot. tmux stamps the
// server's pid as the server sees it, so the env value is only meaningful in
// the marked process's namespace; comparing that namespace to doctor's (the
// marked process and doctor share a /proc view) is what keeps a
// cross-namespace doctor from reading a foreign pid as local evidence.
// Anything unreadable is unproven — false.
func (c *scanContext) samePIDNamespace(pid int) bool {
	self, errSelf := pidNamespaceIDOf(os.Getpid())
	theirs, errTheirs := pidNamespaceIDOf(pid)
	return errSelf == nil && errTheirs == nil && self == theirs
}

// pidNamespaceIDOf maps a process to its PID-namespace identity — Linux reads
// the inode of /proc/<pid>/ns/pid; platforms without PID namespaces put every
// process in one allocation domain, so doctor's own identity stands in. It is
// a var so tests can stage a process outside doctor's namespace.
var pidNamespaceIDOf = func(pid int) (string, error) {
	if runtime.GOOS != "linux" {
		return proctree.PIDNamespaceID()
	}
	info, err := os.Stat(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return "", fmt.Errorf("/proc/%d/ns/pid: missing inode identity", pid)
	}
	return fmt.Sprintf("pidns:%x:%x", uint64(stat.Dev), stat.Ino), nil
}

// sameSocket reports whether two tmux socket paths identify the same socket.
// Compares by filesystem identity (os.SameFile) when both stat, so a symlinked
// TMUX_TMPDIR — or macOS /var vs /private/var — does not split one socket into
// two; falls back to a cleaned lexical comparison when either path cannot be
// statted (a dead server leaves no socket file to stat, and the comparison
