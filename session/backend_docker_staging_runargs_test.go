package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// runArgsMountDestinations is the recorder the bind-delivery refusal consults,
// so it must model the same pflag consumption #7726 pins on the validators: a
// token a bare value-taking option swallowed installs no mount, and an option
// af cannot model is an error rather than a skipped mount.

func TestRunArgsMountDestinations_SwallowedMountRecordsNothing(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want [][2]string
	}{
		{name: "longopt swallows -v",
			args: []string{"--label", "-v", "/host:/dst"}, want: nil},
		{name: "longopt swallows --volume",
			args: []string{"--user", "--volume", "/host:/dst"}, want: nil},
		{name: "longopt swallows --mount",
			args: []string{"--network", "--mount", "type=bind,source=/host,target=/dst"}, want: nil},
		{name: "shorthand swallows -v",
			args: []string{"-l", "-v", "/host:/dst"}, want: nil},
		{name: "attached value consumes nothing",
			args: []string{"--label=k=v", "-v", "/host:/dst"},
			want: [][2]string{{"-v /host:/dst", "/dst"}}},
		{name: "real mount after the consumed value",
			args: []string{"--label", "k", "-v", "/host:/dst"},
			want: [][2]string{{"-v /host:/dst", "/dst"}}},
		{name: "booleans consume nothing",
			args: []string{"--interactive", "-v", "/host:/dst"},
			want: [][2]string{{"-v /host:/dst", "/dst"}}},
		{name: "positional ends docker's parsing",
			args: []string{"image-name", "-v", "/host:/dst"}, want: nil},
		{name: "mount before the positional still records",
			args: []string{"-v", "/host:/dst", "image-name"},
			want: [][2]string{{"-v /host:/dst", "/dst"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runArgsMountDestinations(tt.args)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestRunArgsMountDestinations_RefusesUnknownOptions fails closed where the
// recorder cannot model the walk: an unknown option in a position where it
// could take a value, and the -- terminator.
func TestRunArgsMountDestinations_RefusesUnknownOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus-flag", "-v", "/host:/dst"},
		{"-Z", "-v", "/host:/dst"},
		{"--", "-v", "/host:/dst"},
	} {
		_, err := runArgsMountDestinations(args)
		require.Errorf(t, err, "an option af cannot model was walked past: %v", args)
	}
}

// TestRunArgsMountDestinations_TmpfsDestinationIsPreColon pins Docker's
// <dest>[:<opts>] grammar on the --tmpfs recorder: the destination is the
// substring before the first ':', so a tmpfs carrying an options suffix is
// recorded at the mount point Docker actually installs — the path the
// enclosing-mount refusal compares against. Without the split the whole
// "<dest>:<opts>" value is recorded, naming a path Docker never mounts and
// letting an enclosing tmpfs slip past the pre-flight guard.
func TestRunArgsMountDestinations_TmpfsDestinationIsPreColon(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want [][2]string
	}{
		{name: "bare tmpfs", args: []string{"--tmpfs", "/usr/local"},
			want: [][2]string{{"--tmpfs /usr/local", "/usr/local"}}},
		{name: "tmpfs with size option", args: []string{"--tmpfs", "/usr/local:size=64m"},
			want: [][2]string{{"--tmpfs /usr/local:size=64m", "/usr/local"}}},
		{name: "tmpfs with mode and size", args: []string{"--tmpfs", "/tmp:mode=1777,size=16m"},
			want: [][2]string{{"--tmpfs /tmp:mode=1777,size=16m", "/tmp"}}},
		{name: "tmpfs equals form bare", args: []string{"--tmpfs=/etc"},
			want: [][2]string{{"--tmpfs=/etc", "/etc"}}},
		{name: "tmpfs equals form with options", args: []string{"--tmpfs=/etc:rw"},
			want: [][2]string{{"--tmpfs=/etc:rw", "/etc"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runArgsMountDestinations(tt.args)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestRunArgsInheritMounts_SwallowedFlagIsNotInherited pins the same model on
// the --volumes-from probe: a --volumes-from that IS another option's value
// inherits nothing — docker never parses it — and must not trip the cp
// fallback.
func TestRunArgsInheritMounts_SwallowedFlagIsNotInherited(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "bare", args: []string{"--volumes-from", "donor"}, want: true},
		{name: "attached", args: []string{"--volumes-from=donor"}, want: true},
		{name: "after its own option", args: []string{"--label", "k", "--volumes-from", "donor"}, want: true},
		{name: "swallowed by a longopt", args: []string{"--label", "--volumes-from", "donor"}, want: false},
		{name: "attached spelling swallowed", args: []string{"--user", "--volumes-from=donor"}, want: false},
		{name: "swallowed by a shorthand", args: []string{"-l", "--volumes-from", "donor"}, want: false},
		{name: "swallowed by a cluster", args: []string{"-tv", "--volumes-from=donor"}, want: false},
		{name: "after a positional", args: []string{"evil/img", "--volumes-from=donor"}, want: false},
		{name: "none", args: []string{"-it"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, runArgsInheritMounts(tt.args))
		})
	}
}
