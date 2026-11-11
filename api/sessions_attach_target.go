package api

import "github.com/sachiniyer/agent-factory/apiclient"

// resolveAttachTarget resolves the {title, repoID} pair `sessions attach` hands
// to the daemon's WS PTY stream.
//
// --repo scoping is honored (#891, same class as #761/#776): an empty repoID
// preserves the all-repo search, a non-empty one confines the attach to that
// repo so `attach <title> --repo <other>` can never connect the terminal to a
// same-titled session in a different repo.
//
// The remote branch is #1974. Attach runs over the apiclient transport, so it
// follows --daemon-url to another machine — but it was resolving the session on
// THIS machine's disk first (resolveRepoID + findLiveInstanceByTitleInScope),
// which fails "session not found" before any remote call for a session that
// only exists on the daemon. Both halves were wrong against a remote: the repo
// ID hashes the CLIENT's cwd, naming a repo the daemon has never heard of, and
// the instances.json read has no bearing on what the remote holds. So a remote
// target hands the bare title to the daemon, which resolves it on its own side
// (by id from memory, then by title from its own disk) — mirroring the preview
// command's remote branch above, the other apiclient-transport read.
//
// The LOCAL path is deliberately unchanged: resolveLookupScope is identical
// to resolveRepoID when no remote target is set, and the disk lookup still
// restores the instance, which is what gives the local daemon a session to
// attach to.
func resolveAttachTarget(title string) (string, string, error) {
	scope, err := resolveLookupScope()
	if err != nil {
		return "", "", err
	}
	if apiclient.IsRemoteTarget() {
		return title, scope.RepoID, nil
	}
	instance, _, err := findLiveInstanceByTitleInScope(scope.RepoID, title)
	if err != nil {
		return "", "", scope.nameMiss(title, err)
	}
	return instance.Title, scope.RepoID, nil
}
