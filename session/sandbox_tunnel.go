package session

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/watchlifeline"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon-local half of the ssh/sandbox tunnel's lifetime (#5564).
//
// A sandbox tunnel is a long-lived `ssh -L` child of the process that spawned
// it — in production, the af daemon. Its session's own reap deliberately does
// NOT run on daemon exit (a session must survive the daemon that hosts it), so
// without the bindings in this file the child is reparented to init on every
// daemon exit and keeps holding its forwarded port: one leaked ssh process and
// port per live ssh sandbox session per restart.
//
// Three layers bind the tunnel to the daemon that owns it:
//
//  1. THE LIFELINE. The tunnel spawns under `af __watch-lifeline`
//     (internal/watchlifeline, #5329), which leads the tunnel's process group
//     and kills the WHOLE group when the daemon-held lifeline pipe hits EOF —
//     which the kernel produces on the daemon's death by ANY cause, SIGKILL
//     included, on Linux and macOS alike. That makes it strictly stronger than
//     Pdeathsig, which is Linux-only, signals only the direct child, and in Go
//     can fire on parent-THREAD death (golang/go#27505).
//  2. THE REGISTRY + SHUTDOWN STOP. Every provisioner that may hold a tunnel
//     registers in liveSandboxTunnels, and the daemon's deferred
//     StopAllSandboxTunnels stops them all on every exit path — covering
//     tunnels spawned on the no-lifeline fallback and making graceful shutdown
//     synchronous rather than fd-close-eventual.
//  3. THE MARKER + STARTUP SWEEP. Every tunnel child carries
//     AF_SANDBOX_TUNNEL_HOME=<this home> in its initial environment, inherited
//     through the whole group. The next daemon's startup sweep kills the
//     leftovers it can PROVE are this home's — env-marker equality re-verified
//     by (pid, StartID) at signal time — and only REPORTS unmarked
//     af-sandbox-shaped processes (pre-fix leftovers a sweep cannot prove),
//     never kills on argv alone.

const (
	// sandboxTunnelHomeEnv marks a process as an af-spawned sandbox transport
	// belonging to one canonical AF home. The environment is fixed at exec and
	// cannot be shed, and every group member — lifeline, sh, ssh, ProxyCommand
	// helpers — inherits it, so member-wise kills reap the whole group without
	// ever matching a process by its command line.
	sandboxTunnelHomeEnv = "AF_SANDBOX_TUNNEL_HOME"

	// sandboxTunnelLifelineGrace is how long the lifeline waits between its
	// group SIGTERM and SIGKILL once it sees the daemon is gone. ssh exits on
	// SIGTERM immediately; the grace exists for a ProxyCommand that traps it.
	sandboxTunnelLifelineGrace = 2 * time.Second

	// sandboxTunnelLifelineAckTimeout bounds the wait for the lifeline's first
	// ack line. A binary that speaks the protocol acks within milliseconds of
	// exec; the bound only matters for one that neither acks nor exits, which
	// is then killed and the tunnel spawned directly.
	sandboxTunnelLifelineAckTimeout = 10 * time.Second
)

// sandboxTunnelHome is this process's canonical AF home — the value stamped
// into every tunnel child's environment and the value the startup sweep's
// caller compares it against. Canonicalized exactly like the daemon's
// canonicalDir (filepath.Abs + pathutil.ResolveForCompare) so a symlinked or
// relative spelling of the same home compares equal; resolved once because
// AGENT_FACTORY_HOME is fixed at process start.
var sandboxTunnelHome = sync.OnceValues(func() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return pathutil.ResolveForCompare(abs), nil
})

// stampSandboxTunnel marks the command as a sandbox transport this AF home
// owns. A spawn that cannot resolve its home still runs — the marker exists
// for cleanup, not correctness — but is then unsweepable, so the resolution
// failure is said out loud once per spawn rather than silently degrading the
// ownership contract.
func stampSandboxTunnel(cmd *exec.Cmd) {
	home, err := sandboxTunnelHome()
	if err != nil {
		log.WarningLog.Printf("sandbox runtime: cannot resolve the AF home to stamp the tunnel's ownership marker (%v); "+
			"if this daemon dies uncleanly the next one's startup sweep cannot prove the tunnel is af's and will not reap it", err)
		return
	}
	cmd.Env = append(os.Environ(), sandboxTunnelHomeEnv+"="+home)
}

// sandboxTunnelLifelineExe resolves the binary spawned as the tunnel's
// lifeline — this process's own — once per process. A var so a test can point
// it at a binary that does not answer the protocol.
var sandboxTunnelLifelineExe = sync.OnceValue(func() string {
	exe, err := watchlifeline.Exe()
	if err != nil {
		log.WarningLog.Printf("sandbox runtime: cannot resolve the af binary to supervise ssh tunnels (%v); "+
			"tunnels run without a lifeline and rely on graceful-shutdown and startup-sweep cleanup (#5564)", err)
		return ""
	}
	return exe
})

// sandboxTunnelLifelineUnavailable latches when a lifeline probe fails — the
// binary could not be exec'd, or ran but did not answer the protocol — so
// later provisions go straight to the direct spawn instead of paying a dead
// spawn and an ack-timeout each. The latch lasts the daemon's lifetime: a
// binary that cannot speak the protocol once is not expected to learn it until
// the daemon itself restarts on the installed binary.
var sandboxTunnelLifelineUnavailable atomic.Bool

// sandboxTunnelLifelineUnavailableWarn records a failed probe: latches the
// fallback and warns ONCE PER DAEMON, not once per provision — a user with
// several sandbox sessions must not read the same degradation N times.
func sandboxTunnelLifelineUnavailableWarn(exe string, cause error) {
	if sandboxTunnelLifelineUnavailable.Swap(true) {
		return
	}
	log.WarningLog.Printf("sandbox runtime: %s does not run the watch lifeline (%v); every ssh -L tunnel this daemon "+
		"spawns runs without one — still stopped on a clean daemon exit and reaped by the next daemon's startup sweep, "+
		"but left running by an unclean daemon death until then (#5564). This usually means the af binary at that path was "+
		"replaced while this daemon was running; restarting the daemon (af daemon restart) runs the installed binary and "+
		"re-arms the probe", exe, cause)
}

// tunnelLifeline holds the daemon-side ends of a lifeline-spawned tunnel's
// pipes: keepW is the write end whose close EOFs the lifeline into its group
// kill (held open for the tunnel's life, closed by stopTunnel), and ackR is
// where the lifeline's status lines arrive. nil for a directly-spawned tunnel.
type tunnelLifeline struct {
	keepW *os.File
	ackR  *os.File
}

func (l *tunnelLifeline) close() {
	_ = l.keepW.Close()
	_ = l.ackR.Close()
}

// liveSandboxTunnels registers every provisioner that may currently hold a
// tunnel child, from before its spawn until stopTunnel. It is the daemon's
// only complete view of live tunnels: a tunnel is a daemon-local child that no
// instance row describes, and a mid-create or mid-reprovision tunnel is not
// reachable through m.instances at all.
var liveSandboxTunnels sync.Map // map[*sandboxProvisioner]struct{}

func registerSandboxTunnel(p *sandboxProvisioner)   { liveSandboxTunnels.Store(p, struct{}{}) }
func unregisterSandboxTunnel(p *sandboxProvisioner) { liveSandboxTunnels.Delete(p) }

// StopAllSandboxTunnels stops every live tunnel this process spawned. The
// daemon defers it at construction so EVERY exit path — signal, Shutdown RPC,
// early return during restore — runs it, the same discipline the daemon's own
// supervisors use.
//
// Per-provisioner it skips only one whose reaping flag is set: a reap() holds
// reapMu through its whole remote teardown (bounded by the reap ssh timeout),
// and daemon exit must not park behind that — the in-flight reap runs its own
// stopTunnel as its first act. Any other holder of reapMu is a startTunnel
// assignment section, over in nanoseconds; this blocks through it so a child
// that exists but is not yet published is still stopped, not skipped. For
// tunnels on the lifeline path the group kill here and the lifeline's own
// EOF-driven kill simply converge on the same dead group.
func StopAllSandboxTunnels() {
	liveSandboxTunnels.Range(func(key, _ any) bool {
		p := key.(*sandboxProvisioner)
		if p.reaping.Load() {
			return true
		}
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
		return true
	})
}

// errTunnelOwnerStopped distinguishes "the provisioner was already stopped" —
// the spawn-after-stop race — from an ordinary spawn failure, so the transport
// supervisor can tell a re-dial that must abort from one that should retry.
var errTunnelOwnerStopped = errors.New("the tunnel's owner is already stopping")

// tunnelChild is one generation of the supervised ssh transport: the spawned
// child, its lifeline pipes when it runs under the watch lifeline, and the
// reaper broadcast announcing its single cmd.Wait() result.
//
// The reaper goroutine is the ONLY Waiter because os/exec does not support two
// callers of Cmd.Wait — a second one fails with "waitid: no child processes"
// and, worse, leaves the real exit status unobserved. done is CLOSED rather
// than sent on so every Waiter — the supervisor parked on the exit and a
// killTunnel joining teardown — wakes off one delivery; err is written before
// the close, so any goroutine that observes done sees it (close is the
// happens-before edge). Every kill path joins `done` instead of waiting the
// process itself.
type tunnelChild struct {
	cmd  *exec.Cmd
	ll   *tunnelLifeline
	done chan struct{} // closed by the reaper once cmd.Wait() returns
	err  error         // the Wait result; valid once done is closed
	born time.Time
}

// newTunnelChild wraps an already-started child with its reaper.
func newTunnelChild(cmd *exec.Cmd, ll *tunnelLifeline) *tunnelChild {
	t := &tunnelChild{cmd: cmd, ll: ll, done: make(chan struct{}), born: time.Now()}
	go func() { t.err = cmd.Wait(); close(t.done) }()
	return t
}

// provisionTunnelChild is the spawn+publish half of a tunnel (re)dial, held
// under reapMu so a daemon-exit StopAllSandboxTunnels can never interleave
// between a born child and its publication. The post-spawn stopped check is
// what makes that airtight at the other end: a provisioner whose stop already
// ran — including one whose registry entry is already gone — kills its own
// newborn and fails the provision rather than publish a tunnel no remaining
// stop can reach. The caller owns registration: registerSandboxTunnel before
// this, stopTunnel via reap on every failure after.
//
// The spawn composes p.forward plus p.reverseFwds — W3.4's -R slot, empty when no
// callback was granted — so every re-dial re-establishes the same endpoint contract.
func (p *sandboxProvisioner) provisionTunnelChild() (*tunnelChild, error) {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	cmd, ll, err := p.spawnTunnel(p.forward, p.reverseFwds...)
	if err != nil {
		return nil, err
	}
	child := newTunnelChild(cmd, ll)
	if p.stopped {
		killTunnel(child)
		return nil, errTunnelOwnerStopped
	}
	p.tunnel = child
	return child, nil
}

// killTunnel is the destructive half of stopTunnel: the whole group —
// lifeline+sh+ssh together under the lifeline, sh+ssh directly — so a
// ProxyCommand helper does not outlive the client it was opened for, then the
// lifeline pipe ends, whose keepW close would EOF the lifeline into the same
// group kill had it survived. It is also how the transport supervisor clears a
// generation that already exited on its own: the group kill on a dead pid
// reports ESRCH into the ignored fallback, the join returns instantly off the
// already-closed done, and the lifeline ends are still closed.
//
// The join waits on the reaper's broadcast rather than calling cmd.Wait() —
// the reaper is the only Waiter (see tunnelChild). It is BOUNDED: a process
// that survived group SIGKILL would otherwise park teardown forever, and at
// that point the best af can do is log that the reap never confirmed.
func killTunnel(t *tunnelChild) {
	if t == nil {
		return
	}
	if t.cmd != nil && t.cmd.Process != nil {
		select {
		case <-t.done:
			// The reaper already Wait()ed this generation: its PID is released
			// and may now belong to an unrelated process. Signalling the group
			// number anyway could SIGKILL a stranger, so a reaped child is
			// joined, never signalled.
		default:
			if err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL); err != nil {
				_ = t.cmd.Process.Kill()
			}
		}
		select {
		case <-t.done:
		case <-time.After(sandboxTunnelReapJoinTimeout):
			log.WarningLog.Printf("sandbox runtime: tunnel pid %d still unreaped %s after group SIGKILL", t.cmd.Process.Pid, sandboxTunnelReapJoinTimeout)
		}
	}
	if t.ll != nil {
		t.ll.close()
	}
}

// spawnTunnel starts the long-lived `ssh -L` child, under the watch lifeline
// when this process's own binary can serve as one and directly otherwise.
//
// The inner command text is identical on both paths —
// `<sandbox_ssh> -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1"`
// run by `sh -c` with $0=af-sandbox — because each flag is load-bearing rather
// than hygiene:
//
//   - ExitOnForwardFailure: the local port is reserved by binding :0 and
//     closing it, so another process can win it in the gap. OpenSSH defaults
//     the option to NO (`ssh -G` confirms), which would leave this -N child
//     alive after a FAILED bind — and the readiness probe would then connect
//     to whatever DID win the port and report a healthy tunnel owned by an
//     unrelated process. With it, ssh exits on the failed forward and the
//     probe times out honestly. The same option covers the -R reverse binds
//     W3.4 will add: a refused remote bind is fatal, never silently absent.
//   - ServerAliveInterval/CountMax: the protocol-level keepalive (#5571 W1.2).
//     Without it a dead peer — a host that vanished, a NAT mapping that
//     expired — is indistinguishable from a quiet tunnel until TCP gives up
//     (~2h by default). ssh exits after 3 unanswered 15s probes (~45s bound)
//     and the transport supervisor re-dials. These land AFTER p.sshCmd, so the
//     operator's own -o values keep precedence: OpenSSH takes the FIRST
//     setting it reads.
//
// `reverse` is W3.4's slot for -R forwards (the in-sandbox callback): each
// element is composed as `-R "$N"` after the -L spec, so it gets the same
// ExitOnForwardFailure fatality, the same process-group teardown, and the same
// supervision on every re-dial.
func (p *sandboxProvisioner) spawnTunnel(forward string, reverse ...string) (*exec.Cmd, *tunnelLifeline, error) {
	script := p.sshCmd + ` -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1"`
	args := []string{forward}
	for i := range reverse {
		args = append(args, reverse[i])
		script += fmt.Sprintf(` -R "$%d"`, len(args))
	}
	if exe := sandboxTunnelLifelineExe(); exe != "" && !sandboxTunnelLifelineUnavailable.Load() {
		cmd, ll, direct, err := spawnTunnelViaLifeline(exe, script, args...)
		if err == nil {
			return cmd, ll, nil
		}
		if !direct {
			return nil, nil, err
		}
		sandboxTunnelLifelineUnavailableWarn(exe, err)
	}
	// Its own group so stopTunnel can kill the ssh client and any ProxyCommand
	// helper together, not just the `sh` that launched them.
	cmd := exec.Command("sh", append([]string{"-c", script, "af-sandbox"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// No stdin payload here, so WaitDelay is correct and wanted: a tunnel that
	// ignores its kill must not hold af's pipes open forever. This is the child
	// TRAP 1 says it belongs on.
	cmd.WaitDelay = sandboxTunnelWaitDelay
	stampSandboxTunnel(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, nil, nil
}

// spawnTunnelViaLifeline starts the tunnel under af's watch lifeline: the
// lifeline leads the tunnel's process group, gets the lifeline pipe's read end
// on fd 3 and the ack pipe's write end on fd 4, and kills its whole group when
// the daemon's write end closes — by daemon exit of any kind.
//
// direct reports whether the caller may spawn the tunnel itself: true when the
// binary could not be exec'd or did not answer the protocol (err carries the
// reason for the once-per-daemon warning), false when the lifeline spoke and
// reported the SHELL could not start — a direct spawn hits the same wall, so
// that error must fail the provision rather than be retried around.
func spawnTunnelViaLifeline(exe, script string, args ...string) (cmd *exec.Cmd, ll *tunnelLifeline, direct bool, err error) {
	lifelineR, keepW, err := os.Pipe()
	if err != nil {
		return nil, nil, true, fmt.Errorf("creating the lifeline pipe: %w", err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = lifelineR.Close()
		_ = keepW.Close()
		return nil, nil, true, fmt.Errorf("creating the lifeline ack pipe: %w", err)
	}

	cmd = exec.Command(exe, watchlifeline.Argv(sandboxTunnelLifelineGrace, "sh", append([]string{"-c", script, "af-sandbox"}, args...)...)...)
	// The lifeline must lead the group it kills; it refuses to run otherwise.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = sandboxTunnelWaitDelay
	stampSandboxTunnel(cmd)
	// watchlifeline.LifelineFD and AckFD in the child. The write end of the
	// lifeline pipe stays here and only here: os.Pipe sets O_CLOEXEC on both
	// ends, and only ExtraFiles reach a child, so nothing else this process
	// starts can inherit it and keep the lifeline open past daemon death.
	cmd.ExtraFiles = []*os.File{lifelineR, ackW}
	startErr := cmd.Start()
	_ = lifelineR.Close()
	_ = ackW.Close()
	if startErr != nil {
		_ = keepW.Close()
		_ = ackR.Close()
		return nil, nil, true, fmt.Errorf("cannot exec it: %w", startErr)
	}

	_ = ackR.SetReadDeadline(time.Now().Add(sandboxTunnelLifelineAckTimeout))
	line, readErr := bufio.NewReader(ackR).ReadString('\n')
	supported, started, shellErr := watchlifeline.ParseAck(line)
	if supported && started {
		return cmd, &tunnelLifeline{keepW: keepW, ackR: ackR}, false, nil
	}
	// Not a usable lifeline. The group's leader is this process's own unreaped
	// child, so the group SIGKILL cannot reach anyone else's processes; it goes
	// first because a binary that neither acked nor exited would otherwise be
	// waited on forever.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	_ = keepW.Close()
	_ = ackR.Close()
	if supported {
		// The lifeline spoke and could not start the shell — a direct spawn of
		// the same sh fails identically, so this is the provision's error.
		return nil, nil, false, fmt.Errorf("start sh: %v", shellErr)
	}
	if readErr != nil {
		return nil, nil, true, fmt.Errorf("no ack: %w", readErr)
	}
	return nil, nil, true, fmt.Errorf("no ack: it wrote %q", strings.TrimSpace(line))
}

// sandboxTunnelArgv reports whether an argv is af's sandbox transport shape,
// in either of the two forms a transport can present:
//
//   - the af-sandbox supervisor marker as an element — the $0 af gives the
//     `sh -c` that runs both the tunnel and the bounded provision/reap
//     commands, visible whenever that sh is still alive;
//   - the ssh argv those `sh -c` commands become on shells that tail-optimize
//     the last command: `ssh … -o ExitOnForwardFailure=yes -N -L <forward>` is
//     what a leaked tunnel looks like once its sh is gone. darwin's /bin/sh
//     ALWAYS exec-optimizes, so on a Mac every pre-#5564 leftover has this
//     shape and no af-sandbox element — matching only the marker would render
//     the report silently absent for exactly the population it exists for.
//
// Element equality, never a substring: this match may inform a report but it
// never arms a kill, and a user's unrelated "my-af-sandbox" must not even be
// named one.
func sandboxTunnelArgv(argv []string) bool {
	if slices.Contains(argv, "af-sandbox") {
		return true
	}
	if len(argv) == 0 || filepath.Base(argv[0]) != "ssh" {
		return false
	}
	hasN, hasL, hasForwardGuard := false, false, false
	for i, a := range argv {
		switch a {
		case "-N":
			hasN = true
		case "-L":
			hasL = true
		case "-o":
			if i+1 < len(argv) && argv[i+1] == "ExitOnForwardFailure=yes" {
				hasForwardGuard = true
			}
		}
	}
	return hasN && hasL && hasForwardGuard
}

// SweepOrphanSandboxTunnels reaps ssh -L transport children a PREVIOUS daemon
// generation for this AF home left running (#5564).
//
// It runs once, at daemon startup, BEFORE the manager readiness barrier opens:
// restore only loads inert backends and reprovision is a poll-loop activity,
// so no provision of THIS daemon can have started a tunnel yet — every process
// provably stamped with this home is a leftover by position, not inference.
//
// PROOF, never a guess. A process is killed only when ALL hold:
//
//   - owned by THIS uid: af spawns its tunnels as the daemon's user, and a
//     shared box never has one user's sweep touch another's processes;
//   - its INITIAL environment carries AF_SANDBOX_TUNNEL_HOME=<this home>,
//     canonicalized identically to the stamp — a different AF home's tunnel
//     is never touched, and EnvUnknown is skipped rather than read as absent;
//   - the (pid, StartID) instance still matches at signal time through
//     proctree.Signal, so a pid recycled between scan and kill is never hit.
//
// Tunnels leaked by daemons older than the marker carry no stamp and cannot be
// proven, so they are REPORTED by argv shape — the af-sandbox supervisor
// element, or the bare `ssh -o ExitOnForwardFailure=yes -N -L` argv a leftover
// exec's into on shells that tail-optimize — and left for the operator: the
// warning names the pids and how to inspect and kill them by hand — and never
// prints anything from their environment. Command line is allowed to inform a
// report, never to arm a kill.
func SweepOrphanSandboxTunnels(homeID string) {
	procs, err := proctree.Snapshot()
	if err != nil {
		// A process table that cannot be read is not an empty one (#1939's
		// rule): say so and skip rather than report a clean sweep.
		log.WarningLog.Printf("orphan tunnel sweep: cannot read the process table; skipping: %v", err)
		return
	}
	self, uid := os.Getpid(), os.Getuid()
	var marked, unmarked []proctree.Process
	unreadable := 0
	for _, p := range procs {
		if p.PID <= 1 || p.PID == self {
			continue
		}
		owner, ok := proctree.UID(p.PID)
		if !ok || owner != uid {
			continue
		}
		home, status := proctree.LookupEnv(p.PID, sandboxTunnelHomeEnv)
		switch {
		case status == proctree.EnvFound && home == homeID:
			marked = append(marked, p)
		case status == proctree.EnvFound:
			// Provably a DIFFERENT AF home's tunnel: untouched and unreported —
			// it is very probably live under its own daemon.
		case status == proctree.EnvAbsent && sandboxTunnelArgv(proctree.Argv(p.PID)):
			unmarked = append(unmarked, p)
		case status == proctree.EnvUnknown && sandboxTunnelArgv(proctree.Argv(p.PID)):
			unreadable++
		}
	}

	killed := 0
	for _, p := range marked {
		if err := proctree.Signal(p, syscall.SIGKILL); err != nil && !errors.Is(err, proctree.ErrIdentityChanged) {
			log.WarningLog.Printf("orphan tunnel sweep: cannot kill leftover af sandbox transport %d (%s): %v", p.PID, p.Comm, err)
			continue
		}
		killed++
	}
	if killed > 0 {
		log.InfoLog.Printf("orphan tunnel sweep: killed %d ssh -L tunnel process(es) a previous af daemon for this AF home left running", killed)
	}
	if len(unmarked) > 0 || unreadable > 0 {
		var pids []string
		for _, p := range unmarked {
			pids = append(pids, fmt.Sprint(p.PID))
		}
		list := strings.Join(pids, ",")
		extra := ""
		if unreadable > 0 {
			extra = fmt.Sprintf("; %d further candidate(s) have an unreadable environment and were skipped", unreadable)
		}
		log.WarningLog.Printf("orphan tunnel sweep: %d process(es) look like af sandbox transports (argv matches af's transport "+
			"shape) but have no %s ownership stamp, so af cannot prove they are this home's leftovers and will "+
			"not kill them — they may be tunnels an older af daemon leaked, or live processes of a different AF home. "+
			"Inspect them with `ps -o pid,ppid,command -p %s` and, if they are stale af ssh tunnels holding forwarded ports, "+
			"stop them by hand with `kill %s`%s",
			len(unmarked), sandboxTunnelHomeEnv, list, strings.Join(pids, " "), extra)
	}
}

// --- the supervised connection (#5571 W1.2) ---
//
// superviseTunnel keeps one session's ssh transport established until teardown.
// Without it a dead `ssh -N -L` child is never even reaped (nobody called Wait
// before #5565's teardown paths), so a host blip or a killed client left the
// forward down forever — LG-03/topo:T5.
//
// The loop is one goroutine per provisioner, started by startTunnel after the
// first generation proves ready. Each iteration either waits on the published
// generation's reaper channel, or re-dials through provisionTunnelChild — the
// same spawn+publish path as the first provision, so the stopped-check kills a
// re-dial that raced teardown and the -L/-R spec is identical every time.
//
// Logging follows the maintainer's rule: transitions, not ticks. `down` opens
// an episode at the first observed death; every tunnelSuperviseLogEvery-th
// failed event during it gets one line; a generation that survives
// redialHealthySpan closes it (via the timer the monitor arms per generation,
// so a repair that holds gets its "restored" line even if nothing ever dies
// again). A flapping link — dial ok, dead before the span — stays inside the
// episode and is covered by the every-Nth counter rather than a line per flap.

// sandboxTunnelReapJoinTimeout bounds how long a kill path waits for the
// reaper to collect a group-SIGKILL'd child. The kernel reaps in milliseconds;
// the bound exists only so a wedged reap cannot park teardown forever.
const sandboxTunnelReapJoinTimeout = 5 * time.Second

// tunnelSuperviseLogEvery is the maintainer-asked "every Nth attempt" knob for
// the supervisor's still-down line: events are generation deaths plus failed
// re-dials, so at the ~30s backoff ceiling this is roughly one line per ten
// minutes of sustained outage rather than one per tick.
const tunnelSuperviseLogEvery = 20

// tunnelRedialJitter smears the shared redialDelay ladder ±25% so a fleet of
// sandboxes that lost the same host does not re-dial in lockstep.
const tunnelRedialJitter = 0.25

// tunnelRedialRand is the jitter source — a var so a test can pin it.
var tunnelRedialRand = rand.Float64

// tunnelRedialDelay is the nth re-dial's wait: redialDelay's doubling ladder
// (shared with the PTY recovery path, and shrunken by the same test seam)
// smeared by the jitter factor. The smear can land slightly above the nominal
// cap — that is the point of jitter — while staying order-of-cap bounded.
func tunnelRedialDelay(attempt int) time.Duration {
	d := redialDelay(attempt)
	jitter := 1 + (tunnelRedialRand()*2-1)*tunnelRedialJitter
	return max(time.Duration(float64(d)*jitter), time.Millisecond)
}

// stoppedChanLocked returns the channel stopTunnel closes exactly once. It is
// built lazily — callers hold reapMu — because reap-only provisioners (the
// persisted-handle reload path, most test literals) construct the type without
// one and never spawn a tunnel.
func (p *sandboxProvisioner) stoppedChanLocked() chan struct{} {
	if p.stoppedCh == nil {
		p.stoppedCh = make(chan struct{})
	}
	return p.stoppedCh
}

// tunnelMonitor is the supervisor goroutine's episode state. The healthy-timer
// callback and the loop itself both touch it, so its fields sit behind mu; the
// loop reads the published generation from the provisioner under reapMu — the
// two locks never nest (the timer takes only mu and checks the generation it
// captured at arm time).
type tunnelMonitor struct {
	p *sandboxProvisioner

	mu        sync.Mutex
	current   *tunnelChild // the generation the loop is parked on; nil while re-dialing
	attempt   int          // backoff ladder position; reset when a generation crosses redialHealthySpan
	down      bool         // an outage episode is open
	downSince time.Time
	events    int // deaths + failed re-dials inside this episode
}

// armHealthyTimer starts the generation's healthy-span clock: if the child is
// still the published generation when it fires, the connection held — the
// backoff ladder resets and an open outage episode closes with one "restored"
// line. A generation that dies first fails the identity check and leaves the
// episode running.
func (m *tunnelMonitor) armHealthyTimer(child *tunnelChild) {
	time.AfterFunc(redialHealthySpan, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.current != child {
			return
		}
		m.attempt = 0
		if m.down {
			m.down = false
			log.InfoLog.Printf("sandbox runtime: session %q: ssh transport re-established after %s down (%d events)",
				m.p.spec.Title, time.Since(m.downSince).Round(time.Second), m.events)
		}
	})
}

// noteEvent counts a death or a failed re-dial and emits the every-Nth
// still-down line while an episode is open.
func (m *tunnelMonitor) noteEvent(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.down {
		return
	}
	m.events++
	if m.events%tunnelSuperviseLogEvery == 0 {
		log.WarningLog.Printf("sandbox runtime: session %q: ssh transport still unreachable after %d events over %s (last: %v)",
			m.p.spec.Title, m.events, time.Since(m.downSince).Round(time.Second), err)
	}
}

// superviseTunnel is the per-session transport supervisor (#5571 W1.2). It runs
// until the provisioner's stopped latch closes stoppedCh — teardown always
// wins — and re-dials any generation that exits for any other reason.
func (p *sandboxProvisioner) superviseTunnel() {
	p.reapMu.Lock()
	stoppedCh := p.stoppedChanLocked()
	p.reapMu.Unlock()

	m := &tunnelMonitor{p: p}
	// A healthy-timer armed for the last generation must not fire into an
	// exited monitor — it would read the still-published current as alive and
	// log a "restored" that teardown already made false.
	defer func() {
		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
	}()
	for {
		p.reapMu.Lock()
		child := p.tunnel
		stopped := p.stopped
		p.reapMu.Unlock()
		if stopped {
			return
		}

		if child == nil {
			// No live generation: back off, then re-dial through the same
			// spawn+publish path as the first provision. Re-dial failure is
			// just another event in the episode; the loop comes back around.
			m.mu.Lock()
			delay := tunnelRedialDelay(m.attempt)
			m.attempt++
			m.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-stoppedCh:
				timer.Stop()
				return
			}
			p.clearStaleCallbackSocket()
			next, err := p.provisionTunnelChild()
			switch {
			case errors.Is(err, errTunnelOwnerStopped):
				return
			case err != nil:
				m.noteEvent(err)
				continue
			}
			// Probe readiness outside reapMu — same contract as the first
			// provision. A re-dial that never answers is an event, and its
			// stillborn generation is killed before the next round.
			if err := waitForSandboxTunnelFn(p.label(), p.localAddr, sandboxTunnelReadyTimeout, sandboxTunnelReadyPoll); err != nil {
				p.reapMu.Lock()
				if p.tunnel == next {
					killTunnel(next)
					p.tunnel = nil
				}
				p.reapMu.Unlock()
				m.noteEvent(err)
				continue
			}
			m.armHealthyTimer(next)
			continue
		}

		m.mu.Lock()
		m.current = child
		m.mu.Unlock()
		select {
		case <-stoppedCh:
			return
		case <-child.done:
			err := child.err
			// The generation is gone whatever the cause; clear it so a racing
			// stopTunnel does not double-kill and so the loop's next iteration
			// re-dials. killTunnel's join returns instantly — done is already
			// closed — and closes this generation's lifeline ends.
			p.reapMu.Lock()
			if p.tunnel == child {
				killTunnel(child)
				p.tunnel = nil
			}
			stopped := p.stopped
			p.reapMu.Unlock()
			if stopped {
				return
			}
			m.mu.Lock()
			m.current = nil
			if !m.down {
				m.down = true
				m.downSince = time.Now()
				m.events = 0
				log.WarningLog.Printf("sandbox runtime: session %q: ssh transport went down after %s up: %v; re-dialing with capped backoff",
					p.spec.Title, time.Since(child.born).Round(time.Second), err)
			}
			m.mu.Unlock()
			m.noteEvent(err)
		}
	}
}

// clearStaleCallbackSocket removes the callback carrier left on the sandbox by a
// connection that died without sshd unlinking it, so the re-dial binds the SAME
// path (the in-sandbox client's URL never changes). Best-effort: if the host
// cannot be reached the re-dial fails too and the loop comes back around. The
// path is the session's own, inside its 0700 directory.
func (p *sandboxProvisioner) clearStaleCallbackSocket() {
	p.reapMu.Lock()
	sock := p.callbackSocket
	p.reapMu.Unlock()
	if sock == "" {
		return
	}
	_, _ = p.Run(sshShortStepTimeout, "rm -f -- "+shellQuote(sock), nil, true)
}
