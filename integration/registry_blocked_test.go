package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// registryBlockedEnv gates the check. It stands up a privileged docker-in-docker
// container and is far too heavy for every `go test ./...`, so it skips unless
// asked for: `make registry-free-check`, and one job in pr.yml.
const registryBlockedEnv = "AF_REGISTRY_BLOCKED_CHECK"

// dindImage is the isolated daemon. Pinned: a moving tag would make this check's
// own setup a registry dependency with unpredictable content.
const dindImage = "docker:28-dind"

// TestImageBuildsWithRegistryBlocked is the assertion that separates the #2521
// fix from another mitigation that merely works while Docker Hub is healthy.
//
// Everything else about the fix is observable only as an absence — no request
// went out — and an absence is exactly what a passing test cannot normally
// distinguish from a healthy network. So this stands up a second docker daemon
// with its egress dropped and measures the property directly, in three parts:
//
//	the block is real  — a pull of a not-local tag must FAIL, or the check is vacuous
//	the bug is real    — FROM the public base, absent locally, must fail in `load
//	                     metadata` with the exact signature that reddened master
//	                     and the v1.0.211 release
//	the fix works      — the REAL round-trip Dockerfiles, FROM the local-only
//	                     base, must BUILD with the registry unreachable
//
// The middle part is the one worth keeping honest about: it is this bug's
// reproduction, run every time, so the check can never pass by measuring
// nothing. It also encodes the correction to the original diagnosis — the
// round trip is caused by the base being ABSENT, not by buildkit re-resolving a
// tag it already holds. A build whose base is in the local store completes with
// no registry request at all, which is why the pre-pull in #2525 was the right
// idea; it just only ever reached one of the four lanes that needed it.
//
// The Dockerfiles come from the same functions the real tests use, so this
// cannot drift into proving a property about a copy of them.
func TestImageBuildsWithRegistryBlocked(t *testing.T) {
	if os.Getenv(registryBlockedEnv) != "1" {
		t.Skipf("set %s=1 to run the registry-blocked build check (make registry-free-check); it needs a privileged docker-in-docker container", registryBlockedEnv)
	}
	requireDocker(t)
	base := requireRoundTripBaseImage(t)

	d := startIsolatedDaemon(t)
	d.seedImage(t, base)

	// Drop the daemon's OWN egress. Locally-generated packets traverse OUTPUT,
	// so this kills registry resolution; a build's RUN steps are forwarded
	// instead of locally generated, so `apk add` still reaches the network and
	// the REAL Dockerfiles remain buildable. That asymmetry is what lets this
	// check exercise the actual images rather than a network-free stand-in.
	d.mustRun(t, 30*time.Second, "drop the isolated daemon's egress",
		"iptables -A OUTPUT -o lo -j ACCEPT && iptables -P OUTPUT DROP")

	// The block is real. Without this the whole check could pass by measuring a
	// daemon that was never actually cut off.
	if out, err := d.run(60*time.Second, "docker pull "+roundTripBaseUpstream); err == nil {
		t.Fatalf("the registry block is not in effect — a pull of %s SUCCEEDED, so this check proves nothing:\n%s", roundTripBaseUpstream, out)
	}
	t.Logf("registry block verified: %s cannot be pulled inside the isolated daemon", roundTripBaseUpstream)

	// The bug is real. Nothing seeded `alpine:3.20` into this daemon — only the
	// local-only retag — so a Dockerfile naming the public base has to resolve
	// it remotely, and cannot.
	ctl := d.buildContext(t, "control", dockerRoundTripDockerfile(roundTripBaseUpstream), nil)
	out, err := d.run(300*time.Second, "docker build -t af-registry-blocked-control "+ctl)
	if err == nil {
		t.Fatalf("FROM %s built with the registry blocked and the base absent — the reproduction no longer reproduces, so this check is no longer measuring #2521:\n%s", roundTripBaseUpstream, out)
	}
	if !strings.Contains(out, "failed to resolve source metadata") {
		t.Fatalf("FROM %s failed, but not with the #2521 signature (`failed to resolve source metadata`), so this check may be measuring an unrelated breakage:\n%s", roundTripBaseUpstream, out)
	}
	t.Logf("reproduction confirmed: FROM %s fails in `load metadata` exactly as it did on master and on the v1.0.211 release", roundTripBaseUpstream)

	// The fix works: the real images, from the local-only base, with the
	// registry unreachable.
	for _, tc := range []struct {
		name       string
		dockerfile string
		extra      map[string]string
	}{
		{"docker round-trip image", dockerRoundTripDockerfile(base), nil},
		{"sshd round-trip image", sshdRoundTripDockerfile(base), map[string]string{"entrypoint.sh": sshdRoundTripEntrypoint()}},
	} {
		dir := d.buildContext(t, strings.ReplaceAll(tc.name, " ", "-"), tc.dockerfile, tc.extra)
		out, err := d.run(600*time.Second, "docker build -t af-registry-blocked-subject "+dir)
		if err != nil {
			t.Fatalf("the %s did NOT build with the registry blocked — the fix does not hold: %v\n%s", tc.name, err, out)
		}
		if strings.Contains(out, "registry-1.docker.io") {
			t.Fatalf("the %s built, but its output names registry-1.docker.io, so something still reached for it:\n%s", tc.name, out)
		}
		t.Logf("%s built with registry-1.docker.io unreachable", tc.name)
	}
}

// --- isolated daemon helpers ------------------------------------------------

// isolatedDaemon is a throwaway dockerd in its own container, so its egress can
// be cut without touching the host's daemon or anything else running on the box.
type isolatedDaemon struct{ name string }

// startIsolatedDaemon boots the daemon and registers its teardown.
func startIsolatedDaemon(t *testing.T) *isolatedDaemon {
	t.Helper()
	if !dockerImagePresent(dindImage) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "pull", dindImage)
		cmd.WaitDelay = 10 * time.Second
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("cannot fetch %s, so the isolated daemon cannot be started: %v\n%s", dindImage, err, out)
		}
	}

	d := &isolatedDaemon{name: fmt.Sprintf("af-registry-blocked-%d", os.Getpid())}
	dockerCLI(t, 60*time.Second, false, "rm", "-f", d.name)
	t.Cleanup(func() { dockerCLI(t, 60*time.Second, false, "rm", "-f", d.name) })

	// vfs, not the default overlay2: the host's own storage is already an
	// overlay on most CI runners and in the test container, and overlay-on-overlay
	// is the classic dind failure. The images here are small enough that vfs's
	// layer duplication does not matter.
	dockerCLI(t, 120*time.Second, true, "run", "-d", "--privileged", "--name", d.name, dindImage, "--storage-driver", "vfs")

	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := d.run(15*time.Second, "docker version"); err == nil {
			return d
		}
		if time.Now().After(deadline) {
			logs, _ := dockerCLIOutput(60*time.Second, "logs", "--tail", "40", d.name)
			t.Fatalf("the isolated daemon never became ready:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
}

// seedImage copies an image from the host's store into the isolated daemon's,
// without either of them contacting a registry. A save/cp/load round trip rather
// than a pipeline: nothing streams on stdin, so nothing can be truncated.
func (d *isolatedDaemon) seedImage(t *testing.T, ref string) {
	t.Helper()
	tar := filepath.Join(t.TempDir(), "base.tar")
	dockerCLI(t, 300*time.Second, true, "save", "-o", tar, ref)
	dockerCLI(t, 300*time.Second, true, "cp", tar, d.name+":/base.tar")
	d.mustRun(t, 300*time.Second, "load the base image into the isolated daemon", "docker load -i /base.tar")
}

// buildContext materialises a build context inside the isolated daemon and
// returns its path.
func (d *isolatedDaemon) buildContext(t *testing.T, name, dockerfile string, extra map[string]string) string {
	t.Helper()
	dir := "/contexts/" + name
	d.mustRun(t, 30*time.Second, "create the build context dir", "mkdir -p "+dir)
	d.writeFile(t, dir+"/Dockerfile", dockerfile)
	for filename, content := range extra {
		d.writeFile(t, dir+"/"+filename, content)
	}
	return dir
}

// writeFile pipes content into a file inside the isolated daemon. No WaitDelay:
// this streams on stdin, and a WaitDelay on a stdin-streaming child truncates it
// silently.
func (d *isolatedDaemon) writeFile(t *testing.T, path, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.name, "sh", "-c", "cat > "+path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing %s inside the isolated daemon: %v\n%s", path, err, out)
	}
}

// run executes a shell command inside the isolated daemon.
func (d *isolatedDaemon) run(timeout time.Duration, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", d.name, "sh", "-c", script)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (d *isolatedDaemon) mustRun(t *testing.T, timeout time.Duration, what, script string) {
	t.Helper()
	if out, err := d.run(timeout, script); err != nil {
		t.Fatalf("%s: %v\n%s", what, err, out)
	}
}

// dockerCLI runs a docker command on the HOST daemon. `must` distinguishes the
// steps the check depends on from the best-effort ones (a pre-emptive `rm -f` of
// a container that is not there yet is expected to fail).
func dockerCLI(t *testing.T, timeout time.Duration, must bool, args ...string) {
	t.Helper()
	out, err := dockerCLIOutput(timeout, args...)
	if err != nil && must {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func dockerCLIOutput(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}
