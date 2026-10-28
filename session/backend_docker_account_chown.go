package session

import (
	"fmt"
	"path"
	"strings"
)

// credentialMountTargets returns the in-container destinations of the
// read-only agent-credential bind mounts (docker.mount_agent_credentials,
// #2194) that fall under home — the single files agentCredentialMounts
// bind-mounts at dockerContainerHome/<rel>. Each `-v` entry of mounts is the
// pair ("-v", "host:container:mode"); host and container are absolute unix
// paths with no colons, so the container destination is the segment between
// the last two colons. Only strict descendants of home are returned: the
// credential always mounts under the non-account session home, and pruning
// home itself would skip the whole chown.
func credentialMountTargets(mounts []string, home string) []string {
	var targets []string
	for i := 0; i+1 < len(mounts); i++ {
		if mounts[i] != "-v" {
			continue
		}
		spec := mounts[i+1]
		last := strings.LastIndexByte(spec, ':')
		if last < 0 {
			continue
		}
		body := spec[:last]
		sep := strings.LastIndexByte(body, ':')
		if sep < 0 {
			continue
		}
		container := body[sep+1:]
		if container == "" || home == "" {
			continue
		}
		container = path.Clean(container)
		if strings.HasPrefix(container, home+"/") {
			targets = append(targets, container)
		}
	}
	return targets
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

// volumeBindContainer reports the container destination of a -v / --volume
// value ("host:container[:mode]") when that value is a bind mount — a host
// source that is an absolute path — and ok=false for a named or anonymous
// volume. Only a bind mount lands host files at the container path, so only it
// is pruned from the recursive HOME chown: a named volume ("name:container")
// or an anonymous volume ("container") is docker-managed and writable, a
// chown into it is harmless, and the home is left to the plain recursive chown.
// The container path is the segment between the last two colons (mode last,
// container before it); host and container are absolute unix paths with no
// colons, so this split is unambiguous. An empty container or a value without
// a host:container pair (a bare anonymous-volume path) is not a bind.
func volumeBindContainer(spec string) (container string, ok bool) {
	last := strings.LastIndexByte(spec, ':')
	if last < 0 {
		return "", false // -v container, an anonymous volume: no host
	}
	body := spec[:last]
	sep := strings.LastIndexByte(body, ':')
	var host string
	if sep < 0 {
		// host:container (no mode): the only colon separates host from container.
		host = body
		container = spec[last+1:]
	} else {
		// host:container:mode: host is before sep, container between sep and last.
		host = body[:sep]
		container = body[sep+1:]
	}
	if container == "" || !volumeSourceIsHostPath(host) {
		return "", false // a named/anonymous volume, not a host-source bind
	}
	return container, true
}

// volumeSourceIsHostPath reports whether the source half of a -v spec names a
// host path rather than a volume. A volume name cannot start with "/" or ".",
// so an absolute path and — since Docker Engine 23 — a relative one ("./cache",
// "../cache", "." or "..") are both host binds.
func volumeSourceIsHostPath(source string) bool {
	return strings.HasPrefix(source, "/") || strings.HasPrefix(source, "./") ||
		strings.HasPrefix(source, "../") || source == "." || source == ".."
}

// mountBindContainer reports the container destination of a --mount value when
// that value is a bind mount ("type=bind,..."), reusing the account path's
// CSV field parsing so a quoted field such as `"dst=/root/cache"` is read the
// way Docker reads it. A --mount without type=bind (type=volume, type=tmpfs,
// or no type) is not a host bind and is not pruned: a volume is docker-managed
// and writable, and a tmpfs is in-memory, so a chown into either is harmless.
// The destination key is matched case-insensitively (Docker lowercases it), so
// DST=/root/cache mounts exactly where dst= does.
func mountBindContainer(spec string) (container string, ok bool) {
	var isBind bool
	for _, field := range dockerMountFields(spec) {
		key, val, _ := strings.Cut(field, "=")
		switch strings.ToLower(key) {
		case "type":
			isBind = strings.EqualFold(val, "bind")
		case "dst", "destination", "target":
			container = val
		}
	}
	if !isBind || container == "" {
		return "", false
	}
	return container, true
}

// bindMountTargets returns the in-container destinations of the bind mounts
// that docker.run_args installs under home, so the recursive HOME chown can
// prune them the way it prunes the af-managed credential mounts. A repo-
// controlled run_args entry can bind a host directory beneath /root
// ("-v /host/cache:/root/cache"), and a recursive chown that descends into it
// either re-owns the operator's host files (a read-write bind) or fails with
// EROFS (a read-only bind), so each must be pruned (#5424 review). Named and
// anonymous volumes are docker-managed and writable, so a chown into one is
// harmless and they are left to the plain recursive chown; only host-source
// bind mounts (-v/--volume with an absolute host, or --mount type=bind) are
// pruned. Only strict descendants of home are returned; a bind at home itself
// is not, since pruning home would skip the whole chown.
//
// The -v/--volume and --mount forms and their =/attached spellings are read
// the way the run_args validator reads them, including a -v hidden behind
// another option in a combined short option such as -tv, so a bind mounted
// that way is pruned too. --tmpfs is not a host bind and is not pruned.
func bindMountTargets(args []string, home string) []string {
	if home == "" {
		return nil
	}
	var targets []string
	add := func(container string) {
		// Docker records path.Clean(target) as the mount destination, so a
		// trailing slash or "." segment names the same directory find visits.
		container = path.Clean(container)
		if strings.HasPrefix(container, home+"/") {
			targets = append(targets, container)
		}
	}
	apply := func(kind byte, value string) {
		switch kind {
		case 'v':
			if c, ok := volumeBindContainer(value); ok {
				add(c)
			}
		case 'm':
			if c, ok := mountBindContainer(value); ok {
				add(c)
			}
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-v" || arg == "--volume":
			if i+1 < len(args) {
				i++
				apply('v', args[i])
			}
		case arg == "--mount":
			if i+1 < len(args) {
				i++
				apply('m', args[i])
			}
		case strings.HasPrefix(arg, "--volume="):
			apply('v', strings.TrimPrefix(arg, "--volume="))
		case strings.HasPrefix(arg, "--mount="):
			apply('m', strings.TrimPrefix(arg, "--mount="))
		case strings.HasPrefix(arg, "-v="):
			apply('v', strings.TrimPrefix(arg, "-v="))
		case strings.HasPrefix(arg, "-v") && len(arg) > 2:
			apply('v', strings.TrimPrefix(arg, "-v"))
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// A combined short option such as -tv /host:/root/cache, whose -v
			// mounts a host file the way -v alone does. Reuse the pflag walk the
			// run_args validator uses so a -v behind another option is still
			// read; the index deliberately does NOT advance past the value, for
			// the same reason the validator's doesn't (the value may belong to
			// an earlier option in the cluster, and a real --mount written next
			// would otherwise be skipped).
			_ = dockerCheckShorthandCluster(arg, args, i, "v",
				func(_ byte, value string) error {
					apply('v', value)
					return nil
				},
				"a non-account session", "the -v")
		}
	}
	return targets
}
