// Package dockerrootverify is the in-container half of the docker backend's
// root-exec verification (#7577 item 3). The daemon execs the staged af binary
// as `af __docker-root-verify v1 <checks>` inside the session container, and
// this process judges — against the container's own /proc/self/mountinfo, read
// in-process with no shell — one property: every program af will run as root,
// resolved through the container's real symlinks with all mounts applied,
// walks to a path whose every component — and every symlink hop's target —
// sits on a mount whose covering mount is the root filesystem. A
// docker.run_args mount can place a repo-chosen program at
// any of those paths (a bind at /bin/cat lands on /bin/busybox and supplies
// every BusyBox applet at once), but it cannot hide from the kernel's mount
// table, and it cannot substitute this verifier, which arrives on af's own
// staged bind or a mount-checked cp.
//
// The argv contract is v1, tagged values only, so a field can never be read
// past its tag:
//
//	size=<n>     os.Executable(), resolved, must be a regular file of n
//	             bytes — the staged binary's identity.
//	file=<path>  <path> must be a readable regular file — the staged policy.
//	prog=<spec>  <spec> is an absolute exec target, or a bare name found on
//	             the pinned PATH the root scripts use; either way the path is
//	             walked component by component and every materialized
//	             component — and every symlink hop's target — must sit on a
//	             mount whose covering mount is "/". A program absent
//	             everywhere reports ABSENT, not FAIL — a missing program is
//	             the caller's own pre-existing error path, and nothing this
//	             check is asked to keep off a mount.
//	write=<path> a destination af is about to `docker cp` into: the leaf
//	             must not be a symlink (cp resolves it wherever it points),
//	             the same component walk applies to the destination, and no
//	             mount may sit beneath it, where a cp writing through a
//	             directory destination would land. An absent tail is
//	             allowed — it is created beneath the deepest verified
//	             ancestor.
//
// Each check prints one verdict line to stdout — `OK`, `ABSENT`, or `FAIL`
// followed by the detail — so the daemon can name the covering mount in its
// refusal. The exit is 1 when any FAIL printed, 0 otherwise, 64 on an argv
// this binary does not speak.
package dockerrootverify

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// Arg is the internal subcommand, consumed in main before Cobra like the
	// other internal exec hooks — it never reaches `af --help`.
	Arg = "__docker-root-verify"
	// Protocol is the only argv contract this binary speaks.
	Protocol = "v1"

	// ExitUsage is sysexits(3) EX_USAGE: the daemon passed an argv this
	// binary does not speak.
	ExitUsage = 64
	// ExitFail marks at least one FAIL verdict.
	ExitFail = 1
)

// PinnedPathDirs is the PATH af pins inside every root script it sends the
// container (`export PATH=…` at the head of each). Bare program names the
// checks are given resolve against exactly these directories, in order, so
// the program this binary judges is the program the script will run.
var PinnedPathDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// PinnedPATH is the shell form of PinnedPathDirs, exported for the host side
// to splice into its `export PATH=` lines so the script's PATH and this
// package's search order can never drift apart.
const PinnedPATH = "/usr/sbin:/usr/bin:/sbin:/bin"

// HandleExec consumes `af __docker-root-verify` invocations before Cobra, like
// watchlifeline.HandleExec and the sessionenv markers.
func HandleExec() {
	if len(os.Args) < 2 || os.Args[1] != Arg {
		return
	}
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run evaluates each tagged check in argv and prints its verdict line.
func Run(argv []string, stdout, stderr *os.File) int {
	if len(argv) < 2 || argv[0] != Arg || argv[1] != Protocol {
		fmt.Fprintf(stderr, "af %s: want %s %s <checks>\n", Arg, Arg, Protocol)
		return ExitUsage
	}
	points, err := mountPoints("/proc/self/mountinfo")
	if err != nil {
		// Fail closed as a verdict, not a silent exit: the daemon reads FAIL
		// lines as a refusal, and an unreadable mount table proves nothing.
		fmt.Fprintf(stdout, "FAIL mountinfo: %v\n", err)
		return ExitFail
	}
	failed := false
	for _, arg := range argv[2:] {
		kind, spec, ok := strings.Cut(arg, "=")
		if !ok || spec == "" {
			fmt.Fprintf(stderr, "af %s: malformed check %q\n", Arg, arg)
			return ExitUsage
		}
		var verdict string
		switch kind {
		case "size":
			verdict = checkSelfSize(spec)
		case "file":
			verdict = checkReadableFile(spec)
		case "prog":
			verdict = checkProgram(spec, points)
		case "write":
			verdict = checkWriteDest(spec, points)
		default:
			fmt.Fprintf(stderr, "af %s: unknown check kind %q\n", Arg, kind)
			return ExitUsage
		}
		fmt.Fprintln(stdout, verdict)
		if strings.HasPrefix(verdict, "FAIL") {
			failed = true
		}
	}
	if failed {
		return ExitFail
	}
	return 0
}

// checkSelfSize verifies the running binary is the staged file af delivered:
// os.Executable resolves to the cp'd or bind-mounted path, and its size must
// equal the staged byte count — the same identity the delivery check used to
// measure through the image's wc.
func checkSelfSize(spec string) string {
	want, err := strconv.ParseInt(spec, 10, 64)
	if err != nil || want <= 0 {
		return "FAIL size " + spec + ": not a byte count"
	}
	exe, err := os.Executable()
	if err != nil {
		return "FAIL size: os.Executable: " + err.Error()
	}
	res, err := filepath.EvalSymlinks(exe)
	if err != nil {
		res = exe
	}
	st, err := os.Stat(res)
	if err != nil {
		return "FAIL size " + res + ": " + err.Error()
	}
	if !st.Mode().IsRegular() || st.Size() != want {
		return fmt.Sprintf("FAIL size %s: %d bytes, want %d", res, st.Size(), want)
	}
	return fmt.Sprintf("OK size %s", res)
}

// checkReadableFile requires a readable regular file — the operator policy af
// staged or cp'd, readable by the session user.
func checkReadableFile(spec string) string {
	st, err := os.Stat(spec)
	if err != nil {
		return "FAIL file " + spec + ": " + err.Error()
	}
	if !st.Mode().IsRegular() {
		return "FAIL file " + spec + ": not a regular file"
	}
	f, err := os.Open(spec)
	if err != nil {
		return "FAIL file " + spec + ": unreadable: " + err.Error()
	}
	_ = f.Close()
	return "OK file " + spec
}

// The filesystem seams the component walk reads — plain os calls in
// production, swapped for a fixture filesystem by the package's tests.
var (
	fsLstat    = os.Lstat
	fsReadlink = os.Readlink
	fsStat     = os.Stat
)

// resolveDepthCap mirrors the kernel's ELOOP ceiling: a link chain that long
// is unresolvable, never "probably rootfs".
const resolveDepthCap = 40

// checkProgram resolves one program af runs as root — an absolute exec target
// or a bare name the pinned-PATH scripts use — and applies the one property
// this verification exists to state: every component on the resolved path,
// and every symlink hop's target, sits on a mount whose covering mount is
// "/". Anything that cannot be resolved or verified fails closed; a program
// simply not there reports ABSENT — the caller's own pre-existing error path.
func checkProgram(spec string, points []string) string {
	candidate := spec
	if !strings.HasPrefix(spec, "/") {
		found := ""
		for _, dir := range PinnedPathDirs {
			p := path.Join(dir, spec)
			if st, err := fsStat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
				found = p
				break
			}
		}
		if found == "" {
			return "ABSENT prog " + spec + ": not on the pinned PATH"
		}
		candidate = found
	}
	res, missing, err := resolveVerified(candidate, points, 0)
	switch {
	case err != nil:
		return fmt.Sprintf("FAIL prog %s: %v", spec, err)
	case missing != "":
		return fmt.Sprintf("ABSENT prog %s: %s does not exist", spec, missing)
	default:
		return fmt.Sprintf("OK prog %s -> %s", spec, res)
	}
}

// checkWriteDest judges a path af is about to `docker cp` into under the same
// property: the leaf must not be a symlink (cp resolves it wherever it
// points), every materialized component and hop target must sit on the root
// filesystem, and no mount may sit beneath the destination, where a cp
// writing through a directory would land. A missing tail is allowed — it is
// created beneath the deepest verified ancestor, which the walk has already
// proven to be rootfs.
func checkWriteDest(spec string, points []string) string {
	if st, err := fsLstat(spec); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "FAIL write " + spec + ": the destination leaf is a symlink"
	}
	res, missing, err := resolveVerified(spec, points, 0)
	switch {
	case err != nil:
		return fmt.Sprintf("FAIL write %s: %v", spec, err)
	case missing != "":
		return fmt.Sprintf("OK write %s -> %s (absent tail, created on the rootfs)", spec, res)
	}
	for _, mp := range points {
		if strings.HasPrefix(mp, res+"/") {
			return fmt.Sprintf("FAIL write %s -> %s: the mount %s sits beneath the destination", spec, res, mp)
		}
	}
	return fmt.Sprintf("OK write %s -> %s", spec, res)
}

// resolveVerified walks an absolute path component by component and applies
// the property: each materialized component's covering mount must be "/", and
// each symlink hop's target is itself walked from the root — a hop landing on
// a mount, or one that cannot be resolved at all, fails closed. It returns
// the fully resolved path; or, when a component does not exist, the deepest
// verified ancestor plus missingFrom naming the first absent materialized
// path (everything beneath it is created on that ancestor's filesystem). An
// error means the path could not be resolved or verified — the caller turns
// it into FAIL, never "assume rootfs".
func resolveVerified(p string, points []string, depth int) (resolved string, missingFrom string, err error) {
	resolved = "/"
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		cand := path.Join(resolved, c)
		if mp := enclosingMount(points, cand); mp != "/" {
			return "", "", fmt.Errorf("%s is covered by the mount %s", cand, mp)
		}
		st, lerr := fsLstat(cand)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				return resolved, cand, nil
			}
			return "", "", fmt.Errorf("cannot resolve %s: %w", cand, lerr)
		}
		if st.Mode()&os.ModeSymlink == 0 {
			resolved = cand
			continue
		}
		if depth >= resolveDepthCap {
			return "", "", fmt.Errorf("the link chain through %s exceeds %d hops", cand, resolveDepthCap)
		}
		tgt, rerr := fsReadlink(cand)
		if rerr != nil {
			return "", "", fmt.Errorf("cannot read the link %s: %w", cand, rerr)
		}
		if !path.IsAbs(tgt) {
			tgt = path.Join(path.Dir(cand), tgt)
		}
		hop, hmiss, herr := resolveVerified(tgt, points, depth+1)
		if herr != nil {
			return "", "", fmt.Errorf("the link %s -> %s: %w", cand, tgt, herr)
		}
		if hmiss != "" {
			// A dangling hop: the verified ancestor it stopped at is the
			// deepest proven component — report its first absent point.
			return hop, hmiss, nil
		}
		resolved = hop
	}
	return resolved, "", nil
}

// enclosingMount is the longest mount point equal to p or a path prefix of it
// — the same segment-boundary rule the host-side enclosingMountEntry applies.
// "/" always wins when nothing deeper covers p.
func enclosingMount(points []string, p string) string {
	best := "/"
	for _, mp := range points {
		if (mp == p || strings.HasPrefix(p, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

// mountPoints reads mountinfo and returns the decoded, cleaned mount points,
// requiring the table to be parseable and to declare a root mount — the same
// fail-closed contract the host-side parseMountEntries carries. Only the
// points are needed: the verdict asks whether the covering mount is "/",
// never which filesystem lives there.
func mountPoints(mountinfoPath string) ([]string, error) {
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return nil, err
	}
	var points []string
	sawRoot := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, fmt.Errorf("unparseable mountinfo line %q", line)
		}
		mp := unescapeMountinfo(fields[4])
		if len(mp) == 0 || mp[0] != '/' || (len(mp) > 1 && (mp[1] == '/' || mp[1] == '\\')) || strings.ContainsRune(mp, '\\') {
			return nil, fmt.Errorf("unparseable mountinfo mount point %q", fields[4])
		}
		mp = path.Clean(mp)
		if mp == "/" {
			sawRoot = true
		}
		points = append(points, mp)
	}
	if !sawRoot {
		return nil, fmt.Errorf("mountinfo has no root mount")
	}
	return points, nil
}

// unescapeMountinfo decodes the kernel's \040-style octal escapes in
// mountinfo fields — the same rule the host-side parser applies, kept local
// so this leaf package stays dependency-free.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')*64 + (s[i+2]-'0')*8 + s[i+3] - '0')
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }
