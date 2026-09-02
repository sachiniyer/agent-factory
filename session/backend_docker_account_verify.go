package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Runtime verification of the account boundary (#3598).
//
// validateAccountDockerRunArgs reads docker.run_args as STRINGS, and a lexical
// check cannot see a symlink. An image the repository also selects can carry
// `/device-target -> /af-account`, and Docker resolves it when it installs -v,
// --mount, --tmpfs and --device: the guard sees an unprotected container path,
// the kernel lands the mount inside af's account boundary, and the agent then
// reads repository content as the operator's credentials. An image `VOLUME`
// reaches the same place with no run_args at all.
//
// Resolving those paths BEFORE `docker run` cannot close it. af does not have
// the image's filesystem at validation time, and pulling one to inspect it is
// TOCTOU — the tag can move between the inspect and the run. The check that
// cannot be aliased is the one made against the RUNNING container, where the
// kernel has already resolved every symlink.
//
// So, after `docker run` and before anything touches the account:
//
//	docker inspect     — what Docker was CONFIGURED to install. The daemon's own
//	                     record, so it also covers an option af's argv parse
//	                     misread, and an image VOLUME that appears in no argv.
//	/proc/1/mountinfo  — what the kernel actually MOUNTED, symlinks resolved.
//	the account dir    — read on the HOST, and only when Docker recorded a
//	                     --device. A device is a mknod, so it lands in no mount
//	                     table at all; one aliased onto the account is written
//	                     through the bind, so it is visible from this side.
//
// The verdict is #3402's policy, unchanged: exactly af's own bind covers
// dockerAccountHome, and nothing else lands at or under it or
// dockerAccountRuntimeHome. That is why this ships without a config key — it is
// the same rule the lexical guard already enforces, applied to what Docker did
// rather than to what the string said, and no legitimate run_args puts anything
// under the account path for a knob to re-permit.
//
// A source that cannot be READ is a refusal, never a pass: a check that cannot
// see is not a check that passed.
//
// The residual trust, stated plainly rather than left for a reader to find: the
// mountinfo read runs a binary from the repository-selected image, so an image
// that actively forges its own /proc view can still defeat that one source. That
// is a strictly higher bar than shipping a symlink, which is the attack this
// closes, and the other two sources — the daemon's own record and af's own read
// of the account directory — cannot be forged from inside the container at all.
// Closing the forged-view case needs a host-side read of the container's mount
// namespace, which is not available on every supported engine (rootless Docker's
// container PID is not a host PID, and reading an unrelated host process's
// mountinfo would fail OPEN), so it is deliberately out of scope rather than
// half-done.
//
// One thing no runtime check can do is PREVENT what Docker did before it ran. A
// --device is a mknod the daemon performs at container start, and a bind whose
// destination does not exist yet makes its own mountpoint; both land through the
// account bind onto the host before af can look. So this refuses and names the
// residue rather than promising it never happened. #3595's lexical --device
// check is what stops the non-aliased case BEFORE the container exists, which is
// why this backs it up rather than replacing it.

// accountBoundaryRoots are the container paths #3402 protects: the account's own
// mount and the writable runtime HOME af creates beside it.
func accountBoundaryRoots() []string {
	return []string{dockerAccountHome, dockerAccountRuntimeHome}
}

// accountBoundaryRoot reports which protected root a container path falls at or
// under, or "" when it is outside the boundary. The same predicate the lexical
// guard draws with, applied to paths Docker reported rather than to run_args
// strings — at or under the path, never as a substring, which is what keeps
// /af-account-cache and /af-accountant accepted (#3398).
//
// #3595 hoists the identical predicate out of validateAccountDockerRunArgs as
// accountProtectedPath. Whichever of the two lands second folds this into that
// one: a boundary with two definitions is a boundary that can drift.
func accountBoundaryRoot(target string) string {
	if target == "" {
		return ""
	}
	target = path.Clean(target)
	for _, protected := range accountBoundaryRoots() {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return protected
		}
	}
	return ""
}

// dockerInspectMount is the subset of an inspected container's .Mounts entry
// this check reads. Destination is the CONFIGURED container path — Docker
// records the string it was given, not the path the kernel resolved it to,
// which is precisely why mountinfo is read as well.
type dockerInspectMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// dockerInspectDevice is one .HostConfig.Devices entry — the column #3403's
// classifier missed and #3595 taught the lexical guard to read.
type dockerInspectDevice struct {
	PathOnHost      string `json:"PathOnHost"`
	PathInContainer string `json:"PathInContainer"`
}

type dockerInspectHostConfig struct {
	Tmpfs   map[string]string     `json:"Tmpfs"`
	Devices []dockerInspectDevice `json:"Devices"`
}

type dockerInspectContainer struct {
	Mounts     []dockerInspectMount    `json:"Mounts"`
	HostConfig dockerInspectHostConfig `json:"HostConfig"`
}

// parseDockerInspectContainer reads `docker inspect`'s JSON array and insists on
// exactly one container. Anything else — malformed output, an empty array, a
// combined-output stream carrying a warning ahead of the JSON — is an
// unreadable source, and the caller turns that into a refusal.
func parseDockerInspectContainer(raw []byte) (dockerInspectContainer, error) {
	var containers []dockerInspectContainer
	if err := json.Unmarshal(raw, &containers); err != nil {
		// With the output, bounded. A JSON error alone ("invalid character 'W'")
		// leaves the operator guessing at a stream they cannot see, and the
		// likely causes — a CLI warning ahead of the document, a docker that
		// answered something else entirely — are obvious the moment it is shown.
		return dockerInspectContainer{}, fmt.Errorf("`docker inspect` output could not be read as JSON (%s): %w",
			boundedOutputExcerpt(raw), err)
	}
	if len(containers) != 1 {
		return dockerInspectContainer{}, fmt.Errorf("`docker inspect` described %d containers, want exactly 1", len(containers))
	}
	return containers[0], nil
}

// boundedOutputExcerpt renders command output for an error message without
// letting an unexpected megabyte of it become the message.
func boundedOutputExcerpt(raw []byte) string {
	const limit = 200
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "no output"
	}
	if len(trimmed) > limit {
		return fmt.Sprintf("%q…", trimmed[:limit])
	}
	return fmt.Sprintf("%q", trimmed)
}

// configuredContainerPaths lists every container path Docker recorded, other
// than af's own account mount, in a stable order.
//
// For an account-scoped session af configures exactly ONE mount — the account
// bind — because an account replaces the ambient credential mounts entirely
// (#3082). So everything this returns came from docker.run_args or from the
// image, which is what makes it the right candidate list to name when the
// kernel lands something inside the boundary that af never asked for.
func configuredContainerPaths(c dockerInspectContainer) []string {
	var paths []string
	for _, mount := range c.Mounts {
		if path.Clean(mount.Destination) == dockerAccountHome {
			continue
		}
		paths = append(paths, mount.Destination)
	}
	for target := range c.HostConfig.Tmpfs {
		paths = append(paths, target)
	}
	for _, device := range c.HostConfig.Devices {
		paths = append(paths, device.PathInContainer)
	}
	sort.Strings(paths)
	return paths
}

func quotedPathList(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	return strings.Join(quoted, ", ")
}

// verifyConfiguredAccountBoundary holds the boundary against what Docker was
// CONFIGURED to install, which is the daemon's record rather than af's reading
// of run_args. It is the backstop for an option the argv walk misparses and for
// an image VOLUME, which never appears in argv at all; the aliased cases it
// cannot see by construction are the resolved check's job.
//
// accountSource is the host directory af mounted, so this also proves the mount
// sitting at dockerAccountHome is af's own rather than something that arrived
// with the same destination. A string comparison is the right one: Docker
// records a bind source verbatim rather than canonicalising it (measured on
// 29.4.0, including a source reached through a symlinked ancestor), and af hands
// it filepath.Abs of the account directory, which does not resolve symlinks
// either — so the two spellings are the same spelling.
func verifyConfiguredAccountBoundary(c dockerInspectContainer, accountSource string) error {
	own := 0
	for _, mount := range c.Mounts {
		destination := path.Clean(mount.Destination)
		if destination == dockerAccountHome {
			if mount.Type != "bind" || path.Clean(mount.Source) != path.Clean(accountSource) {
				return fmt.Errorf(
					"Docker recorded a %q mount from %q at %s, but af mounted the account from %q; refusing rather than running the session on a directory af did not select",
					mount.Type, mount.Source, dockerAccountHome, accountSource)
			}
			own++
			continue
		}
		if root := accountBoundaryRoot(destination); root != "" {
			return configuredBoundaryRefusal("a "+mount.Type+" mount", mount.Destination, root)
		}
	}
	tmpfsTargets := make([]string, 0, len(c.HostConfig.Tmpfs))
	for target := range c.HostConfig.Tmpfs {
		tmpfsTargets = append(tmpfsTargets, target)
	}
	sort.Strings(tmpfsTargets)
	for _, target := range tmpfsTargets {
		if root := accountBoundaryRoot(target); root != "" {
			return configuredBoundaryRefusal("a tmpfs", target, root)
		}
	}
	for _, device := range c.HostConfig.Devices {
		if root := accountBoundaryRoot(device.PathInContainer); root != "" {
			return configuredBoundaryRefusal("a device node", device.PathInContainer, root)
		}
	}
	if own != 1 {
		return fmt.Errorf(
			"Docker recorded %d mounts at %s where af configured exactly one (the account); refusing rather than running the session on a boundary af cannot account for",
			own, dockerAccountHome)
	}
	return nil
}

func configuredBoundaryRefusal(what, target, root string) error {
	return fmt.Errorf(
		"Docker was configured to install %s at %q, inside af's account boundary at %s; nothing but af's own account mount may land there",
		what, target, root)
}

// parseMountinfoTargets reads the mount points out of a /proc/<pid>/mountinfo
// stream — the kernel's own record of what is mounted where, with every symlink
// in the destination already resolved.
//
// Field 5 is the mount point, and the kernel octal-escapes space, tab, newline
// and backslash in it. A malformed line is an error rather than a skip: this is
// a credential boundary, and a line af cannot read is a line that could be
// hiding the mount it is looking for.
func parseMountinfoTargets(raw []byte) ([]string, error) {
	var targets []string
	for number, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, " ")
		// Six mandatory fields, then any number of optional ones, then the "-"
		// separator that ends them. Requiring the separator is what proves the
		// line has mountinfo's shape rather than merely enough words in it.
		if len(fields) < 7 {
			return nil, fmt.Errorf("mountinfo line %d has %d fields, want at least 7: %q", number+1, len(fields), line)
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 0 {
			return nil, fmt.Errorf("mountinfo line %d has no %q separator: %q", number+1, "-", line)
		}
		target, err := unescapeMountinfoField(fields[4])
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d: %w", number+1, err)
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("mountinfo listed no mounts at all, which no running container can be")
	}
	return targets, nil
}

// unescapeMountinfoField undoes the kernel's octal escaping of the mountinfo
// path fields (\040 space, \011 tab, \012 newline, \134 backslash).
func unescapeMountinfoField(field string) (string, error) {
	if !strings.Contains(field, `\`) {
		return field, nil
	}
	var out strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] != '\\' {
			out.WriteByte(field[index])
			continue
		}
		if index+3 >= len(field) {
			return "", fmt.Errorf("truncated octal escape in %q", field)
		}
		var value int
		for _, digit := range []byte(field[index+1 : index+4]) {
			if digit < '0' || digit > '7' {
				return "", fmt.Errorf("invalid octal escape in %q", field)
			}
			value = value*8 + int(digit-'0')
		}
		// Three octal digits reach 0777, which does not fit a byte. The kernel
		// emits only \040, \011, \012 and \134, so anything above 0377 is not
		// something it wrote — and silently truncating it would invent a path
		// character the source never had.
		if value > 0xff {
			return "", fmt.Errorf("octal escape out of range in %q", field)
		}
		out.WriteByte(byte(value))
		index += 3
	}
	return out.String(), nil
}

// verifyResolvedAccountBoundary holds the boundary against what the KERNEL
// mounted. This is the half a symlink cannot survive: whatever the run_args or
// the image spelled, mountinfo names the path the mount actually landed on.
//
// configured is the container paths Docker recorded other than af's own account
// mount. They reach the refusal as CANDIDATES — one of them may be what the
// image aliased onto the account, and af cannot say which, or whether it was any
// of them; see resolvedMountCauses.
func verifyResolvedAccountBoundary(targets, configured []string) error {
	atAccountHome := 0
	for _, target := range targets {
		if target == dockerAccountHome {
			atAccountHome++
			continue
		}
		if root := accountBoundaryRoot(target); root != "" {
			return boundaryRefusal(fmt.Sprintf(
				"the kernel mounted %q, inside af's account boundary at %s, which af did not configure",
				target, root), resolvedMountCauses(configured), "")
		}
	}
	if atAccountHome != 1 {
		return boundaryRefusal(fmt.Sprintf(
			"the kernel mounted %d filesystems at %s where af configured exactly one — its own account bind",
			atAccountHome, dockerAccountHome), resolvedMountCauses(configured), "")
	}
	return nil
}

// resolvedMountCauses lists what could put a filesystem inside the boundary that
// af did not configure. BOTH are offered whenever both are possible, and that is
// the point: a candidate list is not causation.
//
// An earlier cut branched on whether Docker recorded any other container path
// and, when it had, told the operator flat out that the image aliased one of
// them. That is wrong whenever the real cause is the other one — the
// clone-source bind every docker session already carries is enough to make the
// list non-empty — and it sends them to remove a valid run_arg, after which the
// session is still refused because the host-side mount is still there (#3602
// review).
//
// af cannot correlate the two from what it has: mountinfo names where a mount
// landed, not which configured entry resolved onto it, and for a tmpfs or a
// same-filesystem bind there is nothing in the line to match against. So it says
// what it observed and what could explain it, and leaves the diagnosis to the
// operator, who can see both sides.
func resolvedMountCauses(configured []string) []string {
	var causes []string
	if len(configured) > 0 {
		causes = append(causes, fmt.Sprintf(
			"the selected image may alias one of the container paths Docker was also asked to install (%s) onto the account — remove that entry from docker.run_args, or select an image that has no symlink at that path",
			quotedPathList(configured)))
	}
	causes = append(causes,
		"the account directory on this host may itself contain a nested mount point, which the account's recursive bind carries into the container — move it out of the account directory")
	return causes
}

// verifyAccountDeviceResidue looks for device nodes af never put in the account
// directory, reading it HERE on the host rather than inside the container.
//
// A --device lands by mknod rather than by mount, so it is invisible to
// mountinfo and needs its own look — and one aliased onto the account is written
// straight through the bind, which means the host side can see it. Reading from
// this side costs the image nothing: no `find`, no tool the documented image
// contract does not already require, and nothing the container could forge.
//
// The account directory arrives as an fs.FS, which production fills with
// os.DirFS: no unprivileged process can mknod a character device, so that is
// what lets a test present a real device MODE to the walk that production runs.
//
// It runs ONLY when `docker inspect` recorded at least one --device, and that
// gate is a proof rather than an optimisation: every device node Docker creates
// comes from .HostConfig.Devices, so a container configured with none created
// none, and walking an account home — which internal/sessionenv defines as the
// agent's WHOLE home, history included — on every provision would be work with
// no question behind it (#3602 review).
//
// What reading the host side gives up, so it is not discovered later: an aliased
// --device under dockerAccountRuntimeHome is NOT detected. af creates that
// directory inside the container, so a node there is written into the
// container's own filesystem, leaves nothing on the host, and dies with the
// container — while verifyConfiguredAccountBoundary still refuses a
// non-aliased one, because Docker recorded the path. What is given up is
// container-local nuisance, never the credential substitution this file exists
// to stop.
func verifyAccountDeviceResidue(accountFS fs.FS, accountSource string, devices []dockerInspectDevice, configured []string) error {
	if len(devices) == 0 {
		return nil
	}
	found, err := accountDeviceResidue(accountFS)
	if err != nil {
		return fmt.Errorf(
			"cannot read account directory %q to establish that docker.run_args planted no device node in it: %w",
			accountSource, err)
	}
	if len(found) == 0 {
		return nil
	}
	inContainer := make([]string, 0, len(found))
	for _, node := range found {
		inContainer = append(inContainer, path.Join(dockerAccountHome, node))
	}
	return boundaryRefusal(fmt.Sprintf(
		"the account directory holds the device node(s) %s, inside af's account boundary, which af did not put there",
		quotedPathList(inContainer)), deviceResidueCauses(configured), deviceResidueNote(found, accountSource))
}

// accountDeviceResidue walks an account directory and returns the paths of every
// character or block device in it, relative to its root.
//
// An fs.FS rather than a path so a test can present a real device MODE, which no
// unprivileged process can mknod. Symlinks are never followed — fs.WalkDir reads
// directory entries, so a symlink is an entry rather than a door, and Info()
// describes the link itself.
//
// It classifies on Info() rather than on DirEntry.Type(), which is the stricter
// of the two for a reason worth recording. Type() is allowed to be the zero
// FileMode when a directory read cannot say what an entry is, and a zero read as
// "not a device" is a fail-OPEN: the node is there and the walk reports a clean
// account. That cannot happen through os.DirFS today — Go lstats an entry whose
// d_type is DT_UNKNOWN before handing it back (os/file_unix.go, newUnixDirent,
// go1.25) — but this walks whatever fs.FS it is given, and a credential boundary
// should not rest on another package's implementation detail (#3602 review).
//
// An entry that has VANISHED between the read and the stat is skipped rather
// than refused: an account is shared by every session using it, so another
// session's agent deleting a file mid-walk is ordinary, and a file that is gone
// is not residue. The ROOT is never skipped that way — see below.
func accountDeviceResidue(fsys fs.FS) ([]string, error) {
	var found []string
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		// The root is never "vanished". An account directory that is not there
		// is a directory af could not READ, which is a refusal; swallowing its
		// ErrNotExist the way an entry's is swallowed would turn "af cannot see
		// the account" into "the account is clean" — the exact fail-open shape
		// this file exists to refuse. Held by
		// TestAccountRuntimeVerify_UnreadableSourceRefuses, which caught it.
		vanished := func(err error) bool {
			return name != "." && errors.Is(err, fs.ErrNotExist)
		}
		if err != nil {
			if vanished(err) {
				return nil
			}
			return err
		}
		info, err := entry.Info()
		if err != nil {
			if vanished(err) {
				return nil
			}
			return err
		}
		if info.Mode()&fs.ModeDevice != 0 {
			found = append(found, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// deviceResidueNote names where a planted node ended up ON THE HOST.
//
// The mknod happens as root at container start, so by the time any runtime check
// can look the node already exists, and it outlives the container this refusal
// tears down. af does not delete it: removing files from the operator's
// credential directory on a repository's say-so is not a thing this boundary
// should do. It names the path instead, and travels as the refusal's closing
// NOTE rather than as one of its candidate causes — it is the remedy either
// cause needs, not a claim about which one it was.
func deviceResidueNote(found []string, accountSource string) string {
	residue := make([]string, 0, len(found))
	for _, node := range found {
		residue = append(residue, path.Join(accountSource, node))
	}
	return fmt.Sprintf(
		"Either way the daemon created it as ROOT through the account bind, so it is on this host at %s and outlives the container af is about to reap; remove it there by hand",
		quotedPathList(residue))
}

// deviceResidueCauses lists what could have put a device node in the account
// directory. As with resolvedMountCauses, both are offered rather than one
// asserted: the gate proves a node can only come from a --device, but not that
// it came from THIS session's, and one an earlier session planted is still
// sitting there when the next one starts.
func deviceResidueCauses(configured []string) []string {
	var causes []string
	if len(configured) > 0 {
		causes = append(causes, fmt.Sprintf(
			"the selected image may alias one of the container paths Docker was asked to install (%s) onto the account — remove that entry from docker.run_args, or select an image that has no symlink at that path",
			quotedPathList(configured)))
	}
	causes = append(causes,
		"an earlier session may have left it: the daemon creates a --device node at container start, before any check can run, so one outlives the refusal that finds it")
	return causes
}

// boundaryRefusal states what af OBSERVED, then lists the explanations it cannot
// tell apart — never one of them as though it were established.
//
// That distinction is the whole content of #3602's second review finding. af
// sees a mount or a node inside the boundary; it does not see which configured
// entry produced it, and picking the likeliest-sounding one hands the operator a
// remedy that may leave the session refused for the reason it did not name.
func boundaryRefusal(observed string, causes []string, note string) error {
	var message strings.Builder
	message.WriteString("af's account boundary does not hold — ")
	message.WriteString(observed)
	message.WriteString(". af configured only its own account mount there")
	switch len(causes) {
	case 0:
		message.WriteString(", and will not run the session on a boundary it cannot account for")
	case 1:
		message.WriteString(", and ")
		message.WriteString(causes[0])
		message.WriteString(". af will not run the session on a boundary it cannot account for")
	default:
		message.WriteString(". Either ")
		message.WriteString(strings.Join(causes[:len(causes)-1], "; or "))
		message.WriteString("; or ")
		message.WriteString(causes[len(causes)-1])
		message.WriteString(". af cannot tell these apart from here, and will not run the session on a boundary it cannot account for")
	}
	if note != "" {
		message.WriteString(". ")
		message.WriteString(note)
	}
	return errors.New(message.String())
}

// verifyAccountRuntimeBoundary is the whole check, run against the started
// container before anything uses the account. Every failure — including a source
// af could not read — returns an error, and the caller reaps the container on
// the way out, so a refusal tears the session down rather than starting it with
// a shadowed account.
func (p *dockerProvisioner) verifyAccountRuntimeBoundary() error {
	if p.spec.Account.Dir == "" {
		return nil
	}
	refuse := func(err error) error {
		return fmt.Errorf("backend=docker: refusing account %q for session %q: %w",
			p.spec.Account.Name, p.spec.Title, err)
	}
	out, err := p.docker(dockerShortStepTimeout, "inspect", "--type", "container", p.containerID)
	if err != nil {
		return refuse(fmt.Errorf("cannot read the container's configured mounts: %s: %w", strings.TrimSpace(string(out)), err))
	}
	inspected, err := parseDockerInspectContainer(out)
	if err != nil {
		return refuse(fmt.Errorf("cannot read the container's configured mounts: %w", err))
	}
	source, err := p.accountBoundarySourceDir()
	if err != nil {
		return refuse(err)
	}
	if err := verifyConfiguredAccountBoundary(inspected, source); err != nil {
		return refuse(err)
	}
	configured := configuredContainerPaths(inspected)

	out, err = p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "cat", "/proc/1/mountinfo")
	if err != nil {
		return refuse(fmt.Errorf("cannot read the container's resolved mount table: %s: %w", strings.TrimSpace(string(out)), err))
	}
	targets, err := parseMountinfoTargets(out)
	if err != nil {
		return refuse(fmt.Errorf("cannot read the container's resolved mount table: %w", err))
	}
	if err := verifyResolvedAccountBoundary(targets, configured); err != nil {
		return refuse(err)
	}

	if err := verifyAccountDeviceResidue(os.DirFS(source), source, inspected.HostConfig.Devices, configured); err != nil {
		return refuse(err)
	}
	return nil
}

// accountBoundarySourceDir resolves the host directory the verification must
// find at dockerAccountHome. It calls the SAME helper accountMountAndEnv used to
// build the mount, so the check and the mount cannot drift into disagreeing
// about which directory is the account.
func (p *dockerProvisioner) accountBoundarySourceDir() (string, error) {
	source, err := accountMountSource(p.spec.Account)
	if err != nil {
		return "", fmt.Errorf("cannot resolve the account's own mount source to verify against: %w", err)
	}
	return source, nil
}
