package dockerrootverify

// The fixture filesystem the walk's seams are pointed at: a flat map of
// cleaned paths to entries (dir/file/symlink), so each test can lay out
// exactly the symlinks, absent parents, and ENOTDIR shapes a container
// image plus its applied mounts would present — without real mounts.

import (
	"os"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeEnt struct {
	kind   byte // 'd' dir, 'f' file, 'l' symlink
	target string
	exec   bool
}

type fakeFS map[string]fakeEnt

type fakeInfo struct {
	name string
	mode os.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() os.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

func (fs fakeFS) infoFor(p string, e fakeEnt) os.FileInfo {
	var mode os.FileMode
	switch e.kind {
	case 'd':
		mode = os.ModeDir | 0o755
	case 'l':
		mode = os.ModeSymlink | 0o777
	default:
		mode = 0o644
		if e.exec {
			mode = 0o755
		}
	}
	return fakeInfo{name: path.Base(p), mode: mode}
}

// lstat mirrors a real filesystem's component errors: a lookup whose
// ancestor is a regular file is ENOTDIR, not ENOENT — the walk must not
// read that as an absent tail.
func (fs fakeFS) lstat(p string) (os.FileInfo, error) {
	p = path.Clean(p)
	if p == "/" {
		return fakeInfo{name: "/", mode: os.ModeDir | 0o755}, nil
	}
	if e, ok := fs[p]; ok {
		return fs.infoFor(p, e), nil
	}
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		e, ok := fs[d]
		if !ok {
			break
		}
		if e.kind == 'f' {
			return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ENOTDIR}
		}
	}
	return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
}

func (fs fakeFS) readlink(p string) (string, error) {
	p = path.Clean(p)
	if e, ok := fs[p]; ok && e.kind == 'l' {
		return e.target, nil
	}
	return "", &os.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
}

func (fs fakeFS) stat(p string) (os.FileInfo, error) {
	p = path.Clean(p)
	for i := 0; i < resolveDepthCap; i++ {
		fi, err := fs.lstat(p)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return fi, nil
		}
		tgt := fs[p].target
		if !path.IsAbs(tgt) {
			tgt = path.Join(path.Dir(p), tgt)
		}
		p = path.Clean(tgt)
	}
	return nil, &os.PathError{Op: "stat", Path: p, Err: syscall.ELOOP}
}

// installFS points the package's filesystem seams at the fixture for the
// duration of a test.
func installFS(t *testing.T, fs fakeFS) {
	t.Helper()
	oldL, oldR, oldS := fsLstat, fsReadlink, fsStat
	fsLstat, fsReadlink, fsStat = fs.lstat, fs.readlink, fs.stat
	t.Cleanup(func() { fsLstat, fsReadlink, fsStat = oldL, oldR, oldS })
}

// TestRootfsProperty is the single property this package exists to state,
// exercised once per shape a mounted container can present: every component
// on a program's resolved path — and every symlink hop's target — must sit
// on a mount whose covering mount is "/". Anything unresolvable fails
// closed.
func TestRootfsProperty(t *testing.T) {
	tests := []struct {
		name   string
		fs     fakeFS
		mounts []string
		check  func([]string) string
		want   string
	}{
		{
			name: "prog: rootfs through a usr-merge link",
			fs: fakeFS{
				"/bin":         {'l', "usr/bin", false},
				"/usr":         {'d', "", false},
				"/usr/bin":     {'d', "", false},
				"/usr/bin/cat": {'f', "", true},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("/bin/cat", m) },
			want:   "OK prog /bin/cat -> /usr/bin/cat",
		},
		{
			name: "prog: the hop's target is a mount",
			fs: fakeFS{
				"/bin":         {'l', "usr/bin", false},
				"/usr":         {'d', "", false},
				"/usr/bin":     {'d', "", false},
				"/usr/bin/cat": {'f', "", true},
			},
			mounts: []string{"/", "/usr/bin"},
			check:  func(m []string) string { return checkProgram("/bin/cat", m) },
			want:   "FAIL prog /bin/cat",
		},
		{
			name: "prog: a bare name searched on the pinned PATH",
			fs: fakeFS{
				"/usr":         {'d', "", false},
				"/usr/bin":     {'d', "", false},
				"/usr/bin/git": {'f', "", true},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("git", m) },
			want:   "OK prog git -> /usr/bin/git",
		},
		{
			name: "prog: bare name whose PATH hit sits on a mount",
			fs: fakeFS{
				"/usr":         {'d', "", false},
				"/usr/bin":     {'d', "", false},
				"/usr/bin/git": {'f', "", true},
			},
			mounts: []string{"/", "/usr/bin"},
			check:  func(m []string) string { return checkProgram("git", m) },
			want:   "FAIL prog git",
		},
		{
			name:   "prog: absent everywhere is ABSENT, not FAIL",
			fs:     fakeFS{"/usr": {'d', "", false}, "/usr/bin": {'d', "", false}},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("nonexistent-tool", m) },
			want:   "ABSENT prog nonexistent-tool",
		},
		{
			// Codex review, #7607 P1: the image links /usr/local/bin ->
			// /opt/link and /opt/link -> /safe/bin; a host dir bind-mounted
			// at /opt SHADOWS the image's /opt content, so inside the real
			// container /opt/link is the host's plain directory and the
			// cp'd af lands on the host. A pristine-image probe resolves to
			// /safe/bin/af and sees nothing; the in-container walk fails at
			// the hop's first component.
			name: "write: a mount retargets an intermediate symlink hop",
			fs: fakeFS{
				"/usr":           {'d', "", false},
				"/usr/local":     {'d', "", false},
				"/usr/local/bin": {'l', "/opt/link", false},
				"/opt":           {'d', "", false},
				"/opt/link":      {'d', "", false},
			},
			mounts: []string{"/", "/opt"},
			check:  func(m []string) string { return checkWriteDest("/usr/local/bin/af", m) },
			want:   "FAIL write /usr/local/bin/af",
		},
		{
			// Codex review, #7607 P1: an image alias lands an af write
			// inside the account bind — the verifier runs after every bind
			// is applied, so the hop onto the bind is a refusal.
			name: "write: the destination hops onto the account bind",
			fs: fakeFS{
				"/usr":               {'d', "", false},
				"/usr/local":         {'d', "", false},
				"/usr/local/bin":     {'l', "/af-account/bin", false},
				"/af-account":        {'d', "", false},
				"/af-account/bin":    {'d', "", false},
				"/af-account/bin/af": {'f', "", true},
			},
			mounts: []string{"/", "/af-account"},
			check:  func(m []string) string { return checkWriteDest("/usr/local/bin/af", m) },
			want:   "FAIL write /usr/local/bin/af",
		},
		{
			// Codex review, #7607 P1: a credential bind's destination
			// parents are absent from the image. The tail is created
			// beneath the deepest verified ancestor — allowed, and never
			// a probe failure.
			name: "write: absent parents are an allowed tail",
			fs: fakeFS{
				"/root": {'d', "", false},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkWriteDest("/root/.codex/auth.json", m) },
			want:   "OK write /root/.codex/auth.json",
		},
		{
			name:   "write: absent parents under a mounted ancestor refuse",
			fs:     fakeFS{"/root": {'d', "", false}},
			mounts: []string{"/", "/root"},
			check:  func(m []string) string { return checkWriteDest("/root/.codex/auth.json", m) },
			want:   "FAIL write /root/.codex/auth.json",
		},
		{
			name: "write: the destination leaf is a symlink",
			fs: fakeFS{
				"/etc":             {'d', "", false},
				"/etc/policy.json": {'l', "/elsewhere", false},
				"/elsewhere":       {'d', "", false},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkWriteDest("/etc/policy.json", m) },
			want:   "FAIL write /etc/policy.json",
		},
		{
			name: "write: a mount at the destination file",
			fs: fakeFS{
				"/etc":             {'d', "", false},
				"/etc/policy.json": {'f', "", false},
			},
			mounts: []string{"/", "/etc/policy.json"},
			check:  func(m []string) string { return checkWriteDest("/etc/policy.json", m) },
			want:   "FAIL write /etc/policy.json",
		},
		{
			name: "write: a mount beneath a directory destination",
			fs: fakeFS{
				"/cfg":       {'d', "", false},
				"/cfg/inner": {'d', "", false},
			},
			mounts: []string{"/", "/cfg/inner"},
			check:  func(m []string) string { return checkWriteDest("/cfg", m) },
			want:   "FAIL write /cfg",
		},
		{
			name: "write: an unresolvable component fails closed",
			fs: fakeFS{
				"/a":   {'d', "", false},
				"/a/b": {'f', "", false},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkWriteDest("/a/b/c", m) },
			want:   "FAIL write /a/b/c",
		},
		{
			name: "write: a symlink loop fails closed",
			fs: fakeFS{
				"/x": {'l', "/y", false},
				"/y": {'l', "/x", false},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkWriteDest("/x/z", m) },
			want:   "FAIL write /x/z",
		},
		{
			name: "write: a dangling hop reports its absent point",
			fs: fakeFS{
				"/etc":      {'d', "", false},
				"/etc/conf": {'l', "/missing/dir", false},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkWriteDest("/etc/conf/file", m) },
			want:   "OK write /etc/conf/file",
		},
		{
			// Codex review, #7607 P1 — kernel order, validated against
			// realpath(3) in a real container: "../victim" is resolved
			// against the link's RESOLVED directory (/x/d1 through the
			// /x/abs hop), never the lexical one.
			name: "prog: a relative hop resolves from the link's resolved dir",
			fs: fakeFS{
				"/x":        {'d', "", false},
				"/x/abs":    {'l', "/x/d1", false},
				"/x/d1":     {'d', "", false},
				"/x/d1/y":   {'l', "../victim", false},
				"/x/victim": {'f', "", true},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("/x/abs/y", m) },
			want:   "OK prog /x/abs/y -> /x/victim",
		},
		{
			// Codex review, #7607 P1 — the divergence case: ".." in a hop
			// target cancels a component that is ITSELF a symlink. Kernel
			// order lands on the link's resolved parent (/deep/b); lexical
			// cleaning would erase "/x/sub" and land on /x/b — where a
			// mount could wait unseen.
			name: "prog: .. in a hop target steps off the resolved prefix",
			fs: fakeFS{
				"/x":        {'d', "", false},
				"/x/a":      {'l', "sub/../b", false},
				"/x/sub":    {'l', "/deep/dir", false},
				"/deep":     {'d', "", false},
				"/deep/dir": {'d', "", false},
				"/deep/b":   {'f', "", true},
				"/x/b":      {'f', "", true},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("/x/a", m) },
			want:   "OK prog /x/a -> /deep/b",
		},
		{
			// .. inside the SPEC itself is walked the same way — through
			// the resolved prefix, not lexically cleaned.
			name: "prog: .. in the spec walks the resolved prefix",
			fs: fakeFS{
				"/x":      {'d', "", false},
				"/x/d1":   {'l', "/real/d", false},
				"/real":   {'d', "", false},
				"/real/d": {'d', "", false},
				"/real/v": {'f', "", true},
			},
			mounts: []string{"/"},
			check:  func(m []string) string { return checkProgram("/x/d1/../v", m) },
			want:   "OK prog /x/d1/../v -> /real/v",
		},
		{
			// The hop's TARGET transits a mount even when ".." escapes back
			// onto the rootfs: kernel order resolves /x/a -> /mnt/b, but
			// the mount at /mnt/real supplied a component of the hop and
			// the property refuses every hop component off "/".
			name: "prog: a hop target transiting a mount refuses",
			fs: fakeFS{
				"/x":        {'d', "", false},
				"/x/a":      {'l', "sub/../b", false},
				"/x/sub":    {'l', "/mnt/real", false},
				"/mnt":      {'d', "", false},
				"/mnt/real": {'d', "", false},
				"/mnt/b":    {'f', "", true},
			},
			mounts: []string{"/", "/mnt/real"},
			check:  func(m []string) string { return checkProgram("/x/a", m) },
			want:   "FAIL prog /x/a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFS(t, tt.fs)
			got := tt.check(tt.mounts)
			if !strings.HasPrefix(got, tt.want) {
				t.Fatalf("got %q, want prefix %q", got, tt.want)
			}
		})
	}
}
