package daemon

import "github.com/sachiniyer/agent-factory/session"

// KillSessionKept is KillSession that also reports the branch the kill left in
// place in the repo (#6233), kept on the committed path too, where KillSession
// drops the response payload.
func KillSessionKept(req KillSessionRequest) (session.KeptBranch, error) {
	var resp KillSessionResponse
	err := callDaemon("KillSession", req, &resp)
	if err != nil && !isMutationCommitted(err) {
		return session.KeptBranch{}, err // clean failure → caller hard-fails
	}
	return resp.KeptBranch, err // success (nil) OR committed path → caller classifies via IsMutationCommitted
}
