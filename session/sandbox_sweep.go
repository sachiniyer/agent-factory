package session

import (
	"crypto/subtle"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/log"
)

// The sandbox host sweep: reap what af created on a host and no persisted row
// owns, by PERSISTED IDENTITY (#5571 W1.7).
//
// A daemon that dies between provisioning a workspace and persisting its row, a
// row lost to a failed write, or a retired tombstone can leave a session dir and
// a running agent-server on the host that nothing on the daemon side names. The
// sweep finds them — and only them — under one hard constraint, af's
// confused-deputy stance: it never kills or removes what it cannot PROVE it
// owns, and never by name or pattern.
//
// What a candidate is, and why that is proof:
//   - a direct child of a directory root where a persisted row's own session dir
//     lives (the roots come from rows, never from a guessed path);
//   - holding the two W1.1 identity files, the token ref and the derived bearer
//     token;
//   - whose token EQUALS the one this AF home's runtime-identity key derives from
//     that ref. The key never leaves this machine, so a dir written by another af
//     home, another user, or a lookalike fails the comparison and is left alone.
//
// Then, in order, a candidate is skipped when a persisted row owns it (by dir or
// by ref) or it is young (an in-flight provision), REPORTED — dir, process and
// all left in place — when its workspace may hold work that exists nowhere else
// (uncommitted or untracked files, commits on no remote, stashes, or git unable
// to answer), and reaped only when it is provably clean. The reap is the ordinary
// identity-checked one: the process must match its own dir's argv[0] AND its
// AF_TOKEN_REF, so a same-named process elsewhere survives.

// SweepAction is what the sweep did with a candidate.
type SweepAction string

const (
	SweepReaped   SweepAction = "reaped"
	SweepReported SweepAction = "reported"
)

// SweepFinding is one candidate the sweep acted on or flagged. Owned, young,
// foreign and unproven dirs are not findings: they were never af's to touch.
type SweepFinding struct {
	Host   string
	Dir    string
	Ref    string
	Action SweepAction
	Reason string
}

// SweepRow is the persisted identity of one session's remote runtime.
type SweepRow struct {
	Title       string
	BackendType string
	Cleanup     *RuntimeCleanupData
}

// SweepRowsOf projects instances to their persisted runtime identity. Every row
// is included — archived and tombstoned too — because each still owns its dir.
func SweepRowsOf(instances []*Instance) []SweepRow {
	rows := make([]SweepRow, 0, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		d := inst.ToInstanceData()
		// ToInstanceData stages the backend's identity privately; the public
		// RuntimeCleanup is only published by ForStorage. Read the staged one
		// first so a live instance is never projected as owning nothing — that
		// would hide its dir from the ownership checks and let it look orphaned.
		cleanup := d.runtimeCleanup
		if cleanup == nil {
			cleanup = d.RuntimeCleanup
		}
		if cleanup == nil {
			continue
		}
		rows = append(rows, SweepRow{Title: d.Title, BackendType: d.BackendType, Cleanup: cleanup})
	}
	return rows
}

// Identity returns the session dir and token ref this row's runtime persisted.
func (r SweepRow) Identity() (dir, ref string) {
	if r.Cleanup == nil {
		return "", ""
	}
	switch {
	case r.Cleanup.Sandbox != nil:
		return r.Cleanup.Sandbox.SessionDir, r.Cleanup.Sandbox.TokenRef
	case r.Cleanup.SSH != nil:
		return r.Cleanup.SSH.SessionDir, r.Cleanup.SSH.TokenRef
	}
	return "", ""
}

// sweepHostState is everything the persisted rows say about one host.
type sweepHostState struct {
	label string
	shell sandboxShell
	// reaper builds the provisioner whose reap removes one orphan.
	reaper func(dir, pid, ref string) *sandboxProvisioner
	roots  map[string]bool
	dirs   map[string]bool
	refs   map[string]bool
}

// SweepSandboxOrphans sweeps every host the persisted rows name. stillOwned is
// consulted again immediately before each reap, so a row that appeared while the
// sweep ran (a restore, a create that just persisted) is never reaped. With no
// runtime-identity key there is no proof to make and nothing is touched.
func SweepSandboxOrphans(rows []SweepRow, minAge time.Duration, stillOwned func(dir, ref string) bool) []SweepFinding {
	key, err := runtimetoken.LoadKey()
	if err != nil || len(key) == 0 {
		return nil
	}
	var findings []SweepFinding
	for _, h := range sweepHostsOf(rows) {
		findings = append(findings, sweepHost(h, key, minAge, stillOwned)...)
	}
	return findings
}

func sweepHostsOf(rows []SweepRow) []*sweepHostState {
	hosts := map[string]*sweepHostState{}
	for _, row := range rows {
		// No I/O: rebuilds only the cleanup-capable backend from the stored handle.
		backend, _, err := restoreRuntimeCleanup(row.Title, row.BackendType, row.Cleanup)
		if err != nil {
			continue
		}
		var prov *sandboxProvisioner
		switch b := backend.(type) {
		case *sandboxBackend:
			prov = b.provisioner
		case *sshBackend:
			prov = b.provisioner
		}
		if prov == nil || prov.sessionDir == "" || !path.IsAbs(prov.sessionDir) {
			continue
		}
		h := hosts[prov.sshCmd]
		if h == nil {
			cmd, label := prov.sshCmd, prov.backendLabel
			h = &sweepHostState{
				label: prov.label(), shell: prov, roots: map[string]bool{}, dirs: map[string]bool{}, refs: map[string]bool{},
			}
			h.reaper = func(dir, pid, ref string) *sandboxProvisioner {
				return &sandboxProvisioner{sshCmd: cmd, backendLabel: label, sessionDir: dir, remotePID: pid, tokenRef: ref}
			}
			hosts[prov.sshCmd] = h
		}
		h.roots[path.Dir(prov.sessionDir)] = true
		h.dirs[prov.sessionDir] = true
		if prov.tokenRef != "" {
			h.refs[prov.tokenRef] = true
		}
	}
	out := make([]*sweepHostState, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h)
	}
	return out
}

type sweepCandidate struct {
	dir, ref, token string
	old             bool
}

func sweepHost(h *sweepHostState, key []byte, minAge time.Duration, stillOwned func(dir, ref string) bool) []SweepFinding {
	var findings []SweepFinding
	for root := range h.roots {
		cands, err := sweepList(h.shell, root, minAge)
		if err != nil {
			log.WarningLog.Printf("sandbox orphan sweep: cannot list %s on %s: %v", root, h.label, err)
			continue
		}
		for _, c := range cands {
			if f, ok := sweepOne(h, key, c, stillOwned); ok {
				findings = append(findings, f)
			}
		}
	}
	return findings
}

// sweepList lists the identity-bearing direct children of root.
func sweepList(sh sandboxShell, root string, minAge time.Duration) ([]sweepCandidate, error) {
	mins := int(minAge / time.Minute)
	if mins < 0 {
		mins = 0
	}
	script := `cd ` + shellQuote(root) + ` 2>/dev/null || exit 0; ` +
		`for d in ./*/; do d=${d#./}; d=${d%/}; ` +
		`[ -n "$d" ] && [ -d "$d" ] && [ ! -L "$d" ] || continue; ` +
		`[ -f "$d/` + sshIdentityEnvName + `" ] && [ -f "$d/` + runtimetoken.TokenFileName + `" ] || continue; ` +
		`ref=$(sed -n 's/^` + runtimetoken.RefEnvName + `=//p' "$d/` + sshIdentityEnvName + `" | sed -n '1p' | tr -d "'"); ` +
		`tok=$(sed -n '1p' "$d/` + runtimetoken.TokenFileName + `" | tr -d '\r\n'); ` +
		`old=0; [ -n "$(find "$d/` + sshIdentityEnvName + `" -mmin +` + strconv.Itoa(mins) + ` 2>/dev/null)" ] && old=1; ` +
		`abs=$(cd "$d" && pwd -P) || continue; ` +
		`printf 'CAND\t%s\t%s\t%s\t%s\n' "$abs" "$ref" "$tok" "$old"; done; echo SWEEP-LIST-DONE`
	out, err := sh.Run(sshShortStepTimeout, script, nil, false)
	if err != nil {
		return nil, err
	}
	var cands []sweepCandidate
	done := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "SWEEP-LIST-DONE" {
			done = true
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 || f[0] != "CAND" {
			continue
		}
		cands = append(cands, sweepCandidate{dir: f[1], ref: f[2], token: f[3], old: f[4] == "1"})
	}
	if !done {
		// A listing that did not finish proves nothing about what is there.
		return nil, fmt.Errorf("the host's listing did not complete")
	}
	return cands, nil
}

func sweepOne(h *sweepHostState, key []byte, c sweepCandidate, stillOwned func(dir, ref string) bool) (SweepFinding, bool) {
	if c.ref == "" || c.token == "" || !path.IsAbs(c.dir) {
		return SweepFinding{}, false
	}
	// PROOF: this AF home's key derives exactly this token from this ref.
	want, err := runtimetoken.DeriveAgentServerToken(key, c.ref)
	if err != nil || subtle.ConstantTimeCompare([]byte(want), []byte(c.token)) != 1 {
		return SweepFinding{}, false
	}
	if h.dirs[c.dir] || h.refs[c.ref] || !c.old {
		return SweepFinding{}, false
	}
	finding := func(a SweepAction, why string) (SweepFinding, bool) {
		return SweepFinding{Host: h.label, Dir: c.dir, Ref: c.ref, Action: a, Reason: why}, true
	}

	state, err := sweepWorkspaceState(h.shell, c.dir)
	if err != nil {
		return finding(SweepReported, "left in place: could not inspect its workspace: "+err.Error())
	}
	if state != "CLEAN" {
		return finding(SweepReported, "left in place: workspace may hold unpushed work ("+state+")")
	}
	pids, perr := sweepFindPIDs(h.shell, c.dir, c.ref)
	if perr != nil {
		return finding(SweepReported, "left in place: "+perr.Error())
	}
	if len(pids) > 1 {
		return finding(SweepReported, "left in place: more than one process claims this session")
	}
	pid := ""
	if len(pids) == 1 {
		pid = pids[0]
	}
	// Last look: a row that persisted while this sweep ran owns it now.
	if stillOwned != nil && stillOwned(c.dir, c.ref) {
		return SweepFinding{}, false
	}
	if err := h.reaper(c.dir, pid, c.ref).reap(); err != nil {
		return finding(SweepReported, "left in place: the reap did not complete: "+err.Error())
	}
	return finding(SweepReaped, "no persisted row owned it and its workspace was clean")
}

// sweepWorkspaceState reports CLEAN only when no unpushed work exists ANYWHERE
// the session's work can live: the workspace and every worktree linked to its
// repository (git worktree list), every local branch (those with an upstream
// against it, the rest against all remotes), the detached HEAD of each worktree,
// and the stash. Anything else, including git being unable to answer at any
// step, is a state the sweep will not delete.
func sweepWorkspaceState(sh sandboxShell, dir string) (string, error) {
	w := dir + "/" + sshWorkspaceSubdir
	script := `w=` + shellQuote(w) + `; nl='
'; ` +
		`if [ ! -e "$w" ]; then echo CLEAN; exit 0; fi; ` +
		`if [ ! -e "$w/.git" ]; then echo "UNKNOWN: not a git workspace"; exit 0; fi; ` +
		`cd "$w" || { echo "UNKNOWN: cannot enter"; exit 0; }; ` +
		`wl=$(git worktree list --porcelain 2>/dev/null) || { echo "UNKNOWN: git worktree list failed"; exit 0; }; ` +
		`paths=$(printf '%s\n' "$wl" | sed -n 's/^worktree //p'); ` +
		`[ -n "$paths" ] || { echo "UNKNOWN: no worktrees listed"; exit 0; }; ` +
		`printf '%s\n' "$wl" | grep -qE '^(prunable|bare)' && { echo "UNKNOWN: a linked worktree is missing or the repository is bare"; exit 0; }; ` +
		`IFS="$nl"; for p in $paths; do ` +
		`s=$(git -C "$p" status --porcelain --untracked-files=all 2>/dev/null) || { echo "UNKNOWN: git status failed in a linked worktree"; exit 0; }; ` +
		`[ -z "$s" ] || { echo "UNCOMMITTED"; exit 0; }; ` +
		`h=$(git -C "$p" log HEAD --not --remotes --oneline -n 1 2>/dev/null) || { echo "UNKNOWN: git log failed in a linked worktree"; exit 0; }; ` +
		`[ -z "$h" ] || { echo "UNPUSHED COMMITS"; exit 0; }; done; ` +
		`u=$(git log --branches --not --remotes --oneline -n 1 2>/dev/null) || { echo "UNKNOWN: git log failed"; exit 0; }; ` +
		`[ -z "$u" ] || { echo "UNPUSHED COMMITS"; exit 0; }; ` +
		`refs=$(git for-each-ref --format='%(refname) %(upstream)' refs/heads 2>/dev/null) || { echo "UNKNOWN: git for-each-ref failed"; exit 0; }; ` +
		`for l in $refs; do IFS=" "; set -- $l; IFS="$nl"; [ -n "$2" ] || continue; git rev-parse --verify -q "$2" >/dev/null 2>&1 || continue; ` +
		`n=$(git rev-list --count "$1" --not "$2" 2>/dev/null) || { echo "UNKNOWN: git rev-list failed"; exit 0; }; ` +
		`[ "$n" = 0 ] || { echo "UNPUSHED COMMITS"; exit 0; }; done; unset IFS; ` +
		`t=$(git stash list 2>/dev/null) || { echo "UNKNOWN: git stash failed"; exit 0; }; ` +
		`[ -z "$t" ] || { echo "STASH"; exit 0; }; echo CLEAN`
	out, err := sh.Run(sshShortStepTimeout, script, nil, false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sweepFindPIDs lists the processes that are provably this session's
// agent-server: argv[0] is the dir's own af binary AND the environment's
// AF_TOKEN_REF equals the ref. A same-named process anywhere else, or one in
// this dir with another ref, is not listed (the latter is an error: it cannot be
// proven and must not be deleted under). A host without /proc cannot be
// enumerated, so nothing there is reaped.
func sweepFindPIDs(sh sandboxShell, dir, ref string) ([]string, error) {
	script := `dir=` + shellQuote(dir) + `; ref=` + shellQuote(ref) + `; af="$dir/` + sshAfBinaryName + `"; ` +
		`[ -r /proc/self/cmdline ] || { echo NOPROC; exit 0; }; ` +
		`for p in /proc/[0-9]*; do pid=${p#/proc/}; ` +
		`a=$(tr '\000' '\n' < "$p/cmdline" 2>/dev/null | sed -n '1p'); [ "$a" = "$af" ] || continue; ` +
		`r=$(tr '\000' '\n' < "$p/environ" 2>/dev/null | sed -n 's/^` + runtimetoken.RefEnvName + `=//p' | sed -n '1p'); ` +
		`if [ "$r" = "$ref" ]; then echo "PID $pid"; else echo "UNPROVEN $pid"; fi; done; echo SWEEP-PID-DONE`
	out, err := sh.Run(sshShortStepTimeout, script, nil, false)
	if err != nil {
		return nil, err
	}
	var pids []string
	done := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "NOPROC":
			return nil, fmt.Errorf("this host has no /proc, so its processes cannot be proven")
		case line == "SWEEP-PID-DONE":
			done = true
		case strings.HasPrefix(line, "UNPROVEN "):
			return nil, fmt.Errorf("a process runs this session's binary but its identity cannot be proven")
		case strings.HasPrefix(line, "PID "):
			pid := strings.TrimPrefix(line, "PID ")
			if !positivePID(pid) {
				return nil, fmt.Errorf("unparseable process id %q", pid)
			}
			pids = append(pids, pid)
		}
	}
	if !done {
		return nil, fmt.Errorf("the process scan did not complete")
	}
	return pids, nil
}
