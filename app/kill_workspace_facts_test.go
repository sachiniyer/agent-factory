package app

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
)

func measured(f session.WorkspaceFacts) daemon.WorkspaceFactsResponse {
	return daemon.WorkspaceFactsResponse{Measured: true, Facts: f}
}

func TestAssessWorkspaceFacts(t *testing.T) {
	t.Run("clean workspace states nothing", func(t *testing.T) {
		loss := assessWorkspaceFacts(measured(session.WorkspaceFacts{Exists: true}), nil)
		assert.Empty(t, loss.warnings)
		assert.Empty(t, loss.severe)
	})
	t.Run("unpushed commits are severe, with the distinct confirm key", func(t *testing.T) {
		loss := assessWorkspaceFacts(measured(session.WorkspaceFacts{Exists: true, UnpushedCommits: 3}), nil)
		assert.Contains(t, loss.severe, "3 commits exist only in this workspace")
	})
	t.Run("dirty files, branches and stashes accumulate warnings", func(t *testing.T) {
		loss := assessWorkspaceFacts(measured(session.WorkspaceFacts{Exists: true, DirtyFiles: 1, ExtraBranches: 2, Stashes: 1}), nil)
		assert.Empty(t, loss.severe)
		assert.Len(t, loss.warnings, 3)
		assert.Contains(t, loss.warnings[0], "1 uncommitted file")
		assert.Contains(t, loss.warnings[1], "2 other local branches")
		assert.Contains(t, loss.warnings[2], "1 stash entry")
	})
	t.Run("a missing workspace says so and is not severe", func(t *testing.T) {
		loss := assessWorkspaceFacts(measured(session.WorkspaceFacts{Exists: false}), nil)
		assert.Empty(t, loss.severe)
		assert.Contains(t, loss.warnings[0], "already missing")
	})
	t.Run("unmeasured is a fail-closed warning, never clean", func(t *testing.T) {
		for _, loss := range []killLossAssessment{
			assessWorkspaceFacts(daemon.WorkspaceFactsResponse{Reason: "old agent-server"}, nil),
			assessWorkspaceFacts(daemon.WorkspaceFactsResponse{}, errors.New("daemon down")),
		} {
			assert.Empty(t, loss.severe)
			if assert.Len(t, loss.warnings, 1) {
				assert.Contains(t, loss.warnings[0], "Could not verify the workspace")
			}
		}
	})
	t.Run("a partially verified measurement flags what it could not read", func(t *testing.T) {
		loss := assessWorkspaceFacts(measured(session.WorkspaceFacts{Exists: true, Unverified: []string{"stashes"}}), nil)
		assert.Contains(t, loss.warnings[0], "Could not verify")
	})
}
