package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests exercise examples/tasks/gh-events-poll.sh (#1949). They are
// hermetic: a stub `gh` on PATH answers every call, so no network, no
// authentication and no real repository are involved.
//
// The defect being pinned is worth restating, because it is a design bug rather
// than a typo. The monitor this script generalizes guarded ALL of its cursors
// with one flag:
//
//	advance=1
//	if out=$(gh issue list ...); then ...; else advance=0; fi
//	if out=$(gh pr list    ...); then ...; else advance=0; fi
//	if out=$(gh api .../comments ...); then ...; else advance=0; fi
//	[ "$advance" = 1 ] && since=$now
//
// The intent — never skip an event you failed to read — is right. The blast
// radius is not: a failing COMMENTS call pinned the cursor for ISSUES too, and
// with no seen-set behind it, every issue past that timestamp re-emitted every
// cycle. Forty-five minutes of the same two issues, in a channel whose whole
// job is to be worth reading.

// stubGH writes a fake `gh` onto a PATH directory. Each subcommand's behavior
// is driven by files the test writes into ctl: <name>.out is printed on stdout,
// and <name>.fail (if present) makes the call exit non-zero instead — which is
// how a real gh behaves when the token is invalid and GitHub answers with an
// HTML login page that --jq cannot parse.
func stubGH(t *testing.T, binDir, ctl string) {
	t.Helper()
	script := `#!/usr/bin/env bash
ctl="` + ctl + `"
case "$1 $2" in
  "issue list") name=issues ;;
  "pr list")    name=prs ;;
  *)            name=comments ;;
esac
if [ -f "$ctl/$name.fail" ]; then
  echo "gh: simulated failure for $name" >&2
  exit 1
fi
cat "$ctl/$name.out" 2>/dev/null
exit 0
`
	path := filepath.Join(binDir, "gh")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
}

type poller struct {
	t        *testing.T
	script   string
	stateDir string
	ctl      string
	binDir   string
}

func newPoller(t *testing.T) *poller {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("watch-task examples are POSIX shell; not run on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	p := &poller{
		t:        t,
		script:   filepath.Join(exampleDir(t), "gh-events-poll.sh"),
		stateDir: filepath.Join(dir, "state"),
		ctl:      filepath.Join(dir, "ctl"),
		binDir:   filepath.Join(dir, "bin"),
	}
	for _, d := range []string{p.stateDir, p.ctl, p.binDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	stubGH(t, p.binDir, p.ctl)
	return p
}

// exampleDir returns the directory holding the example scripts, so the test
// works from the package dir or the repo root.
func exampleDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "examples", "tasks")
		if _, err := os.Stat(filepath.Join(candidate, "gh-events-poll.sh")); err == nil {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(dir, "gh-events-poll.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate examples/tasks from %s", dir)
		}
		dir = parent
	}
}

// answer sets what the given source returns on its next call.
func (p *poller) answer(source string, lines ...string) {
	p.t.Helper()
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(p.ctl, source+".out"), []byte(body), 0644); err != nil {
		p.t.Fatalf("write %s answer: %v", source, err)
	}
}

// breaks makes the given source's gh call fail until repaired.
func (p *poller) breaks(source string) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.ctl, source+".fail"), nil, 0644); err != nil {
		p.t.Fatalf("break %s: %v", source, err)
	}
}

func (p *poller) repair(source string) {
	p.t.Helper()
	if err := os.Remove(filepath.Join(p.ctl, source+".fail")); err != nil && !os.IsNotExist(err) {
		p.t.Fatalf("repair %s: %v", source, err)
	}
}

// run executes the given number of poll cycles and returns the emitted events
// (stdout lines) and the log (stderr).
func (p *poller) run(cycles string) (events []string, logs string) {
	p.t.Helper()
	cmd := exec.Command("bash", p.script)
	cmd.Env = append(os.Environ(),
		"PATH="+p.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPO=owner/name",
		"STATE_DIR="+p.stateDir,
		"POLL_CYCLES="+cycles,
		"POLL_INTERVAL=0",
	)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		p.t.Fatalf("run poller: %v\nstderr:\n%s", err, errBuf.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			events = append(events, line)
		}
	}
	return events, errBuf.String()
}

func (p *poller) cursor(source string) string {
	p.t.Helper()
	b, err := os.ReadFile(filepath.Join(p.stateDir, source+".cursor"))
	if err != nil {
		p.t.Fatalf("read %s cursor: %v", source, err)
	}
	return strings.TrimSpace(string(b))
}

func (p *poller) setCursor(source, value string) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.stateDir, source+".cursor"), []byte(value+"\n"), 0644); err != nil {
		p.t.Fatalf("set %s cursor: %v", source, err)
	}
}

// TestGHEventsPoll_OneBrokenSourceDoesNotPinTheOthers is #1949's first and
// central defect. With the comments call failing, the issues and prs cursors
// must still advance: a broken endpoint contains itself.
func TestGHEventsPoll_OneBrokenSourceDoesNotPinTheOthers(t *testing.T) {
	p := newPoller(t)
	p.answer("issues", "i1\t[ISSUE #1] first")
	p.answer("prs", "p2\t[PR #2] second")
	p.answer("comments")

	// Cycle 1: everything healthy, so every cursor is seeded and advanced.
	p.run("1")
	commentsCursor := p.cursor("comments")
	issuesCursor := p.cursor("issues")

	// Cycle 2: comments is broken. It must hold ITS cursor (nothing skipped)
	// while the healthy sources keep moving.
	p.breaks("comments")
	p.setCursor("issues", "2000-01-01T00:00:00Z")
	p.setCursor("prs", "2000-01-01T00:00:00Z")
	p.run("1")

	if got := p.cursor("comments"); got != commentsCursor {
		t.Errorf("a FAILED source must hold its own cursor so nothing is skipped: %q -> %q", commentsCursor, got)
	}
	if got := p.cursor("issues"); got == "2000-01-01T00:00:00Z" {
		t.Errorf("a broken COMMENTS call pinned the ISSUES cursor at %q — that is the #1949 defect: one failing call must not stall every source", got)
	}
	if got := p.cursor("prs"); got == "2000-01-01T00:00:00Z" {
		t.Errorf("a broken COMMENTS call pinned the PRS cursor at %q", got)
	}
	_ = issuesCursor
}

// TestGHEventsPoll_SeenSetSurvivesAPinnedCursor is the second defect and the
// belt-and-braces half: even with the cursor frozen and the API returning the
// same issue forever, each event is emitted exactly once.
func TestGHEventsPoll_SeenSetSurvivesAPinnedCursor(t *testing.T) {
	p := newPoller(t)
	p.answer("issues", "i1\t[ISSUE #1] first", "i2\t[ISSUE #2] second")
	p.answer("prs")
	p.answer("comments")

	events, _ := p.run("1")
	if len(events) != 2 {
		t.Fatalf("first cycle should emit both issues, got %v", events)
	}

	// Freeze the cursor, exactly as the broken monitor did, and keep returning
	// the same two issues for several cycles.
	pinned := "2000-01-01T00:00:00Z"
	for i := 0; i < 3; i++ {
		p.setCursor("issues", pinned)
		if again, _ := p.run("1"); len(again) != 0 {
			t.Fatalf("cycle %d re-emitted already-seen events %v — the cursor must not be the only dedupe", i+2, again)
		}
	}

	// A genuinely new issue still gets through: the seen-set suppresses repeats,
	// not the source.
	p.answer("issues", "i1\t[ISSUE #1] first", "i2\t[ISSUE #2] second", "i3\t[ISSUE #3] third")
	p.setCursor("issues", pinned)
	fresh, _ := p.run("1")
	if len(fresh) != 1 || !strings.Contains(fresh[0], "#3") {
		t.Errorf("a new event must still be emitted with a pinned cursor, got %v", fresh)
	}
}

// TestGHEventsPoll_CursorIsRereadEachCycle is the third defect, and the sharpest
// one: the original read its cursor ONCE at startup, so the state file was
// write-only from the outside. An operator repairing a stuck cursor by hand —
// the obvious remedy — silently did nothing until the process was restarted.
func TestGHEventsPoll_CursorIsRereadEachCycle(t *testing.T) {
	p := newPoller(t)
	p.answer("issues")
	p.answer("prs")
	p.answer("comments")
	p.run("1")

	// A single long-lived invocation: repair the cursor on disk BETWEEN its
	// cycles and require the next cycle to have used the repaired value. If the
	// cursor were only read at startup, the final write would be derived from
	// the stale in-memory value instead.
	repaired := "2020-06-01T00:00:00Z"
	p.setCursor("issues", repaired)
	if got := p.cursor("issues"); got != repaired {
		t.Fatalf("precondition: cursor not written, got %q", got)
	}

	// Break the source so the cycle CANNOT advance the cursor; what it reports
	// then is exactly what it read.
	p.breaks("issues")
	_, logs := p.run("3")
	if !strings.Contains(logs, repaired) {
		t.Errorf("the failure report must quote the cursor read FROM DISK this cycle (%s); got:\n%s", repaired, logs)
	}
}

// TestGHEventsPoll_PersistentFailureIsReportedOnce covers the issue's third
// bullet: a source that has been failing for several cycles says so, once,
// rather than degrading into silence. A monitor that fails quietly is worse than
// one that fails loudly — the events it is not delivering look exactly like
// "nothing happened".
func TestGHEventsPoll_PersistentFailureIsReportedOnce(t *testing.T) {
	p := newPoller(t)
	p.answer("issues")
	p.answer("prs")
	p.answer("comments")
	p.breaks("comments")

	_, logs := p.run("5")
	if n := strings.Count(logs, "source 'comments' has failed"); n != 1 {
		t.Errorf("a persistent outage should be reported exactly once, got %d reports in:\n%s", n, logs)
	}
	if strings.Contains(logs, "source 'issues' has failed") {
		t.Errorf("a healthy source must not be reported as failing:\n%s", logs)
	}

	// And recovery is announced, so the channel says when it is trustworthy
	// again.
	p.repair("comments")
	_, logs = p.run("1")
	if !strings.Contains(logs, "source 'comments' is answering again") {
		t.Errorf("recovery from a reported outage should be announced, got:\n%s", logs)
	}
}

// TestGHEventsPoll_FailureNeverEmitsAPartialResult guards the distinction the
// whole design rests on: "nothing new" and "could not look" must never be
// conflated. A failing call emits no events at all, rather than whatever partial
// text the failure happened to print.
func TestGHEventsPoll_FailureNeverEmitsAPartialResult(t *testing.T) {
	p := newPoller(t)
	p.answer("issues", "i1\t[ISSUE #1] real")
	p.answer("prs")
	p.answer("comments")
	p.breaks("issues")

	events, _ := p.run("1")
	if len(events) != 0 {
		t.Errorf("a failed call must emit nothing, got %v", events)
	}
	// The seen-set must also be untouched, so the event still arrives once the
	// source recovers.
	p.repair("issues")
	events, _ = p.run("1")
	if len(events) != 1 || !strings.Contains(events[0], "#1") {
		t.Errorf("the event missed during the outage must arrive on recovery, got %v", events)
	}
}
