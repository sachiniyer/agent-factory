//go:build linux

package git

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookWorktreeIdentityDoesNotRecycleInode(t *testing.T) {
	original := &hookWorktreeIdentity{Device: 7, Inode: 11, Token: strings.Repeat("a", hookWorktreeIdentityBytes*2)}
	replacement := &hookWorktreeIdentity{Device: 7, Inode: 11, Token: strings.Repeat("b", hookWorktreeIdentityBytes*2)}
	if original.same(replacement) {
		t.Fatal("a replacement checkout with a recycled inode matched the original identity")
	}
	sameCheckout := &hookWorktreeIdentity{Device: 99, Inode: 101, Token: original.Token}
	if !original.same(sameCheckout) {
		t.Fatal("the durable checkout token did not authorize its own journal")
	}
}

func TestHookProgressOwnerScanDoesNotHoldPublicationLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	originalLoad := hookProgressOwnerLoad
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce, workerDoneOnce sync.Once
	hookProgressOwnerLoad = func() (map[string]json.RawMessage, []config.RepoInstancesSkip, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		workerDoneOnce.Do(func() { close(workerDone) })
		return originalLoad()
	}
	publisherDone := make(chan error, 1)
	publisherJoined := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		<-workerDone
		if !publisherJoined {
			<-publisherDone
		}
		deadline := time.Now().Add(time.Second)
		for {
			hookProgressOwnerFlights.Lock()
			active := hookProgressOwnerFlights.byHome[home]
			hookProgressOwnerFlights.Unlock()
			if active == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("hook progress owner flight did not drain")
			}
			time.Sleep(time.Millisecond)
		}
		hookProgressOwnerLoad = originalLoad
		relocationIdentityTimeout = previousTimeout
	})
	go func() {
		_, publishErr := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
		publisherDone <- publishErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("hook progress owner scan did not start")
	}
	acquired, lockErr := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if lockErr != nil || !acquired {
		t.Fatalf("owner scan retained the publication lock: acquired=%v err=%v", acquired, lockErr)
	}
	select {
	case publishErr := <-publisherDone:
		publisherJoined = true
		if publishErr != nil {
			t.Fatalf("inconclusive owner scan blocked journal publication: %v", publishErr)
		}
	case <-time.After(5 * relocationIdentityTimeout):
		t.Fatal("stalled instances file wedged journal publication past the shared deadline")
	}
	releaseOnce.Do(func() { close(release) })
}
