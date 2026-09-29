package proctree

import (
	"path/filepath"
	"strings"
)

// isTmuxArgv reports whether argv names the tmux binary itself. The basename is
// matched exactly: a prefix would also take tmuxinator, tmuxp and
// tmux-mem-cpu-load.
func isTmuxArgv(argv []string) bool {
	return len(argv) > 0 && filepath.Base(argv[0]) == "tmux"
}

// IsTmuxProcess reports whether pid is running tmux at all, as a server or as a
// client, from its own command line. An unreadable command line reports false.
//
// This is the predicate for callers that must LEAVE tmux alone: the worktree
// occupancy gate and the worktree-writer reaper. Neither may signal or count any
// tmux process. The server is shared infrastructure, and a client is not a
// writer; af's own short-lived clients also inherit the daemon's cwd, which can
// sit inside a worktree. Never use it to choose a process to signal — see
// IsTmuxServer.
//
// It also accepts a retitled argv[0], `tmux: server (<socket>)` or
// `tmux: client (<socket>)`: the form setproctitle(3) writes into the argument
// area on the BSDs. tmux does not do this on Linux (it retitles the kernel task
// name via prctl; measured: every live server's argv[0] is plain `tmux`) or on
// darwin (it cannot retitle at all). So on those platforms the clause only ever
// matches a fixture shaped that way, such as the reaper's regression test.
// Keeping it costs nothing on the leave-alone side. It is matched on the whole
// argv[0]: filepath.Base would cut a real title at its socket path's slash.
func IsTmuxProcess(pid int) bool {
	argv := Argv(pid)
	return isTmuxArgv(argv) || (len(argv) > 0 && strings.HasPrefix(argv[0], "tmux: "))
}

// IsTmuxServer reports whether pid is a tmux SERVER, identified positively.
// Anything it cannot identify, including a process whose command line or
// process-table entry is unreadable, reports false. That is also the safe
// direction for its signalling caller: false means "do not signal".
//
// The command line cannot answer this on its own. A server is normally forked
// from the client that started it and never execs, so its argv is that client's
// argv — `tmux new-session -d -s x` — byte for byte (measured on Linux and
// darwin, #4678). Reading argv alone therefore admitted every client, which is
// how the socket-absent probe came to send a terminating SIGUSR1 to tmux
// clients.
//
// So identity comes from what the SERVER code path does to the process:
//
//   - On Linux, proc_start("server") retitles the task to `tmux: server` via
//     prctl(PR_SET_NAME). Clients become `tmux: client`. Neither the foreground
//     `-D` server nor the daemonised one is missed.
//   - Where tmux cannot retitle (darwin: no prctl, and the kernel task name
//     stays the exec'd image name), a server is recognised by the daemon(3)
//     shape server_start leaves behind. The server called setsid, so it leads
//     its own session; its forking parent exited, so it was reparented to
//     pid 1. A client that af launches is neither, or has a live parent.
//
// Neither rule reads a subcommand, so a tmux subcommand added later cannot turn
// a client into a "server": whatever the subcommand, a client runs through
// client_main, which titles it a client and never daemonises it.
//
// Both rules are also safe to act on. tmux blocks every signal from before its
// fork until the server's own handler is installed, and on Linux the
// retitling happens inside that window. So a SIGUSR1 sent to a process matched
// here is handled (tmux recreates its socket), never fatal.
//
// Not identified, deliberately: a foreground `tmux -D` server where tmux cannot
// retitle. It starts as a client, with SIGUSR1 still at its default disposition
// (terminate), and nothing readable separates its first milliseconds from the
// rest. It also never daemonises, so the daemon(3) shape never appears to name
// it. It reports false, which leaves it unsignalled.
func IsTmuxServer(pid int) bool {
	argv := Argv(pid)
	if !isTmuxArgv(argv) {
		return false
	}
	p, err := Lookup(pid)
	if err != nil {
		return false
	}
	return tmuxServerIdentity(p)
}

// tmuxServerIdentity is IsTmuxServer's decision over a process-table entry whose
// argv already names tmux. It is separate so that every shape can be pinned
// without a live tmux.
func tmuxServerIdentity(p Process) bool {
	switch p.Comm {
	case "tmux: server":
		return true
	case "tmux":
		// Un-retitled. Only the daemon(3) shape identifies a server here. On a
		// platform that does retitle, this is a process that has not reached
		// proc_start yet, and a server in that state has every signal blocked.
		// It is refused alongside a client: nothing positive names it yet.
		return p.PID > 0 && p.SID == p.PID && p.PPID == 1
	default:
		// `tmux: client`, or a title this code does not know. A known title
		// that is not the server's is positively not a server.
		return false
	}
}
