package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// This file owns the test-residue check: the directories af's OWN test harness
// (internal/testguard) creates directly under the temp dir and removes only in
// a cleanup that a killed test binary never reaches — a `go test -timeout`
// panic, a SIGKILL, a killed lane (#4170).
//
// No other check could see them, and not for want of looking. checkStaleTempHomes
// identifies a home by marker files and needs two; a leaked SandboxHome holds
// exactly one (its package's agent-factory.log) and a leaked tmux socket dir
// holds none. Measured on the box the issue was filed from: 3,911 of 3,942
// af-test-home-* dirs held nothing but that log, and 3,665 of 3,700
// af-tmux-pkg-* dirs were empty. Neither a larger budget nor a rerun could ever
// have surfaced them.
//
// The marker rule stays exactly as it is for arbitrary directories — it is what
// stops doctor proposing to delete a stranger's directory that happens to hold
// a file called agent-factory.log. This check does not weaken it; it asks a
// different question that only applies where af chose the NAME, and it
// authorises a removal on the directory's exact CONTENT, the argument
// checkDeadSocketHomes makes:
//
//   - the name is one testresidue.Classify recognises, directly under the temp
//     dir, and the directory is ours;
//   - its content is precisely what the harness leaves behind and nothing else —
//     the package log (plus its rotations), or tmux-<uid>/ dirs holding only
//     sockets — so the most a removal can take is a dead run's test log;
//   - every socket in it refused a dial, nothing has changed in it for
//     MinTempHomeAge, and no live process names it, has a file open inside it,
//     or is working inside it;
//   - the fix re-checks all of that and removes with os.Remove, entry by entry,
//     never os.RemoveAll — anything that appeared since the scan makes it fail.
//
// Anything that does not fit is REPORTED, not removed, unless it is a home
// checkStaleTempHomes already judges on the lock.

const (
	checkTestResidueDir = "test-residue-dir"

	// testResidueMaxEntries bounds how much of one candidate the check reads.
	// The shapes it accepts are tiny — a log and at most log_max_backups
	// rotations, or a tmux-<uid> dir or two — so anything past this is a
	// directory the harness did not leave, and it is decided without listing the
	// rest of it (the #3466 lesson: a consumer of the bounded sweep must not undo
	// the bound by reading a huge candidate in full).
	testResidueMaxEntries = 16
)

// maxTestResidueDirs bounds how many harness directories one run assesses. Each
// costs a handful of syscalls; the bound exists for the same pathological temp
// dir the sweep's own budgets do, and tripping it is reported, not silent. A var
// so a test can exercise the truncation without creating fifty thousand
// directories.
var maxTestResidueDirs = 50000

// The platform's view of open files. Linux exposes /proc/<pid>/fd; a platform
// without it cannot rule out a test binary still writing its log, so a
// SandboxHome is reported rather than removed there. Package vars so a test can
// stage either answer.
var (
	openFilesReadable = procOpenFilesReadable
	processOpenFiles  = readProcessOpenFiles
)

func procOpenFilesReadable() bool {
	_, err := os.ReadDir("/proc/self/fd")
	return err == nil
}

// readProcessOpenFiles returns the paths pid holds open, as the kernel names
// them. An unreadable fd table yields nothing: that is only ever a missing
// reason to KEEP a directory, and the processes that could hold a test log open
// — its own test binary, and that binary's children — belong to the user running
// doctor, whose fd tables are readable. Refusing on every unreadable table would
// refuse on every box running an ssh-agent, which is no protection at all.
func readProcessOpenFiles(pid int) []string {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(fdDir, e.Name())); err == nil {
			paths = append(paths, target)
		}
	}
	return paths
}

// residueEnvKeys are the variables that tie a live process to a harness
// directory. A test binary's own environ does NOT show them — testguard sets
// them with os.Setenv, and /proc/<pid>/environ is the environment the process
// STARTED with — but everything it spawns inherits them: an af daemon under
// test (AGENT_FACTORY_HOME, AF_TESTGUARD_RUN), and every tmux client, server
// and pane process of its private server (TMUX_TMPDIR, TMUX).
var residueEnvKeys = []string{"AGENT_FACTORY_HOME", "AF_TESTGUARD_RUN", "TMUX_TMPDIR", "TMUX"}

// residueRefs is one reading of everything that can show a harness directory is
// still in use. Every entry is a POSITIVE observation: it can spare a directory
// and never authorise a removal.
type residueRefs struct {
	// named maps a directory to why a live process's environment names it.
	named map[string]string
	// open maps a directory to a pid holding a file open somewhere inside it.
	open map[string]int
	// openFilesKnown reports whether this platform exposes open files at all.
	// Without it a SandboxHome cannot be removed: its log is held open by the
	// test binary that owns it for as long as that binary lives.
	openFilesKnown bool
}

func collectResidueRefs(snap map[int]proctree.Process, tempDir string) residueRefs {
	refs := residueRefs{named: map[string]string{}, open: map[string]int{}, openFilesKnown: openFilesReadable()}
	pids := make([]int, 0, len(snap))
	for pid := range snap {
		pids = append(pids, pid)
	}
	sort.Ints(pids) // so the pid a row names is stable across runs
	for _, pid := range pids {
		for _, key := range residueEnvKeys {
			value, found, err := daemonProcessEnvLookup(pid, key)
			if err != nil || !found || value == "" {
				continue
			}
			if key == "TMUX" {
				// "<socket path>,<server pid>,<session index>"
				value, _, _ = strings.Cut(value, ",")
			}
			if dir, ok := residueDirOf(tempDir, value, true); ok {
				if _, seen := refs.named[dir]; !seen {
					refs.named[dir] = fmt.Sprintf("pid %d names it in %s", pid, key)
				}
			}
		}
		if !refs.openFilesKnown {
			continue
		}
		for _, target := range processOpenFiles(pid) {
			if dir, ok := residueDirOf(tempDir, target, false); ok {
				if _, seen := refs.open[dir]; !seen {
					refs.open[dir] = pid
				}
			}
		}
	}
	return refs
}

// residueDirOf maps a path to the first-level directory of tempDir it lies in.
//
// Kernel fd targets are already resolved, so only environment values — which
// are spelled however the process was given them, /var/… for /private/var/… on
// macOS — pay for symlink resolution.
func residueDirOf(tempDir, path string, resolve bool) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false // socket:[…], pipe:[…], anon_inode:…, or a relative value
	}
	if dir, ok := firstLevelUnder(tempDir, filepath.Clean(path)); ok || !resolve {
		return dir, ok
	}
	return firstLevelUnder(tempDir, normalizeHome(path))
}

func firstLevelUnder(tempDir, path string) (string, bool) {
	rel, err := filepath.Rel(tempDir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(tempDir, first), true
}

// inUse returns why a harness directory is in use, if anything live says it is.
func (r residueRefs) inUse(dir string, tmuxHomes map[string]bool, cwds map[int]string) (string, bool) {
	if why, ok := r.named[dir]; ok {
		return why, true
	}
	if tmuxHomes[dir] {
		return "a live tmux session references it", true
	}
	if pid, ok := r.open[dir]; ok {
		return fmt.Sprintf("pid %d has a file open inside it", pid), true
	}
	if pid, ok := processWorkingInside(dir, cwds); ok {
		return fmt.Sprintf("pid %d is working inside it", pid), true
	}
	return "", false
}

// residueRefsNow is the detection pass's reading, taken once per run and only
// if there is a candidate to spend it on.
func (c *scanContext) residueRefsNow(tempDir string) residueRefs {
	if !c.residueRefsScanned {
		c.residueRefs = collectResidueRefs(c.snap, tempDir)
		c.residueRefsScanned = true
	}
	return c.residueRefs
}

// fixTimeResidueRefs is the fix pass's reading, taken once for the pass for the
// reason fixTimeTmuxHomes gives. A process table that cannot be read at fix time
// is an error, not an empty table: a SandboxHome removal rests on seeing that
// nothing has its log open.
func (c *scanContext) fixTimeResidueRefs(tempDir string) (residueRefs, error) {
	if !c.fixResidueScanned {
		c.fixResidueScanned = true
		snap, err := c.opts.snapshot()
		if err != nil {
			c.fixResidueErr = fmt.Errorf("cannot read the process table: %w", err)
		} else {
			c.fixResidueRefs = collectResidueRefs(snap, tempDir)
		}
	}
	return c.fixResidueRefs, c.fixResidueErr
}

// residueShape is what one harness directory holds.
type residueShape struct {
	// owned reports a real directory (not a symlink) owned by this user. A
	// directory that is not is not ours to judge, whatever its name.
	owned bool
	// ok reports that the content is exactly what the harness leaves behind.
	ok bool
	// why describes the content when ok is false.
	why string
	// The removable entries, deepest first when removed: leaf files and sockets,
	// then tmux-<uid> dirs.
	leaves  []string
	sockets []string
	subdirs []string
	// newest is the latest mtime over the directory and everything read.
	newest time.Time
}

var (
	sandboxLogName  = regexp.MustCompile(`^agent-factory\.log(\.[0-9]+)?$`)
	tmuxUserDirName = regexp.MustCompile(`^tmux-[0-9]+$`)
)

func readResidueShape(dir string, kind testresidue.Kind) residueShape {
	var shape residueShape
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !ownedByUs(info) {
		return shape
	}
	shape.owned = true
	shape.newest = info.ModTime()

	entries, err := readBoundedDir(dir)
	if err != nil {
		shape.why = describeUnreadable(err)
		return shape
	}
	shape.ok = true
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		entry, err := os.Lstat(path)
		if err != nil {
			if shape.ok {
				shape.ok, shape.why = false, fmt.Sprintf("%s, which could not be read (%v)", e.Name(), err)
			}
			continue
		}
		shape.newest = later(shape.newest, entry.ModTime())
		switch {
		case kind == testresidue.SandboxHome && sandboxLogName.MatchString(e.Name()) && entry.Mode().IsRegular():
			shape.leaves = append(shape.leaves, path)
		case kind == testresidue.TmuxSocketDir && tmuxUserDirName.MatchString(e.Name()) && entry.IsDir():
			if why := shape.addTmuxUserDir(path); why != "" && shape.ok {
				shape.ok, shape.why = false, why
			}
		default:
			if shape.ok {
				shape.ok, shape.why = false, fmt.Sprintf("%s, which the test harness does not leave behind", e.Name())
			}
		}
	}
	return shape
}

// addTmuxUserDir records one tmux-<uid> dir, which must hold sockets and
// nothing else. It returns why not, when it does not.
func (s *residueShape) addTmuxUserDir(path string) string {
	entries, err := readBoundedDir(path)
	if err != nil {
		return filepath.Base(path) + "/, " + describeUnreadable(err)
	}
	s.subdirs = append(s.subdirs, path)
	why := ""
	for _, e := range entries {
		inner := filepath.Join(path, e.Name())
		info, err := os.Lstat(inner)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			if why == "" {
				why = fmt.Sprintf("%s/%s, which is not a tmux socket", filepath.Base(path), e.Name())
			}
			continue
		}
		s.newest = later(s.newest, info.ModTime())
		s.sockets = append(s.sockets, inner)
	}
	return why
}

var errTooManyEntries = errors.New("holds more than the test harness leaves behind")

// readBoundedDir lists dir, refusing to read past testResidueMaxEntries.
// O_NONBLOCK|O_NOFOLLOW for the reason holdsOnlyADaemonSocket gives: the name
// was a directory when the root was listed, and must still be one now rather
// than a FIFO or a symlink swapped in since.
func readBoundedDir(dir string) ([]os.DirEntry, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), dir)
	defer func() { _ = f.Close() }()
	var all []os.DirEntry
	for {
		batch, err := f.ReadDir(testResidueMaxEntries + 1 - len(all))
		all = append(all, batch...)
		if len(all) > testResidueMaxEntries {
			return nil, errTooManyEntries
		}
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return all, nil
		}
	}
}

func describeUnreadable(err error) string {
	if errors.Is(err, errTooManyEntries) {
		return fmt.Sprintf("more than %d entries, which is more than the test harness leaves behind", testResidueMaxEntries)
	}
	return fmt.Sprintf("content that could not be listed (%v)", err)
}

func ownedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// probeResidueSockets dials every socket. It returns the first one something
// answers on, or the first probe that could not be decided; both empty means
// every socket refused its dial or is gone.
func probeResidueSockets(sockets []string) (live string, undecided error) {
	for _, socket := range sockets {
		deadSocketProbe(socket).Match(
			func() {
				if live == "" {
					live = socket
				}
			},
			func() {},
			func() {},
			func(cause error) {
				if undecided == nil {
					undecided = fmt.Errorf("%s: %w", socket, cause)
				}
			},
		)
	}
	return live, undecided
}

// checkTestResidue reports the directories af's test harness left under the
// temp dir, and offers to remove the ones whose content and liveness it can
// establish. See the top of this file for the argument.
func checkTestResidue(ctx *scanContext, report *Report) {
	tempDir := normalizeHome(ctx.opts.TempDir)
	activeHome := normalizeHome(ctx.opts.ConfigDir)

	// A SandboxHome is an AF home, and a live af tmux session can name one; a
	// listing that failed would spare nothing, so the check stands down (#2874).
	tmuxHomes, tmuxHomesErr := liveTmuxHomes(ctx)
	if tmuxHomesErr != nil {
		report.markIncomplete(checkTestResidueDir)
		report.addAdvisoryFinding(Finding{
			Check: "test-residue-scan",
			Detail: fmt.Sprintf("no test-harness directory could be assessed: cannot establish which AF homes "+
				"live tmux sessions claim (%v)", tmuxHomesErr),
			Severity:    StatusWarn,
			Remediation: "re-run `af doctor` once every live session answers",
		})
		return
	}

	sweep := ctx.tempHomeCandidates()
	// This check reads the ROOT listing, which no candidate budget trims, so it
	// is incomplete only when that listing was. checkStaleTempHomes has already
	// filed the temp-home-scan row saying why.
	if sweep.unreadable || sweep.rootPartial {
		report.markIncomplete(checkTestResidueDir)
	}
	root := filepath.Clean(ctx.opts.TempDir)
	assessed := 0
	for _, name := range sweep.roots {
		kind := testresidue.Classify(name)
		if kind == testresidue.None {
			continue
		}
		if assessed >= maxTestResidueDirs {
			report.markIncomplete(checkTestResidueDir)
			report.addAdvisoryFinding(Finding{
				Check: "test-residue-scan",
				Detail: fmt.Sprintf("this run assessed only the first %d test-harness directories under %s; "+
					"any past that are missing from the counts below", maxTestResidueDirs, tempDir),
				Severity:    StatusWarn,
				Remediation: "run `af doctor --fix` to clear the ones it did assess, then re-run it",
			})
			break
		}
		assessed++
		assessTestResidueDir(ctx, report, normalizeHome(filepath.Join(root, name)), kind, tempDir, activeHome, tmuxHomes)
	}
}

func assessTestResidueDir(ctx *scanContext, report *Report, dir string, kind testresidue.Kind,
	tempDir, activeHome string, tmuxHomes map[string]bool) {
	// Resolved, then required to sit DIRECTLY in the temp dir: a symlink by that
	// name resolves elsewhere and is not a directory the harness made here.
	if dir == activeHome || filepath.Dir(dir) != tempDir {
		return
	}
	shape := readResidueShape(dir, kind)
	if !shape.owned {
		return
	}
	age := timeSince(shape.newest)
	if age < ctx.opts.MinTempHomeAge {
		return // possibly a test run still going
	}
	if !shape.ok {
		if isAFHome(dir) {
			return // checkStaleTempHomes judges a real home, on the kernel's lock answer
		}
		report.addAdvisoryFinding(Finding{
			Check: checkTestResidueDir,
			Detail: fmt.Sprintf("%s was made by af's test harness and untouched for %s, but it holds %s — "+
				"reported, not removed", dir, formatAge(age.Seconds()), shape.why),
			Severity:    StatusWarn,
			Remediation: "inspect it, then `" + shellsuggest.Command("rm", "-r", dir) + "` if nothing needs it",
		})
		return
	}

	refs := ctx.residueRefsNow(tempDir)
	if why, held := refs.inUse(dir, tmuxHomes, ctx.liveWorkingDirs()); held {
		report.Pass(sectionProcesses, "test residue", fmt.Sprintf("%s is in use (%s)", dir, why))
		return
	}
	if live, undecided := probeResidueSockets(shape.sockets); live != "" {
		// Not removable, and worth saying: a private tmux server that outlived
		// the test that started it keeps running until someone stops it.
		report.addAdvisoryFinding(Finding{
			Check: checkTestResidueDir,
			Detail: fmt.Sprintf("%s is a test's private tmux socket dir, and a tmux server still answers on %s "+
				"although nothing in the directory has changed for %s — likely a server that outlived its test; "+
				"reported, not removed", dir, live, formatAge(age.Seconds())),
			Severity: StatusWarn,
			Remediation: "check it with `" + shellsuggest.Command("tmux", "-S", live, "ls") + "`, and stop it with `" +
				shellsuggest.Command("tmux", "-S", live, "kill-server") + "` if no test run owns it",
		})
		return
	} else if undecided != nil {
		report.addAdvisoryFinding(Finding{
			Check: checkTestResidueDir,
			Detail: fmt.Sprintf("%s was left by af's test harness, but whether anything is serving its socket "+
				"could not be determined (%v) — reported, not removed", dir, undecided),
			Severity:    StatusWarn,
			Remediation: "verify nothing is using it, then `" + shellsuggest.Command("rm", "-r", dir) + "`",
		})
		return
	}
	if kind == testresidue.SandboxHome && (ctx.snap == nil || !refs.openFilesKnown) {
		report.addAdvisoryFinding(Finding{
			Check: checkTestResidueDir,
			Detail: fmt.Sprintf("%s is a test run's sandbox home holding only its log, untouched for %s, but "+
				"this run cannot see which files processes hold open, so a test binary still writing that log "+
				"cannot be ruled out — reported, not removed", dir, formatAge(age.Seconds())),
			Severity:    StatusWarn,
			Remediation: "verify no test run is using it, then `" + shellsuggest.Command("rm", "-r", dir) + "`",
		})
		return
	}

	what := "a test's private tmux socket dir with no server answering in it"
	if kind == testresidue.SandboxHome {
		what = "a test run's sandbox home holding only that run's log, which nothing has open"
		if len(shape.leaves) == 0 {
			what = "an empty sandbox home from a test run"
		}
	}
	report.addActionableFinding(Finding{
		Check: checkTestResidueDir,
		Detail: fmt.Sprintf("%s is %s, untouched for %s — af's test harness left it behind when its run "+
			"ended without reaching its cleanup", dir, what, formatAge(age.Seconds())),
		FixAction:   "remove " + dir,
		fix:         testResidueRemoveFix(ctx, dir, tempDir, activeHome, kind),
		Severity:    StatusWarn,
		Remediation: "run `af doctor --fix` to remove it, or `" + shellsuggest.Command("rm", "-r", dir) + "`",
	})
}

// testResidueRemoveFix removes one harness directory after re-establishing
// every precondition, since findings are applied after detection. Removal is
// entry by entry with os.Remove: a file that appeared in the window makes the
// final os.Remove of the directory fail rather than be taken with it.
func testResidueRemoveFix(ctx *scanContext, dir, tempDir, activeHome string, kind testresidue.Kind) func() error {
	return func() error {
		if dir == activeHome {
			return fmt.Errorf("refusing to remove the active home %s", dir)
		}
		if filepath.Dir(dir) != tempDir || testresidue.Classify(filepath.Base(dir)) != kind {
			return fmt.Errorf("refusing to remove %s: it is not a test-harness directory directly under %s", dir, tempDir)
		}
		shape := readResidueShape(dir, kind)
		if !shape.owned || !shape.ok {
			return fmt.Errorf("refusing to remove %s: it no longer holds only what the test harness leaves behind", dir)
		}
		if timeSince(shape.newest) < ctx.opts.MinTempHomeAge {
			return fmt.Errorf("refusing to remove %s: it has changed since the scan", dir)
		}
		if live, undecided := probeResidueSockets(shape.sockets); live != "" {
			return fmt.Errorf("refusing to remove %s: a tmux server now answers on %s", dir, live)
		} else if undecided != nil {
			return fmt.Errorf("refusing to remove %s: cannot establish that nothing serves its socket: %w", dir, undecided)
		}
		tmuxHomes, err := ctx.fixTimeTmuxHomes()
		if err != nil {
			return fmt.Errorf("refusing to remove %s: cannot check whether a live tmux session references it: %w", dir, err)
		}
		// A SandboxHome removal rests on seeing that nothing has its log open, so
		// an unreadable table refuses it. A tmux socket dir holds nothing to lose,
		// so for it the table is only ever a reason to keep — the direction
		// fixTimeWorkingDirs takes for the same reason.
		refs, err := ctx.fixTimeResidueRefs(tempDir)
		if kind == testresidue.SandboxHome {
			if err != nil {
				return fmt.Errorf("refusing to remove %s: %w", dir, err)
			}
			if !refs.openFilesKnown {
				return fmt.Errorf("refusing to remove %s: cannot see which files processes hold open", dir)
			}
		}
		if why, held := refs.inUse(dir, tmuxHomes, ctx.fixTimeWorkingDirs()); held {
			return fmt.Errorf("refusing to remove %s: it is now in use (%s)", dir, why)
		}
		for _, path := range append(append([]string(nil), shape.leaves...), shape.sockets...) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
		for _, sub := range shape.subdirs {
			if err := os.Remove(sub); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", sub, err)
			}
		}
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	}
}
