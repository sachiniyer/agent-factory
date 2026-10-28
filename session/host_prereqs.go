package session

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// hostPrereqScript probes, in ONE round trip on the host's non-interactive
// shell (the same PATH every later provision step sees), the tools a sandbox
// session needs. `sh` is implicit: the script is running in it.
const hostPrereqScript = `if command -v git >/dev/null 2>&1; then echo git=ok; else echo git=missing; fi
if command -v tmux >/dev/null 2>&1; then echo "tmux=$(tmux -V 2>/dev/null)"; else echo tmux=missing; fi
if command -v gh >/dev/null 2>&1; then echo gh=ok; else echo gh=missing; fi`

// minHostTmux is the tmux version floor (the same 3.2 the local runtime
// requires for account-scoped sessions and the `new-session -e` marker).
var minHostTmux = [2]int{3, 2}

// hostPrereqReport is what the probe found. Problems refuse the create;
// Warnings are surfaced but never block it.
type hostPrereqReport struct {
	Problems []string
	Warnings []string
	// Complete is true only when every probed tool (git, tmux, gh) produced a
	// recognised answer. Absence of Problems proves nothing unless Complete.
	Complete bool
}

var tmuxVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

// parseHostPrereqs turns the probe output into a report, each finding naming
// its fix (#5571 W2.3). Output it cannot read yields an empty report: an
// unreadable probe must not refuse a host that may be fine — the provision
// steps that follow fail with their own, specific errors. It also must not
// vouch for one: Complete stays false unless all three answers were recognised.
func parseHostPrereqs(out string) hostPrereqReport {
	var r hostPrereqReport
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "git":
			seen[key] = val == "ok" || val == "missing"
			if val == "missing" {
				r.Problems = append(r.Problems, "git is not on the host's non-interactive PATH — install git there (for example `apt-get install git`), or add its directory to the PATH ssh sets for non-interactive commands")
			}
		case "tmux":
			seen[key] = val == "missing" || tmuxVersionPattern.MatchString(val)
			if val == "missing" {
				r.Problems = append(r.Problems, fmt.Sprintf("tmux is not on the host's non-interactive PATH — install tmux %d.%d or newer there, or add its directory to the PATH ssh sets for non-interactive commands", minHostTmux[0], minHostTmux[1]))
				continue
			}
			if m := tmuxVersionPattern.FindStringSubmatch(val); m != nil {
				major, _ := strconv.Atoi(m[1])
				minor, _ := strconv.Atoi(m[2])
				if major < minHostTmux[0] || (major == minHostTmux[0] && minor < minHostTmux[1]) {
					r.Problems = append(r.Problems, fmt.Sprintf("the host's tmux is %d.%d; af needs %d.%d or newer — upgrade tmux on the host", major, minor, minHostTmux[0], minHostTmux[1]))
				}
			}
		case "gh":
			seen[key] = val == "ok" || val == "missing"
			if val == "missing" {
				r.Warnings = append(r.Warnings, "gh is not installed on the host — the agent cannot open pull requests there until it is")
			}
		}
	}
	r.Complete = seen["git"] && seen["tmux"] && seen["gh"]
	return r
}

// runHostPrereqProbe runs the probe through shell and parses it. A failure of
// the probe itself is returned, not folded into an empty report, so each caller
// decides what an unanswered probe means (a create tolerates it; CheckHost
// reports the host unreachable).
func runHostPrereqProbe(shell sandboxShell, timeout time.Duration) (hostPrereqReport, error) {
	out, err := shell.Run(timeout, hostPrereqScript, nil, false)
	if err != nil {
		return hostPrereqReport{}, err
	}
	return parseHostPrereqs(string(out)), nil
}

// recordHostPrereqVerdict applies the one rule for what a probe may write to
// the readiness cache under key, and returns the blocking error when the host
// cannot run a session. Only a complete, positively parsed answer proves the
// host ready; an empty, garbled or truncated reply records nothing, so it can
// neither mark the host ready nor overwrite an earlier blocked verdict.
func recordHostPrereqVerdict(key string, report hostPrereqReport) error {
	for _, warn := range report.Warnings {
		log.WarningLog.Printf("host prerequisite warning: %s", warn)
	}
	if len(report.Problems) == 0 {
		if report.Complete {
			RecordHostReadiness(key, nil)
		} else {
			log.WarningLog.Printf("host prerequisite probe output was incomplete; not recording a readiness verdict")
		}
		return nil
	}
	blocked := fmt.Errorf("the host cannot run a session: %s", strings.Join(report.Problems, "; "))
	RecordHostReadiness(key, blocked)
	return blocked
}

// checkHostPrereqs runs the probe through the workspace's shell. A failure of
// the probe itself is logged and tolerated (see parseHostPrereqs); only a host
// that answered and is missing something refuses the create.
//
// It also records the verdict in the readiness cache under hostKey (#5571 W2.3
// part B), by recordHostPrereqVerdict's rule.
func (w *sandboxWorkspace) checkHostPrereqs(hostKey string, timeout time.Duration) error {
	report, err := runHostPrereqProbe(w.shell, timeout)
	if err != nil {
		log.WarningLog.Printf("host prerequisite probe did not complete (%v); continuing", err)
		return nil
	}
	return recordHostPrereqVerdict(hostKey, report)
}
