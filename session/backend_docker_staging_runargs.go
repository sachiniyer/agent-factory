// run_args walkers the payload-delivery path shares with the docker run_args
// guards: the bind-delivery mount recorder, the -v destination reader, and the
// --volumes-from detector. Each walks docker's pflag model — a bare
// value-taking option swallows the next argv element — so a flag Docker never
// parses as a flag is neither recorded nor refused (#7726).
package session

import "strings"

// runArgsMountDestinations extracts (entry, destination) pairs for every
// mount-shaped run_args token: -v/--volume specs (the second ':'-field, or
// the only field for an anonymous volume), --mount's target/destination/dst
// key, and --tmpfs's mount point. Attached and combined shorthand spellings
// (-v/host:/dst, -tv /host:/dst) are walked with the account guard's pflag
// machinery — an ambiguous cluster is an error rather than a guess.
func runArgsMountDestinations(runArgs []string) ([][2]string, error) {
	var out [][2]string
	for i := 0; i < len(runArgs); i++ {
		a := runArgs[i]
		var spec, entry string
		var isMount, isTmpfs bool
		next := func() string {
			i++
			if i < len(runArgs) {
				return runArgs[i]
			}
			return ""
		}
		switch {
		case a == "-v" || a == "--volume":
			spec = next()
			entry = a + " " + spec
		case strings.HasPrefix(a, "-v=") || strings.HasPrefix(a, "--volume="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
		case a == "--mount":
			spec = next()
			entry = a + " " + spec
			isMount = true
		case strings.HasPrefix(a, "--mount="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
			isMount = true
		case a == "--tmpfs":
			spec = next()
			entry = a + " " + spec
			isTmpfs = true
		case strings.HasPrefix(a, "--tmpfs="):
			spec = strings.SplitN(a, "=", 2)[1]
			entry = a
			isTmpfs = true
		case strings.HasPrefix(a, "--") || (len(a) == 2 && a[0] == '-'):
			// Every other flag spelling pflag knows: a `--name` or
			// `--name=value` long option, or a bare two-character `-x`
			// shorthand. A bare value-taking option (--label, -l, or an env
			// option this path does not record) swallows the NEXT argv
			// element whatever its shape, so a following -v/--mount is its
			// value and installs no mount — the long-option swallow #7726
			// measured; re-reading it as a fresh mount records a destination
			// Docker never installs (the over-refusal direction). Unknown
			// options in a position where they could take a value, and the
			// `--` terminator, fail closed like an ambiguous cluster.
			consume, err := dockerRunOptionConsumption(a, "the bind-delivery path")
			if err != nil {
				return nil, err
			}
			if consume {
				_ = next() // the swallowed value installs no mount
			}
			continue
		case len(a) > 2 && a[0] == '-' && a[1] != '-':
			// An attached value or a combined short option — Docker's pflag
			// honors a -v inside either. Walk it with the account guard's
			// cluster machinery, recording each -v's destination; an
			// ambiguous shape fails closed rather than skipping a mount the
			// refusal check cannot see.
			if err := dockerCheckShorthandCluster(a, runArgs, i, "v",
				func(_ byte, value string) error {
					if dest := volumeSpecDest(value); dest != "" {
						out = append(out, [2]string{a, dest})
					}
					return nil
				},
				"the bind-delivery path", "a -v"); err != nil {
				return nil, err
			}
			// pflag: a cluster ending in a value-taking option consumes the
			// NEXT argv element as that option's value; skip the swallowed
			// token so the outer walk does not re-read it as a fresh mount.
			// The cluster callback above already recorded any real -v inside
			// the cluster, and an inline value (rest of cluster) consumes
			// nothing, so this advance never skips a mount the refusal needs.
			if dockerClusterConsumesNextArg(a) {
				_ = next()
			}
			continue
		default:
			continue
		}
		if spec == "" {
			continue
		}
		dest := ""
		switch {
		case isMount:
			// Docker reads --mount as one CSV record with case-folded keys —
			// the same helpers the account-boundary guard uses.
			for _, kv := range dockerMountFields(spec) {
				if v, ok := dockerMountTarget(kv); ok {
					dest = v
				}
			}
		case isTmpfs:
			dest = spec
		default:
			dest = volumeSpecDest(spec)
		}
		if dest != "" {
			out = append(out, [2]string{entry, dest})
		}
	}
	return out, nil
}

// volumeSpecDest reports the container path a -v spec mounts: the second
// ':'-field for src:dst[:opts], or the only field for an anonymous volume.
func volumeSpecDest(spec string) string {
	fields := strings.Split(spec, ":")
	if len(fields) > 1 {
		return fields[1]
	}
	return fields[0]
}

// runArgsInheritMounts reports whether run_args carries --volumes-from in
// either spelling. The single-dash form is not the option — pflag reads it as
// -v carrying the value "olumes-from" — so only the double-dash spellings
// count here. The walk consumes what pflag consumes: a --volumes-from a
// value-taking option swallowed as its value (--label --volumes-from x)
// inherits nothing and must not count (#7726).
func runArgsInheritMounts(runArgs []string) bool {
	for i := 0; i < len(runArgs); i++ {
		a := runArgs[i]
		switch {
		case a == "--volumes-from" || strings.HasPrefix(a, "--volumes-from="):
			return true
		case strings.HasPrefix(a, "--") || (len(a) == 2 && a[0] == '-'):
			if dockerRunOptionConsumesNext(a) && i+1 < len(runArgs) {
				i++
			}
		case len(a) > 2 && a[0] == '-' && a[1] != '-':
			if dockerClusterConsumesNextArg(a) && i+1 < len(runArgs) {
				i++
			}
		}
	}
	return false
}
