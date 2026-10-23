package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

var proposalSessionFlag string

// callProposalRoute sends one proposal route to the targeted daemon: the local
// one over the control socket (started when needed), or the --daemon-url one
// over HTTP. A remote target never falls back to the local daemon.
var callProposalRoute = func(method string, req, resp any) error {
	if !apiclient.IsRemoteTarget() {
		return daemon.CallProposal(method, req, resp)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return client.CallProposal(method, req, resp)
}

// proposalCaller is who an af proposal command acts for: a session, or a
// project named by repo id (local daemon) or by path (remote daemon).
type proposalCaller struct {
	sessionID string
	repoID    string
	repoPath  string
}

func (c proposalCaller) author() proposal.Author {
	if c.sessionID != "" {
		return proposal.AuthorAgent
	}
	return proposal.AuthorExternal
}

// proposalSessionFromFlagOrSelf resolves --session, or else the calling
// session. found is false only when there is no --session and the caller is
// not inside tmux; inside tmux a failed lookup is an error, so a broken af
// pane never falls back to an external commit.
func proposalSessionFromFlagOrSelf() (data *session.InstanceData, found bool, err error) {
	if proposalSessionFlag != "" {
		if apiclient.IsRemoteTarget() {
			data, err := remoteProposalSession(proposalSessionFlag)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		}
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return nil, false, err
		}
		data, _, err := getSessionByTitleInScope(repoID, proposalSessionFlag)
		if err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	if os.Getenv("TMUX") == "" {
		return nil, false, nil
	}
	data, err = resolveSelfSession()
	if err != nil {
		return nil, false, fmt.Errorf("cannot resolve the calling af session (pass --session <title> to name one): %w", err)
	}
	return data, true, nil
}

// remoteProposalSession finds a session on a remote daemon by title. There
// --repo is a path on the daemon host, so it narrows the match by the project
// root the daemon reports and is never resolved on this machine. A relative or
// ~ path cannot be compared here, so it is refused rather than dropped.
func remoteProposalSession(title string) (*session.InstanceData, error) {
	if repoFlag == "" {
		data, _, err := getSessionByTitle(title)
		return data, err
	}
	if !filepath.IsAbs(repoFlag) {
		return nil, fmt.Errorf("--repo %q must be an absolute path on the daemon host at %s to scope --session %q", repoFlag, apiclient.RemoteTargetURL(), title)
	}
	all, _, _, err := snapshotRead(daemon.SnapshotRequest{})
	if err != nil {
		return nil, err
	}
	want := filepath.Clean(repoFlag)
	var matches []session.InstanceData
	for i := range all {
		root := sessionRepoRoot(&all[i])
		if all[i].Title == title && root != "" && pathutil.IsAtOrInside(want, filepath.Clean(root)) {
			matches = append(matches, all[i])
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("session %q %w in project %s on the daemon at %s", title, errTitleNotFound, repoFlag, apiclient.RemoteTargetURL())
	}
	if paths := session.DedupeSorted(repoPathsOf(matches)); len(paths) > 1 {
		return nil, session.AmbiguousTitleError(title, paths)
	}
	return &matches[0], nil
}

func callerForSession(data *session.InstanceData) (*proposalCaller, error) {
	if data.ID == "" {
		return nil, fmt.Errorf("session %q has no stable id; af proposal needs a session created by a newer af", data.Title)
	}
	if repoFlag != "" && !apiclient.IsRemoteTarget() {
		repo, err := repoFromFlag()
		if err != nil {
			return nil, err
		}
		if id := sessionRepoID(data); id != "" && repo.ID != id {
			return nil, fmt.Errorf("session %q belongs to project %s, not to --repo %s", data.Title, sessionRepoRoot(data), repoFlag)
		}
	}
	return &proposalCaller{sessionID: data.ID}, nil
}

// requireProposalSession resolves the session for a verb that needs one.
func requireProposalSession(verb string) (*proposalCaller, error) {
	data, found, err := proposalSessionFromFlagOrSelf()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("af proposal %s needs a session: run it inside an af session, or pass --session <title>", verb)
	}
	return callerForSession(data)
}

// optionalProposalSession uses the session when there is one, and otherwise
// the project rules.
func optionalProposalSession() (*proposalCaller, error) {
	data, found, err := proposalSessionFromFlagOrSelf()
	if err != nil {
		return nil, err
	}
	if found {
		return callerForSession(data)
	}
	return proposalProject()
}

// proposalProject resolves the project by the shared contract: --repo, then
// the current directory's project. Against a remote daemon --repo is a path on
// that host and the current directory names nothing there, so without --repo
// the calling session gives the project.
func proposalProject() (*proposalCaller, error) {
	if apiclient.IsRemoteTarget() {
		if repoFlag != "" {
			return &proposalCaller{repoPath: repoFlag}, nil
		}
		data, found, err := proposalSessionFromFlagOrSelf()
		if err != nil {
			return nil, err
		}
		if found {
			return callerForSession(data)
		}
		return nil, fmt.Errorf("--repo is required with --daemon-url: pass --repo <path on the daemon host at %s>, or run inside an af session", apiclient.RemoteTargetURL())
	}
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return nil, err
		}
		return &proposalCaller{repoID: repo.ID}, nil
	}
	repo, err := optionalCurrentRepo()
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, errors.New("--repo is required: the current directory is not a git repository — pass --repo <path> to target a project")
	}
	return &proposalCaller{repoID: repo.ID}, nil
}
