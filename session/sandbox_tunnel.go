package session

import (
	"bufio"
	"errors"
	"fmt"
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

// provisionTunnelChild is the spawn+publish half of startTunnel, held under
// reapMu so a daemon-exit StopAllSandboxTunnels can never interleave between a
// born child and its publication. The post-spawn stopped check is what makes
// that airtight at the other end: a provisioner whose stop already ran —
// including one whose registry entry is already gone — kills its own newborn
// and fails the provision rather than publish a tunnel no remaining stop can
// reach. The caller owns registration: registerSandboxTunnel before this,
// stopTunnel via reap on every failure after.
func (p *sandboxProvisioner) provisionTunnelChild(forward string) (*exec.Cmd, *tunnelLifeline, error) {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	cmd, ll, err := p.spawnTunnel(forward)
	if err != nil {
		return nil, nil, err
	}
	if p.stopped {
		killTunnel(cmd, ll)
		return nil, nil, errors.New("the tunnel's owner is already stopping")
	}
	p.tunnel = cmd
	p.lifeline = ll
	return cmd, ll, nil
}

// killTunnel is the destructive half of stopTunnel: the whole group —
// lifeline+sh+ssh together under the lifeline, sh+ssh directly — so a
// ProxyCommand helper does not outlive the client it was opened for, then the
// lifeline pipe ends, whose keepW close would EOF the lifeline into the same
// group kill had it survived. startTunnel uses it for a child born after its
// provisioner's stop already ran.
func killTunnel(cmd *exec.Cmd, ll *tunnelLifeline) {
	if cmd != nil && cmd.Process != nil {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	if ll != nil {
		ll.close()
	}
}

// spawnTunnel starts the long-lived `ssh -L` child, under the watch lifeline
// when this process's own binary can serve as one and directly otherwise.
//
// The inner command text is identical on both paths —
// `<sandbox_ssh> -o ExitOnForwardFailure=yes -N -L "$1"` run by `sh -c` with
// $0=af-sandbox — because the flag is load-bearing rather than hygiene: the
// local port is reserved by binding :0 and closing it, so another process can
// win it in the gap. OpenSSH defaults ExitOnForwardFailure to NO (`ssh -G`
// confirms), which would leave this -N child alive after a FAILED bind — and
// the readiness probe would then connect to whatever DID win the port and
// report a healthy tunnel owned by an unrelated process. With it, ssh exits on
// the failed forward and the probe times out honestly.
func (p *sandboxProvisioner) spawnTunnel(forward string) (*exec.Cmd, *tunnelLifeline, error) {
	script := p.sshCmd + ` -o ExitOnForwardFailure=yes -N -L "$1"`
	if exe := sandboxTunnelLifelineExe(); exe != "" && !sandboxTunnelLifelineUnavailable.Load() {
		cmd, ll, direct, err := spawnTunnelViaLifeline(exe, script, forward)
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
	cmd := exec.Command("sh", "-c", script, "af-sandbox", forward)
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
func spawnTunnelViaLifeline(exe, script, forward string) (cmd *exec.Cmd, ll *tunnelLifeline, direct bool, err error) {
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

	cmd = exec.Command(exe, watchlifeline.Argv(sandboxTunnelLifelineGrace, "sh", "-c", script, "af-sandbox", forward)...)
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
