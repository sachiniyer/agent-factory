package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The run status of a send into a target session carries the delivery verdict the
// daemon reached (#6224): "sent" only for a proven delivery, and every other
// outcome stays distinguishable in the record.
func TestTaskSendStatusCarriesTheDeliveryVerdict(t *testing.T) {
	cases := []struct {
		verdict session.PromptDeliveryStatus
		want    string
	}{
		{session.PromptDelivered, "sent"},
		{session.PromptSentUnverified, TaskStatusSentUnverified},
		{session.PromptCouldNotConfirm, TaskStatusSentUnconfirmed},
		{"", TaskStatusSentUnconfirmed}, // an empty or future verdict is not evidence of delivery
		{session.PromptDeliveryStatus("future"), TaskStatusSentUnconfirmed},
		{session.PromptNotDelivered, taskStatusErroredPromptUndelivered}, // provably not delivered is a failure, not a send
	}
	for _, tc := range cases {
		if got := taskSendStatus(tc.verdict); got != tc.want {
			t.Errorf("taskSendStatus(%q) = %q, want %q", tc.verdict, got, tc.want)
		}
	}
}

// TestDeliverTaskPromptRecordsUnverifiedSend drives the whole task path: a daemon
// that logged a sent-unverified delivery must not leave "sent" on the task.
func TestDeliverTaskPromptRecordsUnverifiedSend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	orig := deliverPromptForTask
	t.Cleanup(func() { deliverPromptForTask = orig })

	for _, tc := range []struct {
		verdict session.PromptDeliveryStatus
		want    string
	}{
		{session.PromptDelivered, "sent"},
		{session.PromptSentUnverified, TaskStatusSentUnverified},
		{session.PromptCouldNotConfirm, TaskStatusSentUnconfirmed},
	} {
		deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
			return taskPromptDeliveryResult{status: "sent", deliveryStatus: tc.verdict}, nil
		}
		tk := task.Task{ID: "verdict01", ProjectPath: repoPath, RepoID: config.RepoIDFromRoot(repoPath), TargetSession: "worker", Program: "claude"}
		got, err := deliverTaskPrompt(&tk, "run it", false)
		if err != nil {
			t.Fatalf("deliverTaskPrompt(%s): %v", tc.verdict, err)
		}
		if got != tc.want {
			t.Errorf("verdict %q recorded %q, want %q", tc.verdict, got, tc.want)
		}
	}
}

// Neither unproven value may read as a failure or fall out of the sandbox's
// closed status vocabulary.
func TestUnprovenSendStatusesAreNotFailuresAndSurviveTheSandboxProjection(t *testing.T) {
	for _, status := range []string{TaskStatusSentUnverified, TaskStatusSentUnconfirmed} {
		if got := sandboxRunStatusHead(status); got != status {
			t.Errorf("sandboxRunStatusHead(%q) = %q; the status must survive the sandbox projection whole", status, got)
		}
		if len(status) >= 8 && status[:8] == "errored:" {
			t.Errorf("%q must not carry the errored: prefix", status)
		}
	}
}

// The interrupted-run family the daemon persists (taskStatusAbortedShutdown on
// drain, taskStatusAbortedRuntimeReplaced on runtime replacement) is its own
// head ("aborted", no ':' inside the head), so a closed-vocabulary consumer
// needs the head listed (sandboxRunStatusHeads). Without it the projection
// collapses both to "" — indistinguishable from "never ran" — while the
// operator TUI/CLI still reads "aborted: ...". The head is daemon-chosen and
// closed-form (the family is keyed by the "aborted:" prefix elsewhere), so
// admitting it adds nothing a sandbox must not read.
func TestAbortedRunStatusesSurviveTheSandboxProjection(t *testing.T) {
	for _, status := range []string{taskStatusAbortedShutdown, taskStatusAbortedRuntimeReplaced} {
		if got := sandboxRunStatusHead(status); got != "aborted" {
			t.Errorf("sandboxRunStatusHead(%q) = %q, want %q; the aborted head must survive the sandbox projection, not collapse to \"\"", status, got, "aborted")
		}
		// The tail after the head is free-form (it carries a reason), so the
		// projection must keep only the head and never the whole status.
		if got := sandboxRunStatusHead(status); got == status {
			t.Errorf("sandboxRunStatusHead(%q) returned the whole status; only the head may cross the boundary", status)
		}
	}
	// sandboxSafeTask keeps LastRunAt and projects LastRunStatus to the head,
	// so a sandbox sees a last-run timestamp with "aborted" — not "" — for an
	// interrupted occurrence.
	at := time.Now()
	got := sandboxSafeTask(task.Task{
		ID: "t", LastRunAt: &at, LastRunStatus: taskStatusAbortedShutdown,
	})
	if got.LastRunStatus != "aborted" {
		t.Errorf("sandboxSafeTask LastRunStatus = %q, want %q", got.LastRunStatus, "aborted")
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(at) {
		t.Errorf("sandboxSafeTask LastRunAt was not preserved: got %v, want %v", got.LastRunAt, at)
	}
	// A genuinely empty status (never ran) still projects to "", so the
	// distinction the interrupted-run marker exists to communicate is preserved.
	if got := sandboxRunStatusHead(""); got != "" {
		t.Errorf("sandboxRunStatusHead(\"\") = %q, want \"\"", got)
	}
}
