package daemon

import (
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/task"
)

type ListOnCompleteRequest = apiproto.ListOnCompleteRequest
type ListOnCompleteResponse = apiproto.ListOnCompleteResponse

// ListOnComplete serves the task package's canonical choices without touching state.
func (s *controlServer) ListOnComplete(_ ListOnCompleteRequest, resp *ListOnCompleteResponse) error {
	resp.Values = task.OnCompleteValues()
	return nil
}
