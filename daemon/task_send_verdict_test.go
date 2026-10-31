package daemon

import (
	"testing"

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
