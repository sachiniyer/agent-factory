package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/doctor"

	"github.com/spf13/cobra"
)

func TestDoctorExitCode(t *testing.T) {
	for _, tt := range []struct {
		name       string
		unresolved bool
		incomplete []string
		want       int
	}{
		{name: "clean", want: 0},
		{name: "incomplete only", incomplete: []string{"stale-temp-home"}, want: 1},
		{name: "unresolved only", unresolved: true, want: 1},
		{name: "unresolved and incomplete", unresolved: true, incomplete: []string{"stale-temp-home"}, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := &doctor.Report{Incomplete: tt.incomplete}
			if tt.unresolved {
				report.Findings = []doctor.Finding{{Actionable: true}}
			}
			if got := doctorExitCode(report); got != tt.want {
				t.Errorf("doctorExitCode(unresolved=%d, incomplete=%v) = %d, want %d", report.UnresolvedCount(), report.Incomplete, got, tt.want)
			}
		})
	}
}

// withDoctorRun replaces the package-level doctor.Run seam for t's duration so
// a test can drive doctorCmd.RunE without the real tmux/daemon/process sweep
// doctor.Run performs. A canned report keeps doctorExitCode at 0 so RunE
// returns normally instead of os.Exit-ing and killing the test binary.
func withDoctorRun(t *testing.T, fn func(doctor.Options) (*doctor.Report, error)) {
	t.Helper()
	prev := doctorRun
	doctorRun = fn
	t.Cleanup(func() { doctorRun = prev })
}

// withDoctorJSONFlag sets doctorJSONFlag for t's duration and restores it after,
// so the shared --json flag does not leak into neighboring tests. Matches the
// configJSONFlag save/restore in configcmd_test.go's TestConfigGetUnknownKey*
// tests.
func withDoctorJSONFlag(t *testing.T) {
	t.Helper()
	prev := doctorJSONFlag
	doctorJSONFlag = true
	t.Cleanup(func() { doctorJSONFlag = prev })
}

// TestDoctorJSONRenderWriteFailureEnvelopesOnStderr locks the fix for the bug
// introduced in 33f91fd6: when `af doctor --json`'s stdout write errors out
// (EPIPE from an early-closing consumer, ENOSPC on a full-disk redirect),
// doctor.RenderJSON returns the write error and RunE must route it through
// jsonWrapError so the shared {data,error} envelope lands on stderr and cobra's
// bare "Error:" line is silenced. Bare `return err` instead left a `--json`
// caller with a plain Go error string on stderr, breaking the contract
// jsonWrapError documents.
//
// closedPipeWriter (defined in configcmd_test.go) models the real write error
// fmt.Fprintln returns from a dead writer -- io.ErrClosedPipe is the same class
// production hits on EPIPE, not a synthetic sentinel (gate-pr.md: "a fake must
// model production's real error shape").
func TestDoctorJSONRenderWriteFailureEnvelopesOnStderr(t *testing.T) {
	withDoctorRun(t, func(doctor.Options) (*doctor.Report, error) {
		return &doctor.Report{}, nil
	})
	withDoctorJSONFlag(t)

	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetOut(closedPipeWriter{}) // stdout write fails immediately (EPIPE)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = false
	cmd.SilenceErrors = false

	err := doctorCmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("doctorCmd.RunE on a failing stdout writer must return an error")
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("returned error = %v, want errors.Is io.ErrClosedPipe", err)
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr must carry the {data,error} envelope, got empty stderr")
	}

	// The envelope a `--json` caller is promised, not a bare Go error string.
	var env apiproto.Envelope
	if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
		t.Fatalf("stderr is not the shared envelope: %v\n%s", err, stderr.String())
	}
	if env.Data != nil {
		t.Errorf("failure envelope data should be null, got %v", env.Data)
	}
	if env.Error == nil {
		t.Fatalf("envelope error must be populated, got nil\n%s", stderr.String())
	}
	if !strings.Contains(env.Error.Message, io.ErrClosedPipe.Error()) {
		t.Errorf("envelope error.message = %q, want it to name the write failure %q",
			env.Error.Message, io.ErrClosedPipe.Error())
	}

	// jsonWrapError must have latched SilenceErrors (and SilenceUsage) on cmd so
	// cobra does not also print a bare "Error:" line on top of the envelope.
	if !cmd.SilenceErrors {
		t.Errorf("cmd.SilenceErrors must be true so cobra's bare Error: line is suppressed")
	}
	if !cmd.SilenceUsage {
		t.Errorf("cmd.SilenceUsage must be true so cobra's usage block is suppressed")
	}
}

// TestDoctorJSONRenderSuccessWritesStdoutOnly is the non-regression companion:
// a successful --json run still puts the report envelope on stdout, leaves
// stderr empty, and returns nil -- the fix did not perturb the happy path.
func TestDoctorJSONRenderSuccessWritesStdoutOnly(t *testing.T) {
	withDoctorRun(t, func(doctor.Options) (*doctor.Report, error) {
		return &doctor.Report{}, nil
	})
	withDoctorJSONFlag(t)

	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := doctorCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("clean --json run returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr must be empty on success, got: %s", stderr.String())
	}
	var env apiproto.Envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not the shared envelope: %v\n%s", err, stdout.String())
	}
	if env.Error != nil {
		t.Errorf("success envelope must have null error, got: %+v", env.Error)
	}
}
