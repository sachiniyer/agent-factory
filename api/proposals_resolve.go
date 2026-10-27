package api

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

var proposalSessionFlag string

var errProposalRemote = errors.New("af proposal does not support --daemon-url yet; run it on the daemon host")

// proposalCaller is who an af proposal command acts for: the project store,
// and the session when there is one.
type proposalCaller struct {
	store     *proposal.Store
	repoID    string
	sessionID string
	title     string
}

func (c proposalCaller) committer() proposal.Committer {
	if c.sessionID == "" {
		return proposal.Committer{Author: proposal.AuthorExternal}
	}
	return proposal.Committer{Author: proposal.AuthorAgent, SessionID: c.sessionID}
}

func proposalPreflight() error {
	if apiclient.IsRemoteTarget() {
		return errProposalRemote
	}
	return nil
}

// proposalSessionFromFlagOrSelf resolves --session, or else the calling
// session. found is false only when there is no --session and the caller is
// not inside tmux; inside tmux a failed lookup is an error, so a broken af
// pane never falls back to an external commit.
func proposalSessionFromFlagOrSelf() (data *session.InstanceData, found bool, err error) {
	if proposalSessionFlag != "" {
		repoID, err := resolveRepoID()
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

func callerForSession(data *session.InstanceData) (*proposalCaller, error) {
	if data.ID == "" {
		return nil, fmt.Errorf("session %q has no stable id; af proposal needs a session created by a newer af", data.Title)
	}
	repoID := sessionRepoID(data)
	if repoID == "" {
		return nil, fmt.Errorf("cannot resolve the project of session %q", data.Title)
	}
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return nil, err
		}
		if repo.ID != repoID {
			return nil, fmt.Errorf("session %q belongs to project %s, not to --repo %s", data.Title, sessionRepoRoot(data), repoFlag)
		}
	}
	store, err := proposal.Open(repoID)
	if err != nil {
		return nil, err
	}
	return &proposalCaller{store: store, repoID: repoID, sessionID: data.ID, title: data.Title}, nil
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
// the current directory's project.
func proposalProject() (*proposalCaller, error) {
	var repo *config.RepoContext
	var err error
	if repoFlag != "" {
		repo, err = repoFromFlag()
	} else {
		repo, err = optionalCurrentRepo()
	}
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, errors.New("--repo is required: the current directory is not a git repository — pass --repo <path> to target a project")
	}
	store, err := proposal.Open(repo.ID)
	if err != nil {
		return nil, err
	}
	return &proposalCaller{store: store, repoID: repo.ID}, nil
}

// sessionTitles maps session IDs of project repoID to titles. Unknown IDs map
// to themselves, so a stale attachment still shows.
func sessionTitles(repoID string, ids []string) []string {
	byID := map[string]string{}
	if instances, err := loadRepoInstanceData(repoID); err == nil {
		for _, in := range instances {
			if in.ID != "" {
				byID[in.ID] = in.Title
			}
		}
	}
	titles := make([]string, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			titles = append(titles, t)
		} else {
			titles = append(titles, id)
		}
	}
	return titles
}
