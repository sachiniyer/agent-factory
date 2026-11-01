package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// lsRemoteBranch asks originURL for refs/heads/<branch> with the sandbox
// archive's probe (lsRemoteArgs: the environment-backed GitHub credential, no
// terminal prompt), within timeout. found is true when origin answered with the
// ref, false only on a positive "origin lacks it"; anything else (no URL,
// unreachable, auth) is an error. out is what ls-remote printed.
func lsRemoteBranch(originURL, branch string, timeout time.Duration) (out []byte, found bool, err error) {
	if strings.TrimSpace(originURL) == "" {
		return nil, false, errors.New("the repository has no origin remote")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", lsRemoteArgs(originURL, branch)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = dockerWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err == nil {
		return out, true, nil
	}
	var exit *exec.ExitError
	// --exit-code makes 2 mean "reached origin, no matching ref"; any other
	// failure says nothing about the ref.
	if errors.As(err, &exit) && exit.ExitCode() == 2 && ctx.Err() == nil {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(stderr.String()+string(out)))
}

// OriginBranchHead is the commit refs/heads/<branch> names on the origin of the
// repository at repoRoot: "" with a nil error when origin has no such branch,
// and an error when af could not ask. It asks within timeout. A var so tests
// need no remote.
var OriginBranchHead = func(repoRoot, branch string, timeout time.Duration) (string, error) {
	out, found, err := lsRemoteBranch(originRemoteURL(repoRoot), branch, timeout)
	if err != nil || !found {
		return "", err
	}
	ref := "refs/heads/" + branch
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
			return f[0], nil
		}
	}
	// ls-remote matches a pattern by its tail, so a hit can be another ref
	// ending in this one: origin has no branch by exactly this name.
	return "", nil
}
