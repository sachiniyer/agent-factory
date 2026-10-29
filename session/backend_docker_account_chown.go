package session

import (
	"fmt"
	"path"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// The recursive HOME chown must never cross into a mount point under HOME,
// however the mount was created (docker.run_args -v/--mount/--volumes-from/
// --tmpfs, an image-declared VOLUME, the af-managed credential binds): a
// recursive chown through a host bind re-owns the operator's host files, and
// through a read-only one aborts provisioning with EROFS (#5424 review).
// Deriving "what is a mount" by parsing run_args kept missing spellings, so the
// set is read from the container's own kernel mount table at chown time
// instead (parseMountPoints), and provisioning fails closed when that table
// cannot be read or trusted.

// unescapeMountinfo decodes the octal escapes (\040 for a space, \011, \012,
// \134) the kernel uses in mountinfo path fields.
func unescapeMountinfo(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+4 <= len(field) {
			if v, ok := octalByte(field[i+1 : i+4]); ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

func octalByte(s string) (byte, bool) {
	if len(s) != 3 {
		return 0, false
	}
	var v int
	for _, c := range []byte(s) {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int(c-'0')
	}
	if v > 255 {
		return 0, false
	}
	return byte(v), true
}

// parseMountPoints parses /proc/self/mountinfo text into the cleaned, de-duplicated
// list of mount points. It errors — callers must then refuse to chown — when the
// table is unusable: a line without a mount-point field, or no "/" mount at all
// (a real container's table always has one, so its absence means the text is not
// a mount table). An empty table is therefore an error too.
func parseMountPoints(mountinfo string) ([]string, error) {
	var points []string
	sawRoot := false
	seen := map[string]bool{}
	for _, line := range strings.Split(mountinfo, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, fmt.Errorf("unparseable mountinfo line %q", line)
		}
		mp := path.Clean(unescapeMountinfo(fields[4]))
		if !strings.HasPrefix(mp, "/") {
			return nil, fmt.Errorf("unparseable mountinfo mount point %q", fields[4])
		}
		if mp == "/" {
			sawRoot = true
		}
		if !seen[mp] {
			seen[mp] = true
			points = append(points, mp)
		}
	}
	if !sawRoot {
		return nil, fmt.Errorf("mountinfo has no root mount")
	}
	return points, nil
}

// mountPointsUnder returns the mount points strictly beneath root (a mount AT
// root is reported by enclosingMount instead).
func mountPointsUnder(points []string, root string) []string {
	root = path.Clean(root)
	// "/" already ends in a separator: root+"/" would be "//" and match nothing,
	// silently pruning no mount under the filesystem root.
	prefix := strings.TrimSuffix(root, "/") + "/"
	var found []string
	for _, mp := range points {
		if mp != root && strings.HasPrefix(mp, prefix) {
			found = append(found, mp)
		}
	}
	return found
}

// enclosingMount returns the mount point that contains p: the longest mount
// point equal to p or a path prefix of it. For a directory on the container's
// own filesystem that is "/". Anything else means p lives inside another mount
// — a host bind, a volume, a tmpfs — however it was created, and is not af's to
// re-own.
func enclosingMount(points []string, p string) string {
	p = path.Clean(p)
	best := "/"
	for _, mp := range points {
		if (mp == p || strings.HasPrefix(p, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

// chownRecursiveScript returns a shell snippet that chowns every entry under
// home to owner, recursively, except the bind mounts in prune — a bare
// `chown -R` would descend into one and either re-own the operator's host
// files (a read-write bind) or fail with EROFS (a read-only bind, such as the
// agent-credential mount), aborting provisioning before the agent can read its
// credential (#5424 review). With no prune targets `chown -R` is simpler and
// stays. With prune targets, `find -prune` skips each mounted path (and its
// subtree) and chowns everything else under home, including home itself, so
// root-owned image descendants such as /root/.codex or /root/.config become
// readable and writable by the selected uid. The mounted file's own
// ownership is already correct on the host, so skipping it loses nothing.
//
// The `chown` in the prune form runs with `-h` (a.k.a. --no-dereference): find
// passes each path as a command-line argument, and GNU coreutils chown
// dereferences command-line symlinks by default, so a symlink under /root (a
// non-root BYO image may ship one) would otherwise re-point the chown at the
// link's target — which may be outside home or on another host bind. `-h` makes
// chown act on the symlink itself, matching what a plain `chown -R` does for
// the entries it descends to. busybox and coreutils both implement `-h`
// (#5424 review).
func chownRecursiveScript(home, owner string, prune []string) string {
	if len(prune) == 0 {
		return fmt.Sprintf("chown -R %s %s", owner, shellQuote(home))
	}
	var b strings.Builder
	b.WriteString("find ")
	b.WriteString(shellQuote(home))
	b.WriteString(" \\( ")
	for i, p := range prune {
		if i > 0 {
			b.WriteString(" -o ")
		}
		b.WriteString("-path ")
		b.WriteString(shellQuote(findLiteralPattern(path.Clean(p))))
	}
	b.WriteString(" \\) -prune -o -exec chown -h ")
	b.WriteString(owner)
	b.WriteString(" {} +")
	return b.String()
}

// findLiteralPattern escapes the shell-pattern metacharacters of a path so
// find's -path matches it literally: -path takes a pattern, not a pathname, so a
// destination such as /root/cache[1] would otherwise match /root/cache1 and miss
// the real mount (#5424 review). Shell quoting is applied separately by the
// caller.
func findLiteralPattern(p string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`).Replace(p)
}

// prepareNonAccountUser mirrors prepareAccountUser for a session with no
// credential account. A BYO image may set a non-root `USER` directive (a common
// hardening practice), and the container is launched with HOME=dockerContainerHome
// (/root). Without this step every provisioning exec runs as that non-root
// default user while HOME is root-owned (mode 0700), so the first
// `git config --global` fails with "could not lock config file
// /root/.gitconfig: Permission denied" and the session is rejected at create
// time with a diagnostic that points at git, not at the forced /root HOME
// against the image's non-root USER.
//
// It probes the image's default user by running `id -u`/`id -g` with no --user
// override, so the image's USER directive resolves via the image's own
// /etc/passwd (a name like `dev`, a bare uid, or a uid:gid pair all resolve to a
// numeric uid:gid this way). It then recursively chowns HOME (a non-root BYO
// image may ship root-owned descendants such as /root/.codex or /root/.config
// with mode 0700, and a shallow chown of the top-level dir alone leaves those
// unreadable by the selected uid) and creates+chowns /workspace as root
// (0:0) to that uid. Subsequent provisioning execs (configureGit,
// cloneWorkspace) and the agent-server itself run as that user via
// sessionExecOptions, with a HOME it can now write to.
//
// A root-default image (probe reports uid 0, on either side of the gid) is the
// exception: it returns early with containerUser unset, because preparation
// would REGRESS it twice over.
// The chown is not a no-op when docker.run_args bind-mounts a host directory at
// /root or /workspace — it would re-own the operator's host files. And a set
// containerUser makes sessionExecOptions force `-e HOME=/root` on every exec,
// overriding a HOME the operator set via run_args (docker exec -e wins over the
// container's configured env). With containerUser left unset every exec runs as
// the image's default user with the container's inherited env — the exact
// behavior non-account sessions had before this step existed — and the clone
// creates /workspace itself, as it always has.
func (p *dockerProvisioner) prepareNonAccountUser() error {
	if p.spec.Account.Dir != "" {
		return nil
	}
	out, err := p.docker(dockerShortStepTimeout,
		"exec", p.containerID, "sh", "-c", `echo "$(id -u):$(id -g)"`)
	if err != nil {
		// The id probe is a read-only exec — the only data-root writes are
		// dockerd's privileged exec bookkeeping, never an unprivileged
		// container-side write — so the failure carries the daemon-write
		// class, matching prepareAccountUser's stat probe and reserving
		// dockerOpRootExec for the subsequent mkdir+chown.
		return dockerStepErr(fmt.Errorf("backend=docker: cannot identify the image's default user: %s: %w", strings.TrimSpace(string(out)), err), dockerOpDaemonWrite)
	}
	owner, err := parseContainerOwner(string(out))
	if err != nil {
		// af failed to read the engine's answer — nothing reached a writer.
		return dockerStepErr(fmt.Errorf("backend=docker: cannot identify the image's default user: %w", err), dockerOpLocal)
	}
	// UID 0 is root regardless of the gid — a `USER 0:<gid>` directive probes
	// as 0:<gid>, and uid 0 needs no writable-HOME/workspace preparation. The
	// comparison is on the uid alone so every root-default image takes the
	// no-preparation path.
	if uid, _, _ := strings.Cut(owner, ":"); uid == "0" {
		return nil
	}
	// The home is chowned recursively (-R) rather than shallowly: a non-root
	// BYO image may ship root-owned descendants under /root (e.g. /root/.codex
	// or /root/.config with mode 0700), and a shallow chown of the top-level
	// directory leaves those unreadable and unwritable by the selected uid,
	// so the agent later cannot read or update its existing configuration.
	//
	// The chown must never cross into a mount point, however it was created
	// (-v, --mount, --volumes-from, --tmpfs, an image VOLUME, af's read-only
	// credential binds): through a host bind it would re-own the operator's
	// files, through a read-only one it would abort with EROFS (#5424 review).
	// So the pruned set is the container's own kernel mount table, read as root
	// right before the chown, not a parse of docker run arguments; and if that
	// table cannot be read or trusted the chown is refused, not guessed.
	home, workspace := p.sessionHome(), dockerWorkspaceDir
	realHome, realWorkspace, mounts, err := p.readMountTable(home, workspace)
	if err != nil {
		return err
	}
	points, err := parseMountPoints(mounts)
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: refusing to chown the session HOME and workspace for the image's default user %s: the container mount table is unusable: %w", owner, err), dockerOpLocal)
	}
	// The recursive chown runs only on a resolved HOME that is a proper,
	// non-system directory. A HOME that resolves to "/" (an operator HOME=/ via
	// run_args, or a symlink to it) would otherwise re-own the whole container
	// filesystem and reach any host bind the prune missed (#5950); the refusal
	// comes before any chown is built, let alone run.
	if err := unchownableHome(realHome); err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: refusing to chown the session HOME for the image's default user %s: %w", owner, err), dockerOpLocal)
	}
	if realWorkspace == "/" {
		return dockerStepErr(fmt.Errorf("backend=docker: refusing to chown the session workspace for the image's default user %s: it resolves to /", owner), dockerOpLocal)
	}
	steps := []string{fmt.Sprintf("mkdir -p %s %s", shellQuote(home), shellQuote(workspace))}
	// Only a directory whose containing mount is the container's own root
	// filesystem is af's to re-own. One AT or BENEATH any other mount (a host
	// bind, a volume, a tmpfs — including via a symlinked HOME, which the resolved
	// path above sees through) would have the chown land on files the container
	// does not own, so it is skipped and reported, never attempted.
	if mp := enclosingMount(points, realHome); mp != "/" {
		log.WarningLog.Printf("docker session HOME %s is inside the mount %s (not the container's root filesystem); not chowning it to %s — make sure it is writable by that user", realHome, mp, owner)
	} else {
		steps = append(steps, chownRecursiveScript(realHome, owner, mountPointsUnder(points, realHome)))
	}
	if mp := enclosingMount(points, realWorkspace); mp != "/" {
		log.WarningLog.Printf("docker session workspace %s is inside the mount %s (not the container's root filesystem); not chowning it to %s — make sure it is writable by that user", realWorkspace, mp, owner)
	} else {
		steps = append(steps, fmt.Sprintf("chown %s %s", owner, shellQuote(realWorkspace)))
	}
	out, err = p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "sh", "-c", strings.Join(steps, " && "))
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: cannot prepare a writable HOME and workspace for the image's default user %s: %s: %w",
			owner, strings.TrimSpace(string(out)), err), dockerOpRootExec)
	}
	p.containerUser = owner
	return nil
}

// systemDirs are directories a session HOME must never resolve to: re-owning
// one recursively reaches the system, devices and host-visible mounts.
var systemDirs = map[string]bool{
	"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
	"/lib": true, "/lib32": true, "/lib64": true, "/media": true, "/mnt": true,
	"/opt": true, "/proc": true, "/run": true, "/sbin": true, "/srv": true,
	"/sys": true, "/tmp": true, "/usr": true, "/var": true,
}

// unchownableHome reports why the resolved (symlink-free) HOME may not be
// chowned recursively, or nil when it is a proper non-root, non-system
// directory. Mounts beneath it are pruned separately from the mount table.
func unchownableHome(realHome string) error {
	h := path.Clean(realHome)
	if h == "/" {
		return fmt.Errorf("the HOME %q resolves to the filesystem root, and a recursive chown there would re-own the whole container filesystem including any host mount — set the container's HOME to a dedicated directory (docker.run_args -e HOME=…) or use an image whose default user has a home", realHome)
	}
	if systemDirs[h] {
		return fmt.Errorf("the HOME %q resolves to the system directory %s, which is not af's to re-own — set the container's HOME to a dedicated directory (docker.run_args -e HOME=…)", realHome, h)
	}
	return nil
}

// readMountTable creates the session HOME and workspace as root, then returns
// their symlink-resolved paths and the container's own /proc/self/mountinfo, in
// one exec so both describe the same mount namespace. The exec shares the
// container's mount namespace, so the table lists every mount however it was
// made. An unreadable table is an error: callers must not chown without it.
func (p *dockerProvisioner) readMountTable(home, workspace string) (realHome, realWorkspace, mountinfo string, err error) {
	out, err := p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "sh", "-c",
		`mkdir -p -- "$1" "$2" && cd -- "$1" && pwd -P && cd -- "$2" && pwd -P && cat /proc/self/mountinfo`,
		"sh", home, workspace)
	if err != nil {
		return "", "", "", dockerStepErr(fmt.Errorf("backend=docker: cannot read the container mount table, so the session HOME is not chowned (a chown cannot be proven to stay off host mounts): %s: %w", strings.TrimSpace(string(out)), err), dockerOpRootExec)
	}
	lines := strings.SplitN(string(out), "\n", 3)
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "/") || !strings.HasPrefix(lines[1], "/") || strings.TrimSpace(lines[2]) == "" {
		return "", "", "", dockerStepErr(fmt.Errorf("backend=docker: the container mount table answer was not understood, so the session HOME is not chowned: %q", strings.TrimSpace(string(out))), dockerOpLocal)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), lines[2], nil
}
