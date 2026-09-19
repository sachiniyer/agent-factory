package tmux

// DIAGNOSTIC ONLY — #4678. Never merge. Records every firing of the
// socket-absent SIGUSR1 probe (tmuxSocketClaimed) and every process it signals,
// so one CI run can show whether the probe fires during the suite and whether
// its targets include tmux CLIENTS. Behaviour is unchanged apart from one argv
// read and one process lookup per candidate pid before the kill.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

const diag4678DefaultPath = "/tmp/af-4678-sigusr1.log"

type diag4678Target struct {
	pid      int
	comm     string
	age      time.Duration
	lookup   string
	argv     []string
	isServer bool
	killErr  error
	skipped  bool
}

func diag4678Snapshot(pid int) diag4678Target {
	d := diag4678Target{pid: pid}
	if p, err := proctree.Lookup(pid); err == nil {
		d.comm = p.Comm
		d.age = time.Since(p.StartedAt)
	} else {
		d.lookup = err.Error()
	}
	d.argv = proctree.Argv(pid)
	return d
}

func diag4678Record(socketPath string, pids []int, targets []diag4678Target, signaled bool) {
	path := os.Getenv("AF_DIAG_4678_LOG")
	if path == "" {
		path = diag4678DefaultPath
	}
	var b strings.Builder
	fmt.Fprintf(&b, "probe t=%s sender_pid=%d sender=%s socket=%s candidates=%d signaled=%v\n",
		time.Now().UTC().Format(time.RFC3339Nano), os.Getpid(), filepath.Base(os.Args[0]),
		socketPath, len(pids), signaled)
	fmt.Fprintf(&b, "  caller %s\n", diag4678Caller())
	for _, d := range targets {
		verb := "kill"
		if d.skipped {
			verb = "skip"
		}
		errText := "<nil>"
		if d.killErr != nil {
			errText = d.killErr.Error()
		}
		fmt.Fprintf(&b, "  %s pid=%d comm=%q age=%s lookup_err=%q is_server=%v kill_err=%q argv=%q\n",
			verb, d.pid, d.comm, d.age.Round(time.Microsecond), d.lookup, d.isServer, errText, d.argv)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(b.String())
	_ = f.Close()
}

func diag4678Caller() string {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var names []string
	for {
		fr, more := frames.Next()
		name := fr.Function
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		names = append(names, name)
		if !more || len(names) == 12 {
			break
		}
	}
	return strings.Join(names, " < ")
}
