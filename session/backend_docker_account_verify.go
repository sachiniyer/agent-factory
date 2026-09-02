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

	"github.com/sachiniyer/agent-factory/log"
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
//	/proc/self/mountinfo of the exec'd reader — what the kernel actually
//	                     MOUNTED, symlinks resolved. SELF rather than PID 1: a
//	                     run_arg may set --pid=host, and PID 1 is then the HOST's
//	                     init, whose mount table has no /af-account at all
//	                     (measured — 55 lines, zero matches, which the check would
//	                     read as "af's account mount is missing" and refuse every
//	                     such session). docker exec joins the container's MOUNT
//	                     namespace whatever its PID namespace, so the reader's own
//	                     table is always the container's. Identical to PID 1's in
//	                     the ordinary case, also measured.
//	the account dir    — read on the HOST. A device is a mknod, so it lands in no
//	                     mount table at all; one aliased onto the account is
//	                     written through the bind, so it is visible from this
//	                     side — and it outlives the session that planted it, so
//	                     this looks on every provision rather than only when this
//	                     container configured a --device.
//
// The verdict is #3402's policy, unchanged: exactly af's own bind covers
// dockerAccountHome, and nothing else lands at or under it or
// dockerAccountRuntimeHome. It is drawn with accountProtectedPath, the SAME
// predicate the lexical guard uses (#3595 hoisted it), applied to the paths
// Docker reported rather than to run_args strings — a boundary with two
// definitions is a boundary that can drift. That is why this ships without a config key — it is
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

// configuredMountPaths lists every container path Docker was asked to install a
// FILESYSTEM at, other than af's own account mount, in a stable order.
//
// For an account-scoped session af configures exactly ONE mount — the account
// bind — because an account replaces the ambient credential mounts entirely
// (#3082). So everything this returns came from docker.run_args or from the
// image, which is what makes it the candidate list to name when the kernel lands
// a filesystem inside the boundary that af never asked for.
//
// --device destinations are deliberately NOT in it, and that is the point of the
// name. A --device makes a NODE, by mknod: it installs no filesystem, appears in
// no mount table, and therefore cannot be what an aliased mount resolved through.
// Offering one as a candidate for a mount would send the operator to delete a
// valid device argument that had nothing to do with the finding (#3602 review).
// The device diagnosis gets its own list, from HostConfig.Devices, for the
// mirror-image reason: an ordinary bind cannot make a node.
func configuredMountPaths(c dockerInspectContainer) []string {
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

// accountFS opens the host account directory for the residue walk. A
// package-level seam (mirroring dockerExec and dockerSelfBinary) rather than a
// direct os.DirFS call, because no unprivileged process can mknod a character
// device — so this is what lets a test present a real device MODE to the walk
// production runs. Production never reassigns it.
var accountFS = os.DirFS

// accountSourceVerdict classifies the bind source Docker RECORDED for the mount
// at dockerAccountHome against the directory af asked it to bind.
type accountSourceVerdict int

const (
	// accountSourceSame: the same directory, by spelling or by identity.
	accountSourceSame accountSourceVerdict = iota
	// accountSourceForeign: a DIFFERENT directory that exists on this host — the
	// one case where af can say the mount is not the account.
	accountSourceForeign
	// accountSourceUnresolvable: a path this host cannot interpret, which is what a
	// daemon that translates client paths reports.
	accountSourceUnresolvable
)

// classifyAccountMountSource decides whether the mount Docker installed at
// dockerAccountHome is the account af named.
//
// A string comparison is NOT that decision, and an earlier cut of this file made
// it one. Docker records a bind source verbatim on a plain Linux daemon
// (measured on 29.4.0, including sources reached through a symlinked leaf and a
// symlinked ancestor) — but ensureAccountDockerEngineLocal deliberately accepts
// unix:// and npipe:// endpoints, which include Docker Desktop, and a daemon
// that lives in a VM can report its OWN spelling of the client's path. Refusing
// on a mismatch there would refuse every account session on that engine, which
// is a worse outcome than the one the check was guarding (#3602 review).
//
// So it refuses only what it can actually establish: a source this host resolves
// to a DIFFERENT directory. A source this host cannot resolve at all is not
// evidence of anything — af says so and lets the session continue, because the
// identity of that mount does not rest on this comparison in the first place.
// af appends the account mount itself, and Docker refuses a second mount at the
// same destination outright ("Duplicate mount point: /af-account", measured), so
// no run_arg can take that slot; what this adds is a check on the one thing
// those facts do not cover.
func classifyAccountMountSource(recorded, want string) accountSourceVerdict {
	if path.Clean(recorded) == path.Clean(want) {
		return accountSourceSame
	}
	recordedInfo, err := os.Stat(recorded)
	if err != nil {
		return accountSourceUnresolvable
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		return accountSourceUnresolvable
	}
	if os.SameFile(recordedInfo, wantInfo) {
		return accountSourceSame
	}
	return accountSourceForeign
}

// verifyConfiguredAccountBoundary holds the boundary against what Docker was
// CONFIGURED to install, which is the daemon's record rather than af's reading
// of run_args. It is the backstop for an option the argv walk misparses and for
// an image VOLUME, which never appears in argv at all; the aliased cases it
// cannot see by construction are the resolved check's job.
//
// It reports EVERY offender rather than the first, for the reason the resolved
// check and the check sequence around it do: an operator who removes the one
// entry a refusal named, only to be refused again for the next, has been given a
// worse answer than the whole list at once (#3602 review).
//
// accountSource is the host directory af mounted. warn receives one line when
// Docker reported a source this host cannot resolve — see
// classifyAccountMountSource for why that is a note rather than a refusal.
func verifyConfiguredAccountBoundary(c dockerInspectContainer, accountSource string, warn func(string, ...any)) error {
	own := 0
	var observed []string
	for _, mount := range c.Mounts {
		destination := path.Clean(mount.Destination)
		if destination == dockerAccountHome {
			own++
			if mount.Type != "bind" {
				observed = append(observed, fmt.Sprintf(
					"Docker recorded a %q mount at %s where af installed a bind of the account directory",
					mount.Type, dockerAccountHome))
				continue
			}
			switch classifyAccountMountSource(mount.Source, accountSource) {
			case accountSourceForeign:
				observed = append(observed, fmt.Sprintf(
					"Docker recorded the mount at %s as a bind of %q, which is a different directory on this host than the account af selected (%q)",
					dockerAccountHome, mount.Source, accountSource))
			case accountSourceUnresolvable:
				if warn != nil {
					warn("backend=docker: the Docker daemon reports the account mount at %s as a bind of %q, which this host cannot resolve; af asked for %q. That is what a daemon translating client paths looks like, so it is not treated as a mismatch — but if these are genuinely different directories, the session is running on the wrong one.",
						dockerAccountHome, mount.Source, accountSource)
				}
			}
			continue
		}
		if root := accountProtectedPath(destination); root != "" {
			observed = append(observed, configuredBoundaryObservation("a "+mount.Type+" mount", mount.Destination, root))
		}
	}
	tmpfsTargets := make([]string, 0, len(c.HostConfig.Tmpfs))
	for target := range c.HostConfig.Tmpfs {
		tmpfsTargets = append(tmpfsTargets, target)
	}
	sort.Strings(tmpfsTargets)
	for _, target := range tmpfsTargets {
		if root := accountProtectedPath(target); root != "" {
			observed = append(observed, configuredBoundaryObservation("a tmpfs", target, root))
		}
	}
	for _, device := range c.HostConfig.Devices {
		if root := accountProtectedPath(device.PathInContainer); root != "" {
			observed = append(observed, configuredBoundaryObservation("a device node", device.PathInContainer, root))
		}
	}
	if own != 1 {
		observed = append(observed, fmt.Sprintf(
			"Docker recorded %d mounts at %s where af configured exactly one (the account)",
			own, dockerAccountHome))
	}
	if len(observed) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s; refusing rather than running the session on a boundary af cannot account for",
		strings.Join(observed, "; and "))
}

// configuredBoundaryObservation states one thing Docker was configured to put
// inside the account boundary. A phrase rather than a whole error, because a
// container can be configured with several and the refusal names them together.
func configuredBoundaryObservation(what, target, root string) string {
	return fmt.Sprintf(
		"Docker was configured to install %s at %q, inside af's account boundary at %s, where nothing but af's own account mount may land",
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
// It reports EVERY protected target, not the first. An image can alias two
// configured destinations onto two different paths under the account, and Docker
// may have created BOTH mountpoints through the account bind before any check
// ran — so an operator handed only the first can remove exactly what the refusal
// named and still be left with a root-created file or directory in their
// credential directory (#3602 review). The same reason the three checks stopped
// returning on the first finding, one level down.
//
// configured is the container paths Docker recorded a FILESYSTEM at, other than
// af's own account mount. They reach the refusal as CANDIDATES — one of them may
// be what the image aliased onto the account, and af cannot say which, or
// whether it was any of them; see resolvedMountCauses.
func verifyResolvedAccountBoundary(targets, configured []string, accountSource string) error {
	atAccountHome := 0
	var inside, roots []string
	seenRoot := map[string]bool{}
	for _, target := range targets {
		if target == dockerAccountHome {
			atAccountHome++
			continue
		}
		root := accountProtectedPath(target)
		if root == "" {
			continue
		}
		inside = append(inside, target)
		if !seenRoot[root] {
			seenRoot[root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(inside)
	sort.Strings(roots)
	var observed []string
	if len(inside) > 0 {
		observed = append(observed, fmt.Sprintf(
			"the kernel mounted %s, inside af's account boundary at %s, which af did not configure",
			quotedPathList(inside), strings.Join(roots, " and ")))
	}
	if atAccountHome != 1 {
		observed = append(observed, fmt.Sprintf(
			"the kernel mounted %d filesystems at %s where af configured exactly one — its own account bind",
			atAccountHome, dockerAccountHome))
	}
	if len(observed) == 0 {
		return nil
	}
	return boundaryRefusal(strings.Join(observed, "; and "),
		resolvedMountCauses(configured), mountpointResidueNote(inside, accountSource))
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

// mountpointResidueNote names the HOST paths foreign mounts landed on, for those
// inside the account bind.
//
// Docker creates a destination that does not exist yet, and it does so as root,
// through the account bind, before any runtime check can run — so the directory
// or file it made is in the operator's account directory and survives the reap
// this refusal triggers. The file header has always said this refuses and NAMES
// the residue; for a mount it was naming only the container-side path, which is
// no help on a host where that path does not exist (#3602 review).
//
// It says "if it was not already yours" rather than "delete it", and that hedge
// is the honest part: Docker leaves an EXISTING destination alone, so a mount
// landing on a path the account already had — /af-account/.config, say — left
// nothing behind, and af cannot tell the two apart after the fact. Telling an
// operator to remove a path that holds their own credentials would be worse than
// saying nothing.
//
// Mounts under dockerAccountRuntimeHome earn no note: af creates that directory
// inside the container, so nothing there reaches the host.
func mountpointResidueNote(targets []string, accountSource string) string {
	if accountSource == "" {
		return ""
	}
	hosts := make([]string, 0, len(targets))
	for _, target := range targets {
		if !strings.HasPrefix(target, dockerAccountHome+"/") {
			continue
		}
		hosts = append(hosts, path.Join(accountSource, strings.TrimPrefix(target, dockerAccountHome+"/")))
	}
	if len(hosts) == 0 {
		return ""
	}
	noun := "Its mountpoint on this host is"
	if len(hosts) > 1 {
		noun = "Their mountpoints on this host are"
	}
	return fmt.Sprintf(
		"%s %s. Docker creates a destination that does not exist yet, as ROOT and through the account bind, before any check can run — so if a path there was not already yours, it is residue this refusal cannot undo; remove it by hand",
		noun, quotedPathList(hosts))
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
// accountFS: no unprivileged process can mknod a character device, so that is
// what lets a test present a real device MODE to the walk that production runs.
//
// It runs on EVERY account provision, and not only when this container
// configured a --device. An earlier cut gated it on that, reasoning that a
// container configured with no device created none — true, and beside the point.
// The node the daemon plants outlives the session that planted it: it is written
// as root at container start, before any check can run, and the refusal that
// finds it reaps a container without touching the host. So the very next
// provision — the operator's retry, which has typically had the offending
// run_arg removed and therefore configures no device at all — would have skipped
// the walk and handed the agent an account with an active device node still
// sitting in it, at /af-account/.config/settings.json for all af knows (#3602
// review).
//
// The cost that gate was buying is already paid down by reading the host side:
// this is af's own in-process walk of a local directory, with no docker exec
// round trip and no dockerShortStepTimeout over it, unlike the in-container
// `find` an earlier cut used.
//
// devices is the container paths Docker was asked to create nodes at. It shapes
// the REFUSAL rather than gating the walk, and only --device entries appear in
// it, because only a --device can make a node — naming an ordinary bind as a
// candidate would send the operator after the wrong line.
func verifyAccountDeviceResidue(fsys fs.FS, accountSource string, devices []dockerInspectDevice) error {
	found, err := accountDeviceResidue(fsys)
	if err != nil {
		return fmt.Errorf(
			"cannot read account directory %q to establish that no device node has been planted in it: %w",
			accountSource, err)
	}
	if len(found) == 0 {
		return nil
	}
	inContainer := make([]string, 0, len(found))
	for _, node := range found {
		inContainer = append(inContainer, path.Join(dockerAccountHome, node))
	}
	configuredDevices := make([]string, 0, len(devices))
	for _, device := range devices {
		configuredDevices = append(configuredDevices, device.PathInContainer)
	}
	sort.Strings(configuredDevices)
	return boundaryRefusal(fmt.Sprintf(
		"the account directory holds the device node(s) %s, inside af's account boundary, which af did not put there",
		quotedPathList(inContainer)), deviceResidueCauses(configuredDevices), deviceResidueNote(found, accountSource))
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
// directory. Both are offered rather than one asserted, as in resolvedMountCauses:
// only a --device can make a node, but never necessarily THIS session's, since
// one an earlier session planted is still sitting there when the next one starts.
//
// devices is the container paths this session asked Docker to create nodes at —
// only those, not every configured path, because an ordinary bind cannot make a
// device node and naming one as a candidate sends the operator after the wrong
// line. Empty means this session configured none, so the alias cause is not
// offered at all.
func deviceResidueCauses(devices []string) []string {
	var causes []string
	if len(devices) > 0 {
		causes = append(causes, fmt.Sprintf(
			"the selected image may alias one of the container paths this session asked Docker to create a device node at (%s) onto the account — remove that entry from docker.run_args, or select an image that has no symlink at that path",
			quotedPathList(devices)))
	}
	causes = append(causes,
		"an earlier session may have left it: the daemon creates a --device node at container start, before any check can run, so one outlives the refusal that finds it and the reap that follows")
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
//
// Once the container's own record is in hand, the remaining checks all RUN and
// their findings are joined; the first one does not return early. They report
// different things — a mount af must refuse, versus a root-created device node
// sitting in the operator's account directory that the reap does not remove —
// and an operator who is told only about the mount never learns to go delete the
// node (#3602 review). Reading a source still stops the sequence, because a
// check that could not see has nothing to add.
func (p *dockerProvisioner) verifyAccountRuntimeBoundary() error {
	if p.spec.Account.Dir == "" {
		return nil
	}
	refuse := func(errs ...error) error {
		joined := errors.Join(errs...)
		if joined == nil {
			return nil
		}
		return fmt.Errorf("backend=docker: refusing account %q for session %q: %w",
			p.spec.Account.Name, p.spec.Title, joined)
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
	configured := configuredMountPaths(inspected)
	findings := []error{verifyConfiguredAccountBoundary(inspected, source, log.WarningLog.Printf)}

	out, err = p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "cat", "/proc/self/mountinfo")
	if err != nil {
		findings = append(findings, fmt.Errorf("cannot read the container's resolved mount table: %s: %w", strings.TrimSpace(string(out)), err))
	} else if targets, perr := parseMountinfoTargets(out); perr != nil {
		findings = append(findings, fmt.Errorf("cannot read the container's resolved mount table: %w", perr))
	} else {
		findings = append(findings, verifyResolvedAccountBoundary(targets, configured, source))
	}

	findings = append(findings,
		verifyAccountDeviceResidue(accountFS(source), source, inspected.HostConfig.Devices))
	return refuse(findings...)
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
