package daemon

import (
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/task"
)

type ListOnCompleteRequest = apiproto.ListOnCompleteRequest
type ListOnCompleteResponse = apiproto.ListOnCompleteResponse

// ListOnComplete serves the task package's canonical choices without touching state.
func (s *controlServer) ListOnComplete(_ ListOnCompleteRequest, resp *ListOnCompleteResponse) error {
	values := task.OnCompleteValues()
	resp.Values = make([]apiproto.OnCompleteOption, 0, len(values))
	for _, value := range values {
		resp.Values = append(resp.Values, apiproto.OnCompleteOption{Value: value, Hint: task.OnCompleteHint(value)})
	}
	return nil
}
