package doctor

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// #4170: af doctor could not see a single directory af's own test harness
// leaves behind when a run is killed — 7,475 of them on the box the issue was
// filed from — because the temp-home sweep identifies a home by two marker files
// and these hold one or none.

// residueOptions is macLikeTempHomeOptions with a READABLE process table (empty
// unless pids are given). A SandboxHome removal rests on seeing that nothing has
// its log open, so these tests must not run blind by default; the blind case has
// its own test.
func residueOptions(t *testing.T, tempRoot string, fix bool, pids ...int) Options {
	t.Helper()
	opts := macLikeTempHomeOptions(t, tempRoot, fix)
	opts.snapshot = snapshotOf(t, pids...)
	return opts
}

// stubOpenFiles pins the open-file view: readable, and pid → the paths it holds.
// Staged so a test asserts doctor's decision on every runner, macOS included,
// rather than whatever the machine happens to have open.
func stubOpenFiles(t *testing.T, readable bool, open map[int][]string) {
	t.Helper()
	prevReadable, prevOpen := openFilesReadable, processOpenFiles
	t.Cleanup(func() { openFilesReadable, processOpenFiles = prevReadable, prevOpen })
	openFilesReadable = func() bool { return readable }
	processOpenFiles = func(pid int) []string { return open[pid] }
}

// ageTree backdates every path, deepest first, so the directory's own mtime is
// not bumped back to now by the entries written into it.
func ageTree(t *testing.T, paths ...string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	for i := len(paths) - 1; i >= 0; i-- {
		require.NoError(t, os.Chtimes(paths[i], old, old))
	}
}

// sandboxHomeResidue is a leaked testguard.SandboxHome: the directory and
// whatever files it was left holding (by default, the package log).
func sandboxHomeResidue(t *testing.T, root, name string, files ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	paths := []string{dir}
	for _, f := range files {
		p := filepath.Join(dir, f)
		require.NoError(t, os.WriteFile(p, []byte("a test run's log line\n"), 0o600))
		paths = append(paths, p)
	}
	ageTree(t, paths...)
	return dir
}

// tmuxResidue is a leaked SandboxTmux/IsolateTmux dir in one of its three
// shapes: "empty", "userdir" (an empty tmux-<uid>/), or "dead" (a socket with
// nothing listening). "live" keeps a listener open for the whole test.
func tmuxResidue(t *testing.T, root, name, shape string) (dir, socket string) {
	t.Helper()
	dir = filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	if shape == "empty" {
		ageTree(t, dir)
		return dir, ""
	}
	user := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()))
	require.NoError(t, os.MkdirAll(user, 0o700))
	paths := []string{dir, user}
	if shape == "dead" || shape == "live" {
		socket = filepath.Join(user, "default")
		l, err := net.Listen("unix", socket)
		require.NoError(t, err)
		if shape == "dead" {
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			require.NoError(t, l.Close())
		} else {
			t.Cleanup(func() { _ = l.Close() })
		}
		paths = append(paths, socket)
	}
	ageTree(t, paths...)
	return dir, socket
}

func residueFinding(t *testing.T, r *Report, dir string) Finding {
	t.Helper()
	// A no-op for every caller here, by construction: each root comes from
	// socketTempHome, which resolved it while it existed, so every path built
	// under it is already canonical. It has to be, because resolution is only
	// reliable while the path exists — this is called after --fix runs that
	// removed the directory, and on a removed path normalizeHome falls back to
	// the unresolved spelling, which on macOS never matches the /private/tmp/…
	// doctor reports (#4496). Kept so a caller passing a path that still exists
	// is compared correctly, not as the mechanism these tests rely on.
	dir = normalizeHome(dir)
	var found []Finding
	for _, f := range findByCheck(r, checkTestResidueDir) {
		if strings.HasPrefix(f.Detail, dir+" ") {
			found = append(found, f)
		}
	}
	require.Len(t, found, 1, "want exactly one test-residue finding for %s, have %v", dir, findByCheck(r, checkTestResidueDir))
	return found[0]
}

func renderedRow(t *testing.T, r *Report, name string) string {
	t.Helper()
	for _, row := range renderRows(r, false, false) {
		if row.name == name {
			return row.detail
		}
	}
	t.Fatalf("no %q row rendered", name)
	return ""
}

func TestASandboxHomeHoldingOnlyItsLogIsReportedThenRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	dir := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"1111", "agent-factory.log", "agent-factory.log.1")
	empty := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"2222")

	report, err := Run(residueOptions(t, root, false))
	require.NoError(t, err)
	for _, d := range []string{dir, empty} {
		f := residueFinding(t, report, d)
		require.True(t, f.Actionable, f.Detail)
		require.NotEmpty(t, f.FixAction)
		require.DirExists(t, d, "a report-only run removes nothing")
	}
	require.Contains(t, renderedRow(t, report, "test-residue-dirs"), "2 directories af's own test harness left")

	report, err = Run(residueOptions(t, root, true))
	require.NoError(t, err)
	for _, d := range []string{dir, empty} {
		f := residueFinding(t, report, d)
		require.True(t, f.Fixed, "fix error: %v", f.FixErr)
		require.NoDirExists(t, d)
	}
}

func TestEveryTmuxSocketDirShapeWithNoServerIsRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	var dirs []string
	for i, shape := range []string{"empty", "userdir", "dead"} {
		prefix := testresidue.PackageTmuxPrefix
		if i%2 == 1 {
			prefix = testresidue.TestTmuxPrefix
		}
		dir, _ := tmuxResidue(t, root, fmt.Sprintf("%s%d", prefix, 1000+i), shape)
		dirs = append(dirs, dir)
	}

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)
	for _, dir := range dirs {
		f := residueFinding(t, report, dir)
		require.True(t, f.Actionable, f.Detail)
		require.True(t, f.Fixed, "fix error for %s: %v", dir, f.FixErr)
		require.NoDirExists(t, dir)
	}
}

// A private server that outlived its test is worth naming, and it is not
// debris: something is answering on it.
func TestALiveTmuxServerIsReportedNotRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	live, socket := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"5555", "live")
	dead, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"6666", "dead") // the witness

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)

	f := residueFinding(t, report, live)
	require.False(t, f.Actionable)
	require.Empty(t, f.FixAction, "a socket something answers on must never carry a removal")
	require.Contains(t, f.Detail, "still answers on "+normalizeHome(socket))
	require.Contains(t, f.Remediation, "kill-server")
	require.FileExists(t, socket)
	require.NoDirExists(t, dead)
}

// The real signal, end to end, where the platform has it: a test binary holds
// its package log open for as long as it lives.
func TestASandboxHomeWhoseLogIsOpenIsSpared(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads real open files through /proc; the decision itself is covered on every platform below")
	}
	root := socketTempHome(t)
	dir := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"3333", "agent-factory.log")
	log, err := os.OpenFile(filepath.Join(dir, "agent-factory.log"), os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	ageTree(t, filepath.Join(dir, "agent-factory.log"), dir) // opening it touched nothing, but be exact

	report, err := Run(residueOptions(t, root, true, os.Getpid()))
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, checkTestResidueDir))
	require.DirExists(t, dir)
	require.True(t, okContains(report, fmt.Sprintf("%s is in use (pid %d has a file open inside it)",
		normalizeHome(dir), os.Getpid())),
		"the check must have looked and SAID why it kept this one")
}

// A test binary's own environ does not show what testguard set with os.Setenv,
// but everything it spawns does — including every process of its private tmux
// server.
func TestAProcessNamingAHarnessDirInItsEnvironmentSparesIt(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	home := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"4444", "agent-factory.log")
	byTmpdir, _ := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"4445", "userdir")
	byTmux, socket := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"4446", "dead")
	witness, _ := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"4447", "empty")

	daemon := spawnWithEnv(t, "test-daemon", nil, map[string]string{"AF_TESTGUARD_RUN": home})
	server := spawnWithEnv(t, "tmux-server", nil, map[string]string{"TMUX_TMPDIR": byTmpdir})
	pane := spawnWithEnv(t, "pane", nil, map[string]string{"TMUX": socket + ",123,0"})

	report, err := Run(residueOptions(t, root, true, daemon.PID, server.PID, pane.PID))
	require.NoError(t, err)

	for dir, by := range map[string]struct {
		pid int
		key string
	}{home: {daemon.PID, "AF_TESTGUARD_RUN"}, byTmpdir: {server.PID, "TMUX_TMPDIR"}, byTmux: {pane.PID, "TMUX"}} {
		require.DirExists(t, dir)
		want := fmt.Sprintf("%s is in use (pid %d names it in %s)", normalizeHome(dir), by.pid, by.key)
		require.True(t, okContains(report, want), "no row saying %q", want)
	}
	require.Len(t, findByCheck(report, checkTestResidueDir), 1, "only the witness")
	require.NoDirExists(t, witness)
}

// Content is what licenses the removal, so anything beyond what the harness
// leaves is reported and kept — and a real home stays with the check that
// judges it on the kernel's lock.
func TestAHarnessDirHoldingMoreThanTheHarnessLeavesIsKept(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	notes := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"7001", "agent-factory.log", "notes.txt")
	realHome := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"7002", "agent-factory.log", "config.toml")
	tmuxDir, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"7003", "userdir")
	stray := filepath.Join(tmuxDir, fmt.Sprintf("tmux-%d", os.Getuid()), "not-a-socket")
	require.NoError(t, os.WriteFile(stray, []byte("x"), 0o600))
	ageTree(t, tmuxDir, filepath.Dir(stray), stray)

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)

	f := residueFinding(t, report, notes)
	require.False(t, f.Actionable)
	require.Empty(t, f.FixAction)
	require.Contains(t, f.Detail, "notes.txt")
	require.FileExists(t, filepath.Join(notes, "notes.txt"))

	f = residueFinding(t, report, tmuxDir)
	require.False(t, f.Actionable)
	require.Contains(t, f.Detail, "not-a-socket")
	require.FileExists(t, stray)

	for _, rf := range findByCheck(report, checkTestResidueDir) {
		require.NotContains(t, rf.Detail, realHome, "a two-marker home is checkStaleTempHomes' to judge")
	}
	require.FileExists(t, filepath.Join(realHome, "config.toml"))
}

// stampResidue writes testguard's owner stamp files into a harness dir, as
// every one of them carries since #4468, and re-ages the dir so writing them
// does not make it read as a live run.
func stampResidue(t *testing.T, dir string, names ...string) {
	t.Helper()
	paths := []string{dir}
	for _, name := range names {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("4242\t99\tboot\tns\n"), 0o644))
		paths = append(paths, p)
	}
	ageTree(t, paths...)
}

// #4468 stamps every harness dir with an owner file (and, for a binary killed
// mid-write, its rename temp). That stamp is harness content in both kinds: a
// recogniser that reads it as foreign would report every dir the harness now
// leaves behind instead of clearing it.
func TestAHarnessDirHoldingTheOwnerStampIsStillRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	homeWithLog := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9901", "agent-factory.log")
	stampResidue(t, homeWithLog, testresidue.OwnerStampFile)
	homeMidStamp := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9902")
	stampResidue(t, homeMidStamp, testresidue.OwnerStampTempFile)
	pkgDead, socket := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"9903", "dead")
	stampResidue(t, pkgDead, testresidue.OwnerStampFile)
	testEmpty, _ := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"9904", "empty")
	stampResidue(t, testEmpty, testresidue.OwnerStampFile, testresidue.OwnerStampTempFile)
	dirs := []string{homeWithLog, homeMidStamp, pkgDead, testEmpty}

	report, err := Run(residueOptions(t, root, false))
	require.NoError(t, err)
	for _, dir := range dirs {
		f := residueFinding(t, report, dir)
		require.True(t, f.Actionable, f.Detail)
		require.NotContains(t, f.Detail, "does not leave behind")
	}
	require.Contains(t, residueFinding(t, report, homeWithLog).Detail, "that run's log and its owner stamp")
	require.Contains(t, residueFinding(t, report, homeMidStamp).Detail, "holding only its owner stamp")

	report, err = Run(residueOptions(t, root, true))
	require.NoError(t, err)
	for _, dir := range dirs {
		f := residueFinding(t, report, dir)
		require.True(t, f.Fixed, "fix error for %s: %v", dir, f.FixErr)
		require.NoDirExists(t, dir)
	}
	require.NoFileExists(t, socket)
}

// Only those two names, and only as regular files. Anything else wearing the
// name, or near it, is content the harness did not leave, and --fix refuses it.
func TestSomethingMerelyNamedLikeTheOwnerStampIsKept(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)

	stampDir := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9911", "agent-factory.log")
	inner := filepath.Join(stampDir, testresidue.OwnerStampFile, "work")
	require.NoError(t, os.MkdirAll(filepath.Dir(inner), 0o700))
	require.NoError(t, os.WriteFile(inner, []byte("do not delete me"), 0o600))
	ageTree(t, stampDir, filepath.Dir(inner), inner)

	stampLink, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"9912", "empty")
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("do not delete me"), 0o600))
	link := filepath.Join(stampLink, testresidue.OwnerStampTempFile)
	require.NoError(t, os.Symlink(target, link))
	// Chtimes follows the link; the link's own mtime is what doctor reads, and
	// left at now it would spare the dir as young rather than judge the link.
	old := unix.NsecToTimeval(time.Now().Add(-48 * time.Hour).UnixNano())
	require.NoError(t, unix.Lutimes(link, []unix.Timeval{old, old}))
	ageTree(t, stampLink)

	nearName, _ := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"9913", "empty")
	stampResidue(t, nearName, testresidue.OwnerStampFile+".bak")

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)
	for dir, name := range map[string]string{
		stampDir:  testresidue.OwnerStampFile,
		stampLink: testresidue.OwnerStampTempFile,
		nearName:  testresidue.OwnerStampFile + ".bak",
	} {
		f := residueFinding(t, report, dir)
		require.False(t, f.Actionable, f.Detail)
		require.Empty(t, f.FixAction)
		require.Contains(t, f.Detail, name+", which the test harness does not leave behind")
		require.DirExists(t, dir)
	}
	require.FileExists(t, inner)
	require.FileExists(t, target)
}

// The fix re-reads the shape of a stamped dir too: a stranger's file added after
// the scan, or the stamp swapped for a directory, refuses the removal before a
// single entry — the stamp included — is taken.
func TestTheResidueFixReReadsAStampedDirectory(t *testing.T) {
	for name, tamper := range map[string]func(t *testing.T, dir string) string{
		"gained a file": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "someone-elses-work")
			require.NoError(t, os.WriteFile(p, []byte("do not delete me"), 0o600))
			ageTree(t, dir, p)
			return p
		},
		"stamp became a directory": func(t *testing.T, dir string) string {
			stamp := filepath.Join(dir, testresidue.OwnerStampFile)
			require.NoError(t, os.Remove(stamp))
			p := filepath.Join(stamp, "work")
			require.NoError(t, os.MkdirAll(stamp, 0o700))
			require.NoError(t, os.WriteFile(p, []byte("do not delete me"), 0o600))
			ageTree(t, dir, stamp, p)
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := socketTempHome(t)
			stubOpenFiles(t, true, nil)
			dir, socket := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"9921", "dead")
			stampResidue(t, dir, testresidue.OwnerStampFile, testresidue.OwnerStampTempFile)
			ctx, err := newScanContext(residueOptions(t, root, true))
			require.NoError(t, err)
			require.True(t, readResidueShape(normalizeHome(dir), testresidue.TmuxSocketDir).ok,
				"precondition: the stamped dir is removable as scanned")
			fix := testResidueRemoveFix(ctx, normalizeHome(dir), normalizeHome(root), normalizeHome(ctx.opts.ConfigDir),
				testresidue.TmuxSocketDir)

			kept := tamper(t, dir)

			err = fix()
			require.Error(t, err)
			require.Contains(t, err.Error(), "no longer holds only what the test harness leaves behind",
				"refused for the content, not for some other reason")
			require.FileExists(t, kept)
			require.FileExists(t, filepath.Join(dir, testresidue.OwnerStampTempFile), "nothing is removed before the refusal")
			require.FileExists(t, socket)
		})
	}
}

func TestAYoungHarnessDirIsLeftAlone(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	young := filepath.Join(root, testresidue.PackageTmuxPrefix+"8001")
	require.NoError(t, os.MkdirAll(young, 0o700)) // mtime: now — a test run may still be going

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, checkTestResidueDir))
	require.DirExists(t, young)
}

func TestNamesTheHarnessDidNotMakeAreIgnored(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	var dirs []string
	for _, name := range []string{"af-tmux-pkg-", "af-tmux-abc", "af-test-home-12x", "af-1450838862", "xaf-tmux-1"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		ageTree(t, dir)
		dirs = append(dirs, dir)
	}
	// A symlink wearing a harness name resolves somewhere else, and that
	// somewhere is not a directory the harness made here.
	elsewhere := t.TempDir()
	ageTree(t, elsewhere)
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(root, testresidue.TestTmuxPrefix+"9001")))

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, checkTestResidueDir))
	for _, dir := range dirs {
		require.DirExists(t, dir)
	}
	require.DirExists(t, elsewhere)
}

// The fix re-reads the content: a directory that gained a file between the scan
// and the removal fails instead of being swept up with it.
func TestTheResidueFixRefusesADirectoryThatGainedAFile(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	dir, _ := tmuxResidue(t, root, testresidue.TestTmuxPrefix+"9100", "userdir")
	ctx, err := newScanContext(residueOptions(t, root, true))
	require.NoError(t, err)
	fix := testResidueRemoveFix(ctx, normalizeHome(dir), normalizeHome(root), normalizeHome(ctx.opts.ConfigDir),
		testresidue.TmuxSocketDir)

	precious := filepath.Join(dir, "someone-elses-work")
	require.NoError(t, os.WriteFile(precious, []byte("do not delete me"), 0o600))
	ageTree(t, precious, dir)

	err = fix()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no longer holds only what the test harness leaves behind",
		"refused for the content, not for some other reason")
	require.FileExists(t, precious)
}

// And it re-reads who has the log open: a process that opened it after the scan
// vetoes the removal.
func TestTheResidueFixRefusesALogOpenedSinceTheScan(t *testing.T) {
	root := socketTempHome(t)
	dir := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9200", "agent-factory.log")
	reader := spawnWithEnv(t, "late-reader", nil, nil)
	opts := residueOptions(t, root, true, reader.PID)

	stubOpenFiles(t, true, nil)
	ctx, err := newScanContext(opts)
	require.NoError(t, err)
	fix := testResidueRemoveFix(ctx, normalizeHome(dir), normalizeHome(root), normalizeHome(ctx.opts.ConfigDir),
		testresidue.SandboxHome)
	stubOpenFiles(t, true, map[int][]string{reader.PID: {normalizeHome(filepath.Join(dir, "agent-factory.log"))}})

	err = fix()
	require.Error(t, err)
	require.Contains(t, err.Error(), "has a file open inside it")
	require.FileExists(t, filepath.Join(dir, "agent-factory.log"))
}

// Where open files cannot be seen, a SandboxHome cannot be cleared — a live test
// binary writing its log is exactly what that view would show — but a tmux
// socket dir, which has nothing in it to lose, still can.
func TestWithoutAnOpenFileViewSandboxHomesAreReportedNotRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, false, nil)
	home := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9300", "agent-factory.log")
	tmuxDir, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"9301", "dead")

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)

	f := residueFinding(t, report, home)
	require.False(t, f.Actionable)
	require.Empty(t, f.FixAction)
	require.Contains(t, f.Detail, "cannot see which files processes hold open")
	require.DirExists(t, home)

	require.True(t, residueFinding(t, report, tmuxDir).Fixed)
	require.NoDirExists(t, tmuxDir)
}

// Blind is not the same thing: an unreadable process table gives no view of open
// files either, so the same rule applies.
func TestABlindProcessTableKeepsSandboxHomes(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	home := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9400", "agent-factory.log")

	report, err := Run(macLikeTempHomeOptions(t, root, true)) // snapshot fails
	require.NoError(t, err)
	require.False(t, residueFinding(t, report, home).Actionable)
	require.DirExists(t, home)
}

// The point of reading the ROOT listing: a candidate budget spent on somebody
// else's nested directories cannot hide the harness's first-level ones, and it
// does not make this row a lower bound either.
func TestTheCandidateBudgetCannotHideHarnessResidue(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	decoy := filepath.Join(root, "0-sorts-first")
	for i := 0; i < 10; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(decoy, fmt.Sprintf("child%02d", i)), 0o755))
	}
	dir, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"9500", "empty")
	opts := residueOptions(t, root, false)
	opts.MaxTempHomeCandidates = 2

	report, err := Run(opts)
	require.NoError(t, err)

	require.Contains(t, report.Incomplete, "stale-temp-home", "precondition: the budget did fire")
	require.NotContains(t, report.Incomplete, checkTestResidueDir)
	require.True(t, residueFinding(t, report, dir).Actionable)
	require.NotContains(t, renderedRow(t, report, "test-residue-dirs"), "at least")
}

// A root listing that was cut short IS a lower bound for this check, and the row
// has to say so itself.
func TestAPartialRootListingMakesTheResidueRowALowerBound(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	for i := 0; i < 40; i++ {
		tmuxResidue(t, root, fmt.Sprintf("%s%d", testresidue.TestTmuxPrefix, 10000+i), "empty")
	}
	withSmallReadBudget(t, 8)

	report, err := Run(residueOptions(t, root, false))
	require.NoError(t, err)

	require.Contains(t, report.Incomplete, checkTestResidueDir)
	found := len(findByCheck(report, checkTestResidueDir))
	require.Greater(t, found, 0, "precondition: the part that was read is still assessed")
	require.Less(t, found, 40, "precondition: the root really was cut short")
	row := renderedRow(t, report, "test-residue-dirs")
	require.True(t, strings.HasPrefix(row, fmt.Sprintf("at least %d directories", found)), row)
	require.Contains(t, row, "lower bound")
}

func TestTheResidueBoundIsReportedNotSilent(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	for i := 0; i < 4; i++ {
		tmuxResidue(t, root, fmt.Sprintf("%s%d", testresidue.PackageTmuxPrefix, 20000+i), "empty")
	}
	prev := maxTestResidueDirs
	maxTestResidueDirs = 2
	t.Cleanup(func() { maxTestResidueDirs = prev })

	report, err := Run(residueOptions(t, root, false))
	require.NoError(t, err)

	require.Len(t, findByCheck(report, checkTestResidueDir), 2)
	require.Contains(t, report.Incomplete, checkTestResidueDir)
	require.Len(t, findByCheck(report, "test-residue-scan"), 1)
}

func TestAnUnreadableTmuxListStandsTheResidueCheckDown(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	home := sandboxHomeResidue(t, root, testresidue.SandboxHomePrefix+"9600", "agent-factory.log")
	opts := residueOptions(t, root, true)
	opts.Exec = blindTmuxLsExec(t, "error connecting to /tmp/tmux-1000/default (Permission denied)", 1)

	report, err := Run(opts)
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, checkTestResidueDir))
	require.Len(t, findByCheck(report, "test-residue-scan"), 1)
	require.Contains(t, report.Incomplete, checkTestResidueDir)
	require.DirExists(t, home)
}

// #4170's second ask, for every collapsed row: a count from a check that did not
// finish is a lower bound, and the ROW says so — not only a separate warning and
// the summary line.
func TestACollapsedRowFromAnIncompleteCheckSaysItIsALowerBound(t *testing.T) {
	findings := []Finding{
		{Check: "stale-temp-home", Detail: "a", Severity: StatusWarn},
		{Check: "stale-temp-home", Detail: "b", Severity: StatusWarn},
		{Check: checkDeadSocketHome, Detail: "c", Severity: StatusWarn, Actionable: true, FixAction: "remove c"},
	}
	partial := &Report{Findings: findings, Incomplete: []string{"stale-temp-home", checkDeadSocketHome}}
	complete := &Report{Findings: findings}

	for name, want := range map[string]string{
		"stale-temp-homes":  "at least 2 possibly abandoned agent-factory homes",
		"dead-socket-homes": "at least 1 directory under the temp dir",
	} {
		row := renderedRow(t, partial, name)
		require.True(t, strings.HasPrefix(row, want), row)
		require.Contains(t, row, "a lower bound: this run did not finish scanning the temp dir")
		require.NotContains(t, renderedRow(t, complete, name), "at least",
			"a finished scan's count is exact, and hedging it understates what doctor knows")
	}

	var out bytes.Buffer
	require.NoError(t, RenderJSON(&out, partial, false, false))
	require.Contains(t, out.String(), "at least 2 possibly abandoned", "JSON renders the same rows")
}

// The listing only ever yields real directories, but a name can be swapped for a
// symlink between the listing and the assessment. Resolved, it no longer sits
// directly in the temp dir, and that — not the listing — is what keeps doctor
// from judging, or removing, whatever it now points at.
func TestAHarnessNameSwappedForASymlinkAfterTheListingIsNotJudged(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	elsewhere := socketTempHome(t)
	target, _ := tmuxResidue(t, elsewhere, testresidue.TestTmuxPrefix+"9700", "empty")
	name := testresidue.TestTmuxPrefix + "9701"
	require.NoError(t, os.Symlink(target, filepath.Join(root, name)))

	ctx, err := newScanContext(residueOptions(t, root, true))
	require.NoError(t, err)
	ctx.snap = map[int]proctree.Process{}
	ctx.tempSweep, ctx.tempSweepDone = tempHomeSweep{roots: []string{name}, offered: 1, visited: 1}, true
	report := &Report{}
	checkTestResidue(ctx, report)
	require.Empty(t, report.Findings)

	fix := testResidueRemoveFix(ctx, normalizeHome(filepath.Join(root, name)), normalizeHome(root),
		normalizeHome(ctx.opts.ConfigDir), testresidue.TmuxSocketDir)
	require.Error(t, fix())
	require.DirExists(t, target)
}

// #4496, reproduced where CI cannot see it. The macOS runner's /tmp is a
// symlink; Linux's is not, so a test that resolved its paths only after --fix
// removed them passed here and failed there. This stages the macOS shape on
// every platform: doctor is handed the temp dir through a symlink, as
// os.TempDir() hands it out on macOS, and the fixture paths are named from the
// root resolved at creation.
func TestResidueUnderASymlinkedTempDirIsMatchedAfterItIsRemoved(t *testing.T) {
	alias, canonical := aliasedSocketTempHome(t)
	require.NotEqual(t, alias, canonical, "precondition: the temp dir is reached through a symlink")
	stubOpenFiles(t, true, nil)
	home := sandboxHomeResidue(t, canonical, testresidue.SandboxHomePrefix+"9800", "agent-factory.log")
	tmuxDir, _ := tmuxResidue(t, canonical, testresidue.PackageTmuxPrefix+"9801", "dead")

	report, err := Run(residueOptions(t, alias, true))
	require.NoError(t, err)

	for _, dir := range []string{home, tmuxDir} {
		require.NoDirExists(t, dir)
		f := residueFinding(t, report, dir)
		require.True(t, f.Fixed, "fix error for %s: %v", dir, f.FixErr)

		// The hazard, stated: the same directory spelled through the symlink can
		// no longer be resolved, so resolving at assert time yields a path doctor
		// never reported.
		viaAlias := filepath.Join(alias, filepath.Base(dir))
		require.NotEqual(t, dir, normalizeHome(viaAlias),
			"resolution needs the path to exist; a removed one comes back unresolved")
	}
}
