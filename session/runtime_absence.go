package session

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
)

// ProveRuntimeGone reports whether an ssh/sandbox row's runtime is PROVABLY gone
// from its host (#5571 W1.6). It is the only thing that may turn "af has no
// handle to this runtime" into "there is nothing left to preserve": a missing
// handle is never evidence, because a row whose host was unreachable when the
// daemon started has no handle either, while its workspace may hold hours of
// unpushed work.
//
// The proof is one bounded probe of the host the row's PERSISTED identity names
// (ssh command, session dir, token ref — nothing is matched by name or pattern):
// the host answered, the session dir is absent or carries a different identity,
// and no process carries this ref. Everything else — no persisted identity, no
// answer, no /proc to enumerate, the dir still ours — is unproven, with the
// reason, and callers keep the work.
func (i *Instance) ProveRuntimeGone(timeout time.Duration) (bool, string) {
	rows := SweepRowsOf([]*Instance{i})
	if len(rows) == 0 {
		return false, "the row persisted no runtime identity, so its absence cannot be checked"
	}
	dir, ref := rows[0].Identity()
	if dir == "" || ref == "" || !path.IsAbs(dir) {
		return false, "the row's runtime identity is incomplete (no session dir or token ref), so its absence cannot be checked"
	}
	prov := cleanupProvisioner(rows[0])
	if prov == nil || prov.sshCmd == "" {
		return false, "the row's runtime is not an ssh/sandbox host runtime af can probe"
	}
	return proveRuntimeGone(prov, dir, ref, timeout)
}

// proveRuntimeGone runs the absence probe through sh.
func proveRuntimeGone(sh sandboxShell, dir, ref string, timeout time.Duration) (bool, string) {
	script := `d=` + shellQuote(dir) + `; ref=` + shellQuote(ref) + `; echo HOST-ANSWERED; ` +
		`if [ -e "$d" ]; then ` +
		`r=$(sed -n 's/^` + runtimetoken.RefEnvName + `=//p' "$d/` + sshIdentityEnvName + `" 2>/dev/null | sed -n '1p' | tr -d "'\r"); ` +
		`if [ "$r" = "$ref" ]; then echo DIR-OURS; else echo DIR-OTHER; fi; else echo DIR-GONE; fi; ` +
		`[ -r /proc/self/cmdline ] || { echo NOPROC; echo PROBE-DONE; exit 0; }; ` +
		`for p in /proc/[0-9]*; do pid=${p#/proc/}; ` +
		`r=$(tr '\000' '\n' < "$p/environ" 2>/dev/null | sed -n 's/^` + runtimetoken.RefEnvName + `=//p' | sed -n '1p'); ` +
		`[ "$r" = "$ref" ] && echo "PROC $pid"; done; echo PROBE-DONE`
	out, err := sh.Run(timeout, script, nil, false)
	if err != nil {
		return false, "its host did not answer the identity probe: " + err.Error()
	}
	var answered, done, dirGone, dirOther bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "HOST-ANSWERED":
			answered = true
		case line == "PROBE-DONE":
			done = true
		case line == "DIR-GONE":
			dirGone = true
		case line == "DIR-OTHER":
			dirOther = true
		case line == "DIR-OURS":
			return false, "its session dir is still on the host with this session's identity"
		case line == "NOPROC":
			return false, "its host has no /proc, so its processes cannot be enumerated"
		case strings.HasPrefix(line, "PROC "):
			return false, fmt.Sprintf("a process on its host still carries this session's identity (pid %s)", strings.TrimPrefix(line, "PROC "))
		}
	}
	if !answered || !done {
		return false, "its host's identity probe did not complete"
	}
	if !dirGone && !dirOther {
		return false, "its host's identity probe did not report the session dir"
	}
	return true, "the host answered: the session dir is gone or no longer this session's, and no process carries its identity"
}
