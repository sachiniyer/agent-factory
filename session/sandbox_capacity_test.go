package session

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func capacityProvisioner(out string, runErr error, calls *[]string) *sandboxProvisioner {
	return &sandboxProvisioner{
		sessionDir: "/home/u/.af/s.abc",
		runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
			*calls = append(*calls, script)
			return []byte(out), runErr
		},
	}
}

func TestRemoteCapacityEvidence(t *testing.T) {
	boom := errors.New("clone failed")
	cases := []struct {
		name string
		out  string
		err  error
		want error
	}{
		{"enospc", "AF_CAPACITY_FAIL sh: write error: No space left on device\n", nil, syscall.ENOSPC},
		{"quota", "AF_CAPACITY_FAIL : Disk quota exceeded\n", nil, syscall.EDQUOT},
		{"probe passes", "AF_CAPACITY_OK\n", nil, nil},
		{"other failure is no evidence", "AF_CAPACITY_FAIL Permission denied\n", nil, nil},
		{"probe could not run", "", errors.New("ssh down"), nil},
		// The remote prose the failing step carried must never classify; only the probe's marker does.
		{"prose without marker", "No space left on device\n", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got := capacityProvisioner(tc.out, tc.err, &calls).remoteCapacityEvidence(boom)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no evidence, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			if len(calls) != 1 || !strings.Contains(calls[0], "LC_ALL=C") || !strings.Contains(calls[0], "/home/u/.af/s.abc") {
				t.Fatalf("probe script not pinned to the session dir with LC_ALL=C: %q", calls)
			}
		})
	}
}

func TestRemoteCapacityEvidenceSkipsSSHTransportFailure(t *testing.T) {
	var calls []string
	p := capacityProvisioner("AF_CAPACITY_FAIL No space left on device", nil, &calls)
	err := exec.Command("sh", "-c", "exit 255").Run()
	if got := p.remoteCapacityEvidence(err); got != nil || len(calls) != 0 {
		t.Fatalf("ssh exit 255 must not probe: got %v, calls %d", got, len(calls))
	}
}

// The probe lives in the failure path every ssh-transport runtime shares
// (#5686): a backend=ssh (named host list) failure must classify exactly like
// backend=sandbox, and the hook runtime's provisioner too.
func TestReapProvisionFailureJoinsCapacityEvidenceForEverySSHRuntime(t *testing.T) {
	probeFull := func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "AF_CAPACITY") {
			return []byte("AF_CAPACITY_FAIL write error: No space left on device\n"), nil
		}
		return nil, errors.New("reap: not exercised")
	}
	for name, p := range map[string]*sandboxProvisioner{
		"ssh":     newSSHSandboxProvisioner(ProvisionSpec{}, "ssh host", "/af", "claude"),
		"sandbox": {backendLabel: ""},
		"hook":    newHookSandboxProvisioner(ProvisionSpec{}, "ssh host", "/af", "claude"),
	} {
		t.Run(name, func(t *testing.T) {
			p.sessionDir = "/home/u/.af/s.abc"
			p.runCommandFn = probeFull
			err := p.reapProvisionFailure(errors.New("cloning the workspace failed"))
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("backend=%s provisioning failure lost its ENOSPC evidence: %v", name, err)
			}
		})
	}
}
