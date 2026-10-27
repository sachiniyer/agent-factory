package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProvisionFailure_DataRootNamespaceGate is the #5382 item-6 regression:
// the statfs verdict is only meaningful when the engine's reported data root
// is the SAME filesystem object af would probe. A remote or SSH-forwarded
// engine (a tcp:// endpoint — even on loopback, where `ssh -L` parks one)
// reports a daemon-side path in another host's namespace; a local Unix socket
// does not prove a shared mount namespace either — Docker Desktop's VM or an
// af container holding the host socket sees the daemon's /var/lib/docker as a
// different filesystem. Only an endpoint proven on this host AND a daemon
// hostname matching af's own is co-location evidence; anything less must
// yield no verdict — unknown is not-proven, never "full".
func TestProvisionFailure_DataRootNamespaceGate(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)

	failRun := func(args []string) ([]byte, error) {
		if args[0] == "run" {
			return []byte("Error response from daemon: no space left on device\n"), fmt.Errorf("exit status 125")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}

	t.Run("tcp loopback endpoint is not namespace proof", func(t *testing.T) {
		// `ssh -L 2375:host:2375` puts a REMOTE daemon's port on this host's
		// loopback — the reported root is on the remote filesystem, and a
		// local statfs of the same path is a verdict about the wrong
		// filesystem.
		t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
		t.Setenv("DOCKER_CONTEXT", "")
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		statfsCalls := 0
		old := statfsFull
		statfsFull = func(string) (bool, error) { statfsCalls++; return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "tcp-loopback"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a tcp endpoint cannot prove the data root shares af's mount namespace — no ENOSPC may be invented: %v", err)
		assert.Equal(t, 0, statfsCalls, "the data root must not be probed through a tcp endpoint")
	})

	t.Run("daemon hostname mismatch is not namespace proof", func(t *testing.T) {
		// A Unix socket only proves the daemon answers on this machine —
		// a proxied or forwarded socket (Docker Desktop's host artifact,
		// ssh -U) answers with the REMOTE daemon's info, whose reported
		// DockerRootDir lives on another filesystem entirely.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", "af-test-foreign-daemon", false), failRun)

		statfsCalls := 0
		old := statfsFull
		statfsFull = func(string) (bool, error) { statfsCalls++; return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "foreign-ns"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a daemon answering under another kernel's hostname must not get a full verdict from af's filesystem: %v", err)
		assert.Equal(t, 0, statfsCalls, "a mismatched daemon hostname must not reach statfs")
	})

	statfsCounter := func(t *testing.T) *int {
		t.Helper()
		calls := new(int)
		old := statfsFull
		statfsFull = func(string) (bool, error) { *calls++; return true, nil }
		t.Cleanup(func() { statfsFull = old })
		return calls
	}

	t.Run("a peer whose data root is another filesystem is not proof", func(t *testing.T) {
		// The #5382-review gap a hostname cannot close: `--uts=host` gives
		// an af container the host's name and `-v` hands it the daemon's
		// socket — but the answering daemon's own mount table shows its
		// data root on a device af's stat does not see (af's
		// /var/lib/docker is the container rootfs).
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("9:9"), peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "borrowed-socket"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a socket answered for a data root on another device proves the daemon answers, not that af's /var/lib/docker is the daemon's: %v", err)
		assert.Equal(t, 0, *calls, "a foreign-device peer must not reach statfs")
	})

	t.Run("an unidentifiable peer is not proof", func(t *testing.T) {
		// The platform cannot name the answering process (no SO_PEERCRED)
		// or the socket is unreachable — unproven, not shared.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), fmt.Errorf("peer credentials unavailable"))
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "unnamed-peer"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a peer af cannot name cannot be shown to share its data root: %v", err)
		assert.Equal(t, 0, *calls, "an unnamed peer must not reach statfs")
	})

	t.Run("an unreadable peer table falls back to af's own table", func(t *testing.T) {
		// Unprivileged af + rootful dockerd: /proc/<peer> is ptrace-gated,
		// but af's own mountinfo always reads — the data root sitting on a
		// whole filesystem is positive proof enough to keep probing (#5382
		// review: this deployment must not lose typed ENOSPC evidence).
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", "", peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "unprivileged-client"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a docker-group user must still get the verdict when af's own table proves the root: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a permission-blind identity read falls back for a root peer", func(t *testing.T) {
		// Stock Linux, non-root af: /proc/<dockerd>/mountinfo is readable
		// but /proc/<dockerd>/exe needs PTRACE_MODE_READ — EACCES is
		// "identity unproven", routing to the same uid-0 + whole-fs-self
		// fallback an unreadable table takes.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "denied-root"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"EACCES on the identity read must reach the uid-0 fallback, not fail closed: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a readable denied table still vetoes a foreign filesystem", func(t *testing.T) {
		// In the denied route the peer table is readable — it may not
		// prove (identity unknown) but it must veto: a different device
		// under the data root is the foreign-fs shape af's own table
		// would otherwise accept.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:2"), peerTableOn("8:1"), nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "denied-foreign"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a readable peer table showing another device vetoes the fallback: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("a denied route accepts two tables agreeing on a subvolume", func(t *testing.T) {
		// Btrfs `/@`: the data root sits on a mounted subvolume whose
		// mountinfo root is legitimately non-"/". af's own table cannot
		// tell that from a foreign bind — the peer's READABLE table saying
		// the same subtree of the same device is the positive witness.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		subvol := peerTableSubvol("8:1", "/@", "/var/lib/docker")
		stubPeerDataRoot(t, 0, "8:1", subvol, subvol, nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "denied-subvol"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"both tables agreeing on the /@ subtree of af's stat device is co-location proof: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a btrfs stat device that never matches mountinfo still proves", func(t *testing.T) {
		// The real btrfs shape: stat() on a path inside subvol @ reports
		// the SUBVOLUME's anonymous st_dev (0:65), which the covering
		// mount's dev field (0:59) never equals. A table-vs-stat compare
		// would veto a genuinely co-located engine; table-vs-table on
		// (dev, root) does not.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		subvol := peerTableSubvol("0:59", "/@", "/var/lib/docker")
		stubPeerDataRoot(t, 0, "0:65", subvol, subvol, nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "btrfs-anon-dev"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"subvolume anon st_dev divergence must not defeat co-location proof: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a proven daemon on a subvolume shares its filesystem view", func(t *testing.T) {
		// Same shape through the STRONG path: peerExe names dockerd, both
		// tables cover the data root on the same (dev, root) — the compare
		// must not lean on af's stat device either.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		subvol := peerTableSubvol("0:59", "/@", "/var/lib/docker")
		stubPeerDataRoot(t, 0, "0:65", subvol, subvol, nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "proven-subvol"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"two tables naming the same (dev, root) prove co-location for a proven dockerd: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a denied route rejects when the tables disagree on the subtree", func(t *testing.T) {
		// Same device, different covering root — af sees a subvolume mount
		// the peer does not share (or vice versa): the views disagree, so
		// the cross-check cannot carry the verdict.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableSubvol("8:1", "/@", "/var/lib/docker"), nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "denied-root-mismatch"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"disagreeing subtree roots keep the non-/ self mount unproven: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("an unreadable peer table cannot rescue a subvolume view", func(t *testing.T) {
		// Without the peer's table nothing cross-checks the non-"/" root:
		// a mounted subvolume stays indistinguishable from a foreign
		// partial bind and the fallback stays closed.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", "", peerTableSubvol("8:1", "/@", "/var/lib/docker"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "blind-subvol"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a non-/ self root with no peer table to corroborate stays unproven: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("a permission-blind identity read cannot help a non-root peer", func(t *testing.T) {
		// A same-uid forwarder whose exe could not be read still fails:
		// the fallback requires uid 0 and af's own user is not it.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 1000, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.EACCES}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "denied-nonroot"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a denied identity read on a non-root peer must not borrow the fallback: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("a vanished peer is not a denied one", func(t *testing.T) {
		// ENOENT means the answering process is gone — no evidence, never
		// the fallback (which belongs to live-but-unreadable peers).
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		old := peerExe
		peerExe = func(int) (string, error) {
			return "", &os.PathError{Op: "readlink", Path: "/proc/1/exe", Err: syscall.ENOENT}
		}
		t.Cleanup(func() { peerExe = old })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "vanished"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a vanished process is rejected, not denied — no evidence: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("an unprivileged peer cannot use the fallback", func(t *testing.T) {
		// The foreign-socket vector — Docker Desktop's shared socket, an
		// `ssh -U` forwarder — answers as AF'S OWN user. If the peer's
		// table is unreadable AND its uid is not root's, a whole-fs self
		// mount is not proof (#5382 review).
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 1000, "8:1", "", peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "same-uid-proxy"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a same-uid answerer is the proxy/forwarder shape — the fallback must not apply: %v", err)
		assert.Equal(t, 0, *calls, "an unprivileged peer must not reach statfs")
	})

	t.Run("a partial bind over the data root defeats the fallback", func(t *testing.T) {
		// The fallback's remaining hole: af's own table showing a partial
		// bind (mount root != "/") under the data-root path means a foreign
		// subtree — af's statfs there is not the daemon's filesystem.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		selfTable := peerTableOn("8:1") +
			"30 24 8:1 /foreign/var/lib/docker /var/lib/docker rw - ext4 /dev/sda1 rw\n"
		stubPeerDataRoot(t, 0, "8:1", "", selfTable, nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "bind-covered-root"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a subtree bind under the data root could be a foreign filesystem — unproven: %v", err)
		assert.Equal(t, 0, *calls, "a partial-bind-covered root must not reach statfs")
	})

	t.Run("a same-uid forwarder is not the daemon", func(t *testing.T) {
		// socat/ssh -L answering the socket runs as af's uid: its mountinfo
		// is readable and describes AF's filesystem, so a device match alone
		// "proves" co-location with a remote engine. The exe identity is
		// what separates the daemon from the pipe (#5382 review).
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		old := peerExe
		peerExe = func(int) (string, error) { return "/usr/bin/socat", nil }
		t.Cleanup(func() { peerExe = old })
		oldH := listenerHolders
		listenerHolders = func(string) (map[int]struct{}, bool) {
			return map[int]struct{}{424242: {}}, true // socat holds its own listener
		}
		t.Cleanup(func() { listenerHolders = oldH })
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "forwarder"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"a forwarder's mount table describes af's fs, not the remote daemon's — no verdict: %v", err)
		assert.Equal(t, 0, *calls, "a forwarding peer must not reach statfs")
	})

	// stubSocketActivation makes the peer a non-dockerd listener creator
	// (systemd, or the play-test activator) while pid 424243 holds the same
	// listener fd — the `dockerd -H fd://` shape. mountinfo for the creator
	// is PID 1's table; the server's is the daemon table on dev.
	stubSocketActivation := func(t *testing.T, serverExe string, extra int) {
		t.Helper()
		stubPeerDataRoot(t, 0, "8:1", "1 0 0:0 / / rw - rootfs rootfs rw", peerTableOn("8:1"), nil)
		oldE, oldH, oldM := peerExe, listenerHolders, peerMountinfo
		peerExe = func(pid int) (string, error) {
			if pid == 424243 {
				return serverExe, nil
			}
			return "/usr/lib/systemd/systemd", nil
		}
		listenerHolders = func(string) (map[int]struct{}, bool) {
			h := map[int]struct{}{424242: {}, 424243: {}}
			if extra > 0 {
				h[extra] = struct{}{}
			}
			return h, true
		}
		peerMountinfo = func(pid int) ([]byte, error) {
			if pid == 424242 {
				// The creator (PID 1 shape) sees its own table, not the
				// daemon's — nothing under the data root may match.
				return []byte("1 0 0:0 / / rw - rootfs rootfs rw"), nil
			}
			// The serving dockerd AND af's own table both cover the data
			// root on the same (dev, root) — the proven compare.
			return []byte(peerTableOn("8:1")), nil
		}
		t.Cleanup(func() { peerExe, listenerHolders, peerMountinfo = oldE, oldH, oldM })
	}

	t.Run("a socket-activated local daemon still proves co-location", func(t *testing.T) {
		// dockerd -H fd:// behind docker.socket: SO_PEERCRED names PID 1
		// (the listener's creator), but the fd-holder set proves dockerd
		// is the serving process and ITS mountinfo carries the compare.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubSocketActivation(t, "/usr/bin/dockerd", 0)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "activated"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a socket-activated dockerd is still the daemon — evidence must join: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("an activated socket served by a non-dockerd is unproven", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubSocketActivation(t, "/usr/bin/socat", 0)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "act-fwd"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"one holder besides the creator, but it is not dockerd: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("extra listener holders are ambiguous", func(t *testing.T) {
		// A third process holding the fd could have served the connection —
		// the server is no longer uniquely named, so nothing is proven.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubSocketActivation(t, "/usr/bin/dockerd", 424244)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "ambiguous"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"an ambiguous holder set cannot prove the server: %v", err)
		assert.Equal(t, 0, *calls)
	})

	t.Run("stacked mounts take the top of the stack", func(t *testing.T) {
		// Real rigs stack mounts at one mountpoint: a volume bind at
		// /var/lib/docker with a tmpfs (the controlled data root) mounted
		// over it. af's stat sees the top entry; the peer table lists both
		// — the proof must compare against the LAST covering entry.
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stacked := peerTableOn("8:1") +
			"1415 1371 8:1 /volumes/v/_data /var/lib/docker rw - ext4 /dev/sda1 rw\n" +
			"1160 1415 0:198 / /var/lib/docker rw - tmpfs tmpfs rw\n"
		// af's own table shows the same stack (the rig shares the mountns);
		// the compare must take the top entry in BOTH tables.
		stubPeerDataRoot(t, 0, "0:198", stacked, stacked, nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "stacked-root"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"the top of a mount stack is what stat sees — the verdict must apply: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("a symlinked socket path still dials the daemon", func(t *testing.T) {
		// DOCKER_HOST may traverse a symlink (/var/run -> /run); the dial
		// resolves it and the peer answer is unaffected.
		sock := fakeUnixDockerSocket(t)
		sockPath := strings.TrimPrefix(sock, "unix://")
		// The endpoint path must stay under sun_path's ~104 bytes — keep the
		// symlink dir next to the socket under /tmp, not in t.TempDir()'s
		// deep /var/folders tree (macOS dial fails "invalid argument").
		linkParent, lerr := os.MkdirTemp("/tmp", "af-lnk-")
		require.NoError(t, lerr)
		t.Cleanup(func() { os.RemoveAll(linkParent) })
		linkDir := filepath.Join(linkParent, "lnk")
		require.NoError(t, os.Symlink(filepath.Dir(sockPath), linkDir))
		t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(linkDir, filepath.Base(sockPath)))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)
		calls := statfsCounter(t)

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "symlinked-sock"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a symlinked endpoint still reaches the same daemon — the verdict must apply: %v", err)
		assert.Greater(t, *calls, 0)
	})

	t.Run("local endpoint plus matching hostname probes", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		old := statfsFull
		statfsFull = func(string) (bool, error) { return true, nil }
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "co-located"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.True(t, errors.Is(err, syscall.ENOSPC),
			"a co-located engine whose data root is full must still classify environmental: %v", err)
	})

	t.Run("a root af cannot stat is not full", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", fakeUnixDockerSocket(t))
		t.Setenv("DOCKER_CONTEXT", "")
		stubPeerDataRoot(t, 0, "8:1", peerTableOn("8:1"), peerTableOn("8:1"), nil)
		fakeDockerCapacityProbe(t, dockerInfoAnswer(t, "/var/lib/docker", host, false), failRun)

		old := statfsFull
		statfsFull = func(string) (bool, error) {
			return false, &os.PathError{Op: "statfs", Path: "/var/lib/docker", Err: syscall.ENOENT}
		}
		defer func() { statfsFull = old }()

		p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "unstatable"}}
		_, err := p.provision()
		require.Error(t, err)
		assert.False(t, errors.Is(err, syscall.ENOSPC),
			"an unstatable data root cannot produce a full verdict: %v", err)
	})
}
