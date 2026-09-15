package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
)

// `af quota` gained a remote form in #4361: pointed at a remote daemon it asks
// THAT daemon for its usage report over /v1/QuotaReport instead of reading this
// machine's records. These tests pin the three properties that keep the feature
// honest rather than the bug it replaced (a refusal that made the remote read
// impossible):
//
//	routes           the request reaches the TARGETED daemon, and what it
//	                 prints is the daemon's own rows — word for word, caveat
//	                 for caveat — never a report built from the caller's disk.
//	never local      a remote failure reads nothing locally. The tempting
//	                 repair for an unreachable or too-old daemon — "fall back
//	                 to the local records like a daemonless run" — would hand
//	                 an operator a valid-looking report for the wrong host.
//	refuses skew     a daemon that does not serve the route is refused BY
//	                 NAME AND VERSION with the two ways forward.
//
// The local read model itself — which records count, how a parked session's
// sighting time renders — is proven in quotahost's and quota's own tests.

// runQuotaCLI executes `af quota <args…>` through the real command tree and
// returns what the user would see. It restores every piece of process-global
// state cobra and apiclient keep between Execute calls, so one case cannot leak
// a remote target into the next. Mirrors runConfigCLI for this command.
func runQuotaCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	prevFlagURL := apiclient.FlagDaemonURL

	var out, errBuf bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(append([]string{"quota"}, args...))
	t.Cleanup(func() {
		// The bound var is what apiclient resolves; the pflag value is what a
		// later Execute would re-apply. Clear both, then restore the var.
		if f := rootCmd.PersistentFlags().Lookup("daemon-url"); f != nil {
			_ = f.Value.Set("")
			f.Changed = false
		}
		apiclient.FlagDaemonURL = prevFlagURL
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		resetCobraSilence(rootCmd)
	})

	err = rootCmd.Execute()
	return out.String(), errBuf.String(), err
}

// seedLocalQuotaEvidence writes one local parked-codex record — proof-positive
// rows that MUST NOT appear in a remote answer, so a silent local fallback is
// caught by content rather than by the absence of an error.
func seedLocalQuotaEvidence(t *testing.T, home string) {
	t.Helper()
	rows, err := json.Marshal([]session.InstanceData{{
		Title: "local-parked", Program: "codex", Liveness: session.LiveLimitReached,
		LimitResetAt:    time.Unix(2_000_000_000, 0),
		LimitObservedAt: time.Unix(1_900_000_000, 0),
	}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	dir := filepath.Join(home, "instances", "localrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), rows, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestQuotaRemotePrintsTheTargetedDaemonsReport(t *testing.T) {
	for _, name := range remoteRouteNames {
		t.Run(name, func(t *testing.T) {
			home := newConfigHome(t)
			seedLocalQuotaEvidence(t, home)
			stub := newStubDaemon(t, "1.9.0")
			stub.quotaResp = stubQuotaResponse()
			target := remoteRouteTo(name, stub.url())
			t.Setenv("AF_DAEMON_URL", target.env)

			out, _, err := runQuotaCLI(t, withRoute(target.prefix)...)
			if err != nil {
				t.Fatalf("quota against a serving daemon must succeed, got: %v", err)
			}
			if stub.quotaHits() != 1 {
				t.Fatalf("the targeted daemon must receive exactly one QuotaReport call, got %d", stub.quotaHits())
			}
			for _, want := range []string{"stubagent", "not reported", "limit reached", "the daemon's distinctive detail"} {
				if !strings.Contains(out, want) {
					t.Errorf("stdout must carry the daemon's own row cells verbatim; want %q in:\n%s", want, out)
				}
			}
			// The local disk must never leak into a remote answer: the seeded
			// parked codex row's agent name appears in no stub row or note.
			if strings.Contains(out, "codex") {
				t.Errorf("a remote answer must contain no locally-read rows, got:\n%s", out)
			}
		})
	}
}

func TestQuotaRemoteCarriesTheDaemonsCaveats(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "1.9.0")
	resp := stubQuotaResponse()
	resp.Caveats = []string{"1 project record file(s) could not be parsed and were skipped, so this report is INCOMPLETE"}
	stub.quotaResp = resp
	t.Setenv("AF_DAEMON_URL", "")

	_, errOut, err := runQuotaCLI(t, "--daemon-url", stub.url())
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if !strings.Contains(errOut, "INCOMPLETE") {
		t.Errorf("the daemon's under-read caveat must reach the operator, got stderr: %q", errOut)
	}
}

func TestQuotaRemoteRefusesADaemonThatDoesNotServeTheRoute(t *testing.T) {
	home := newConfigHome(t)
	// The whole point: a fallback would have real evidence to print.
	seedLocalQuotaEvidence(t, home)
	stub := newStubDaemon(t, "0.9.1", "/v1/QuotaReport")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runQuotaCLI(t, "--daemon-url", stub.url())
	if err == nil {
		t.Fatal("af quota must REFUSE a daemon that does not serve QuotaReport")
	}
	msg := err.Error()
	for _, want := range []string{"af quota", stub.url(), "QuotaReport", "0.9.1", "Nothing was read locally", "daemon host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must contain %q so the reader can act on it, got: %q", want, msg)
		}
	}
	if strings.Contains(out, "local-parked") {
		t.Errorf("a refused remote read must print no locally-derived rows, got stdout:\n%s", out)
	}
}

func TestQuotaRemoteNeverReadsLocalRecordsOnFailure(t *testing.T) {
	home := newConfigHome(t)
	seedLocalQuotaEvidence(t, home)
	// A daemon that answers nothing at all: connection refused.
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")

	out, _, err := runQuotaCLI(t)
	if err == nil {
		t.Fatal("quota against an unreachable daemon must fail")
	}
	if strings.Contains(out, "local-parked") || strings.Contains(out, "codex") {
		t.Errorf("a failed remote read must not fall back to this machine's records, got stdout:\n%s", out)
	}
}

func TestQuotaLocalReadsThisMachinesRecords(t *testing.T) {
	home := newConfigHome(t)
	seedLocalQuotaEvidence(t, home)
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runQuotaCLI(t)
	if err != nil {
		t.Fatalf("local quota must succeed, got: %v", err)
	}
	if !strings.Contains(out, "codex") || !strings.Contains(out, "limit reached") {
		t.Errorf("the local read must show the seeded parked session, got stdout:\n%s", out)
	}
	// The seeded sighting time must be visible — staleness is the feature.
	if !strings.Contains(out, "observed") {
		t.Errorf("the local read must name when the wall was observed, got stdout:\n%s", out)
	}
}

// stubQuotaResponse is a report only the stub daemon could produce: the agent
// name and detail are deliberately not values a real projection emits, so any
// of it appearing in output proves the remote answer was rendered as sent.
func stubQuotaResponse() daemon.QuotaReportResponse {
	return daemon.QuotaReportResponse{
		Rows: []quota.Row{{
			Agent:    "stubagent",
			Quota:    "not reported",
			Observed: "limit reached",
			Detail:   "the daemon's distinctive detail",
		}},
		Note: quota.ReportNote,
	}
}
