package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// writeEnforce points the SELinux probe at a fixture holding content, or — when
// content is the sentinel "" — at a path that does not exist, standing in for an
// ordinary host with no SELinux at all.
func writeEnforce(t *testing.T, content string) {
	t.Helper()
	enforce := filepath.Join(t.TempDir(), "enforce")
	if content != "" {
		if err := os.WriteFile(enforce, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setSELinuxEnforcePaths(t, enforce)
	// Pin the kernel signal too. Without this the "absent" row would read
	// whatever box the suite happens to run on, and would flip on a Fedora
	// runner — a green run there would then be measuring the host, not the
	// contract.
	setProcFilesystems(t, "nodev\tsysfs\nnodev\tproc\n\text4\n")
}

// setProcFilesystems points the kernel-capability probe at a fixture.
func setProcFilesystems(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "filesystems")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	original := procFilesystemsPath
	procFilesystemsPath = path
	t.Cleanup(func() { procFilesystemsPath = original })
}

// mountRelabeled reports whether a bind spec carries the shared SELinux relabel.
// The mode is the last colon-separated field, so a container path is never
// mistaken for one.
func mountRelabeled(spec string) bool {
	mode := spec[strings.LastIndex(spec, ":")+1:]
	for _, opt := range strings.Split(mode, ",") {
		if opt == "z" {
			return true
		}
	}
	return false
}

// TestBindMountRelabel_CredentialAndAccountMountsAgree is the unification
// contract (#3589): the agent-credential mounts and the account mount are both
// installed by af, into the same container, on the same engine — so a host that
// needs the relabel needs it for BOTH, and one of them silently disagreeing is
// the bug this pins.
//
// All four rows of the decision are covered, and each is asserted as an
// AGREEMENT rather than two independent expectations, so a future change that
// moves one mount off the shared decision fails here even if it happens to pick
// the same answer for the row someone remembered to update.
func TestBindMountRelabel_CredentialAndAccountMountsAgree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxMode short-circuits to unobserved off linux")
	}
	for _, tt := range []struct {
		name        string
		enforce     string
		wantRelabel bool
	}{
		{name: "enforcing", enforce: "1\n", wantRelabel: true},
		{name: "permissive", enforce: "0\n", wantRelabel: false},
		{name: "absent", enforce: "", wantRelabel: false},
		{name: "probe failed", enforce: "banana\n", wantRelabel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeEnforce(t, tt.enforce)

			relabel := selinuxRelabelForHost()
			if relabel != tt.wantRelabel {
				t.Fatalf("selinuxRelabelForHost() = %v, want %v", relabel, tt.wantRelabel)
			}

			creds := agentCredentialMounts(tmux.ProgramCodex, "/home/tester", relabel, func(string) bool { return true })
			if len(creds) != 2 {
				t.Fatalf("expected one credential mount, got %v", creds)
			}
			account, err := dockerAccountMount("work", "/acct/codex/work", relabel)
			if err != nil {
				t.Fatalf("account mount failed: %v", err)
			}
			if len(account) != 2 {
				t.Fatalf("expected one account mount, got %v", account)
			}

			credZ, accountZ := mountRelabeled(creds[1]), mountRelabeled(account[1])
			if credZ != accountZ {
				t.Errorf("the two mounts DISAGREE on the relabel: credential %q (z=%v) vs account %q (z=%v)",
					creds[1], credZ, account[1], accountZ)
			}
			if credZ != tt.wantRelabel {
				t.Errorf("relabel = %v, want %v (credential %q, account %q)", credZ, tt.wantRelabel, creds[1], account[1])
			}
			// The credential mount stays read-only in every row; the account
			// mount stays read-write. Agreeing on the relabel must not have
			// leaked either one's mode into the other.
			if credMode := creds[1][strings.LastIndex(creds[1], ":")+1:]; !strings.Contains(credMode, "ro") {
				t.Errorf("credential mount %q lost its read-only mode", creds[1])
			}
			if strings.Contains(account[1], ":ro") {
				t.Errorf("account mount %q became read-only; an account is the agent's whole writable home", account[1])
			}
		})
	}
}

// TestBindMountRelabel_UnprovenEngineKeepsTheRelabel is the Codex P1 on #3589.
//
// Docker resolves AND labels a bind source on the DAEMON host, not the CLI host.
// selinuxRelabelForHost() can only read the af process's own /sys, so with
// DOCKER_HOST pointing elsewhere it measures the wrong machine — a non-SELinux
// client in front of an enforcing engine would emit plain :ro and the engine
// would deny the read, which is #3451 with an extra hop. Every row here pins the
// permissive local host (whose own answer is "no relabel") so the endpoint is
// provably what flips the decision.
func TestBindMountRelabel_UnprovenEngineKeepsTheRelabel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxMode short-circuits to unobserved off linux")
	}
	provisioner := func() *dockerProvisioner {
		return &dockerProvisioner{spec: ProvisionSpec{Program: tmux.ProgramCodex}, program: tmux.ProgramCodex}
	}
	for _, tt := range []struct {
		name        string
		dockerHost  string
		wantRelabel bool
	}{
		{name: "local unix socket trusts the local probe", dockerHost: "unix:///var/run/docker.sock", wantRelabel: false},
		{name: "local npipe trusts the local probe", dockerHost: "npipe:////./pipe/docker_engine", wantRelabel: false},
		{name: "remote tcp endpoint keeps the relabel", dockerHost: "tcp://10.0.0.7:2376", wantRelabel: true},
		{name: "remote ssh endpoint keeps the relabel", dockerHost: "ssh://build@10.0.0.7", wantRelabel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeEnforce(t, "0\n") // permissive: the LOCAL answer is "no relabel"
			t.Setenv("DOCKER_HOST", tt.dockerHost)
			t.Setenv("DOCKER_CONTEXT", "")
			if got := provisioner().bindMountRelabel(); got != tt.wantRelabel {
				t.Errorf("bindMountRelabel() with DOCKER_HOST=%q = %v, want %v", tt.dockerHost, got, tt.wantRelabel)
			}
		})
	}

	// An endpoint af cannot even resolve is the same class of unproven: it must
	// fail toward relabeling, not toward the local host's answer.
	t.Run("unresolvable endpoint keeps the relabel", func(t *testing.T) {
		writeEnforce(t, "0\n")
		t.Setenv("DOCKER_HOST", "")
		t.Setenv("DOCKER_CONTEXT", "")
		defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
			return []byte("no context"), fmt.Errorf("context inspect failed")
		})()
		if !provisioner().bindMountRelabel() {
			t.Error("an unresolvable Docker endpoint dropped the relabel; locality was never established")
		}
	})
}

// TestBindMountRelabel_RemoteEngineRelabelsBothMounts closes the loop from the
// decision to the argv: an unproven engine must relabel the account mount too,
// not just the credential mounts.
func TestBindMountRelabel_RemoteEngineRelabelsBothMounts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxMode short-circuits to unobserved off linux")
	}
	writeEnforce(t, "0\n")
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.7:2376")
	t.Setenv("DOCKER_CONTEXT", "")

	p := &dockerProvisioner{spec: ProvisionSpec{Program: tmux.ProgramCodex}, program: tmux.ProgramCodex}
	relabel := p.bindMountRelabel()

	creds := agentCredentialMounts(tmux.ProgramCodex, "/home/tester", relabel, func(string) bool { return true })
	mount, _, err := accountMountAndEnv(sessionenv.Account{Agent: "codex", Name: "work", Dir: "/acct/codex/work"}, relabel)
	if err != nil {
		t.Fatalf("account mount failed: %v", err)
	}
	if !mountRelabeled(creds[1]) || !mountRelabeled(mount[1]) {
		t.Errorf("a remote engine must relabel BOTH mounts; credential %q account %q", creds[1], mount[1])
	}
}

// TestBindMountRelabel_ContainerizedAfKeepsTheRelabel is the second Codex P1 on
// #3589: a LOCAL unix socket does not prove af shares the daemon host's SELinux
// state.
//
// af running in a container with the host's Docker socket mounted sees
// `unix://…`, which localDockerEndpoint calls local — but selinuxfs is normally
// not mounted inside a container, so /sys/fs/selinux/enforce is simply ABSENT
// there while the kernel underneath enforces. Read as "no SELinux" that emits
// plain :ro and the engine denies the credential read.
//
// /proc/filesystems settles it, and is the right instrument precisely because it
// is NOT namespaced: it lists what the running kernel registers, so it describes
// the machine Docker will label the bind source on even when read from inside a
// container. Measured on this box — host and container agree there, while
// /sys/fs/selinux does not.
func TestBindMountRelabel_ContainerizedAfKeepsTheRelabel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hostSELinuxMode short-circuits to unobserved off linux")
	}
	// What af sees inside a container on an enforcing host: no enforce file,
	// but a kernel that registers selinuxfs.
	const kernelWithSELinux = "nodev\tsysfs\nnodev\tproc\nnodev\tselinuxfs\n\text4\n"
	const kernelWithoutSELinux = "nodev\tsysfs\nnodev\tproc\nnodev\tcgroup2\n\text4\n"

	for _, tt := range []struct {
		name        string
		filesystems string
		wantRelabel bool
	}{
		{
			name:        "kernel registers selinuxfs but af cannot see it",
			filesystems: kernelWithSELinux,
			wantRelabel: true,
		},
		{
			// The ordinary non-SELinux host. This row is what keeps the gate
			// worth having: it must still reach plain :ro.
			name:        "kernel has no selinuxfs at all",
			filesystems: kernelWithoutSELinux,
			wantRelabel: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setSELinuxEnforcePaths(t, filepath.Join(t.TempDir(), "absent"))
			setProcFilesystems(t, tt.filesystems)
			t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
			t.Setenv("DOCKER_CONTEXT", "")

			p := &dockerProvisioner{spec: ProvisionSpec{Program: tmux.ProgramCodex}, program: tmux.ProgramCodex}
			if got := p.bindMountRelabel(); got != tt.wantRelabel {
				t.Errorf("bindMountRelabel() = %v, want %v", got, tt.wantRelabel)
			}
		})
	}

	// An unreadable /proc/filesystems is another "cannot prove", so it relabels
	// rather than falling through to the permissive answer.
	t.Run("unreadable proc filesystems keeps the relabel", func(t *testing.T) {
		setSELinuxEnforcePaths(t, filepath.Join(t.TempDir(), "absent"))
		original := procFilesystemsPath
		procFilesystemsPath = filepath.Join(t.TempDir(), "gone")
		t.Cleanup(func() { procFilesystemsPath = original })
		if !selinuxRelabelForHost() {
			t.Error("an unreadable /proc/filesystems dropped the relabel")
		}
	})

	// A container that DOES have selinuxfs mounted reads its host's real mode,
	// because selinuxfs is a kernel interface rather than a namespaced one. That
	// value is trustworthy, so permissive stays permissive even with a kernel
	// that registers selinuxfs.
	t.Run("a readable permissive mode is trusted over the kernel signal", func(t *testing.T) {
		enforce := filepath.Join(t.TempDir(), "enforce")
		if err := os.WriteFile(enforce, []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		setSELinuxEnforcePaths(t, enforce)
		setProcFilesystems(t, kernelWithSELinux)
		if selinuxRelabelForHost() {
			t.Error("a readable enforce=0 is the kernel's own answer and must not be overridden")
		}
	})
}
