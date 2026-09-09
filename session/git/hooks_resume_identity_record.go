package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	hookWorktreeIdentityMarker = "agent-factory-checkout-id"
	hookWorktreeIdentityBytes  = 32
)

// hookWorktreeIdentity is a random identity created once in the checkout's Git
// administrative directory. Git deletes that directory when it removes a
// linked worktree, and deleting a main checkout deletes its .git directory. A
// later checkout at the same pathname therefore starts without this marker and
// receives a new random value; it cannot reproduce the journal's identity by
// reusing a filesystem inode or a worktree registration name.
//
// Device and Inode only decode journals written before the token identity was
// introduced. They are deliberately ignored: filesystem allocators may reuse
// both values, so those journals remain pending-unknown.
type hookWorktreeIdentity struct {
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	Token  string `json:"token,omitempty"`
}

func (identity *hookWorktreeIdentity) same(other *hookWorktreeIdentity) bool {
	return identity != nil && other != nil && validHookWorktreeIdentityToken(identity.Token) && identity.Token == other.Token
}

// recordHookWorktreeIdentity establishes the positive resume identity before
// publication. Its caller treats every error as an identity-free journal so a
// recovery convenience can never veto the original operator-requested hooks.
func recordHookWorktreeIdentity(repoPath, worktree string) (*hookWorktreeIdentity, error) {
	dir, err := hookWorktreeIdentityDirectory(repoPath, worktree, true)
	if err != nil {
		return nil, err
	}
	marker := filepath.Join(dir, hookWorktreeIdentityMarker)
	if identity, err := readHookWorktreeIdentityMarker(marker); err == nil {
		return identity, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	random := make([]byte, hookWorktreeIdentityBytes)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate hook worktree identity: %w", err)
	}
	token := hex.EncodeToString(random)
	temporary, err := os.CreateTemp(dir, ".agent-factory-checkout-id-")
	if err != nil {
		return nil, fmt.Errorf("create hook worktree identity temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err = temporary.Chmod(0600)
	if err == nil {
		_, err = fmt.Fprintln(temporary, token)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return nil, fmt.Errorf("write hook worktree identity: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close hook worktree identity: %w", closeErr)
	}
	// Link publishes a fully written value without replacing a token another
	// publisher already established for this checkout.
	if err := os.Link(temporaryPath, marker); err != nil {
		if os.IsExist(err) {
			return readHookWorktreeIdentityMarker(marker)
		}
		return nil, fmt.Errorf("publish hook worktree identity: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open hook worktree identity directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return nil, fmt.Errorf("sync hook worktree identity directory: %w", syncErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close hook worktree identity directory: %w", closeErr)
	}
	return &hookWorktreeIdentity{Token: token}, nil
}

type hookWorktreeIdentityFlight struct {
	done     chan struct{}
	identity *hookWorktreeIdentity
	err      error
	timedOut bool
}

var hookWorktreeIdentityFlights = struct {
	sync.Mutex
	byCheckout map[string]*hookWorktreeIdentityFlight
}{byCheckout: make(map[string]*hookWorktreeIdentityFlight)}

// boundedRecordHookWorktreeIdentity contains every metadata read and marker
// write behind one deadline. A stalled repository may cost future resume, but
// it cannot stop the original provisioning list from being published and run.
func boundedRecordHookWorktreeIdentity(repoPath, worktree string) (*hookWorktreeIdentity, error) {
	key := repoPath + "\x00" + worktree
	hookWorktreeIdentityFlights.Lock()
	if active := hookWorktreeIdentityFlights.byCheckout[key]; active != nil {
		if active.timedOut {
			hookWorktreeIdentityFlights.Unlock()
			return nil, fmt.Errorf("checkout identity creation for %s is still running after an earlier deadline: %w", worktree, context.DeadlineExceeded)
		}
		hookWorktreeIdentityFlights.Unlock()
		return waitForHookWorktreeIdentity(key, worktree, active)
	}
	flight := &hookWorktreeIdentityFlight{done: make(chan struct{})}
	hookWorktreeIdentityFlights.byCheckout[key] = flight
	hookWorktreeIdentityFlights.Unlock()
	go func() {
		flight.identity, flight.err = recordHookWorktreeIdentity(repoPath, worktree)
		hookWorktreeIdentityFlights.Lock()
		if hookWorktreeIdentityFlights.byCheckout[key] == flight {
			delete(hookWorktreeIdentityFlights.byCheckout, key)
		}
		close(flight.done)
		hookWorktreeIdentityFlights.Unlock()
	}()
	return waitForHookWorktreeIdentity(key, worktree, flight)
}

func waitForHookWorktreeIdentity(key, worktree string, flight *hookWorktreeIdentityFlight) (*hookWorktreeIdentity, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.identity, flight.err
	case <-timer.C:
		hookWorktreeIdentityFlights.Lock()
		if hookWorktreeIdentityFlights.byCheckout[key] == flight {
			flight.timedOut = true
			hookWorktreeIdentityFlights.Unlock()
			return nil, fmt.Errorf("timed out after %s while recording checkout identity under %s: %w", relocationIdentityTimeout, worktree, context.DeadlineExceeded)
		}
		hookWorktreeIdentityFlights.Unlock()
		<-flight.done
		return flight.identity, flight.err
	}
}

func readHookWorktreeIdentity(worktree string) (*hookWorktreeIdentity, error) {
	dir, err := hookWorktreeIdentityDirectory("", worktree, false)
	if err != nil {
		return nil, err
	}
	return readHookWorktreeIdentityMarker(filepath.Join(dir, hookWorktreeIdentityMarker))
}

func hookWorktreeIdentityDirectory(repoPath, worktree string, verifyOwner bool) (string, error) {
	dotGit := filepath.Join(worktree, ".git")
	info, err := BoundedLstat(dotGit)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		if verifyOwner {
			metadata, metadataErr := resolveRepoMetadataRoot(repoPath)
			if metadataErr != nil {
				return "", metadataErr
			}
			metadataInfo, metadataErr := BoundedLstat(metadata)
			if metadataErr != nil {
				return "", metadataErr
			}
			if !os.SameFile(info, metadataInfo) {
				return "", fmt.Errorf("main checkout .git directory does not belong to the hook repository")
			}
		}
		return dotGit, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("worktree .git node is not a regular file or directory")
	}
	if verifyOwner {
		if err := VerifyRegisteredWorktreeOccupant(worktree, repoPath); err != nil {
			return "", err
		}
	}
	return verifyWorktreePointerShape(worktree)
}

func readHookWorktreeIdentityMarker(path string) (*hookWorktreeIdentity, error) {
	info, err := BoundedLstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("hook worktree identity marker is not a regular file")
	}
	data, err := BoundedReadFile(path)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSuffix(string(data), "\n")
	if !validHookWorktreeIdentityToken(token) {
		return nil, fmt.Errorf("hook worktree identity marker is malformed")
	}
	return &hookWorktreeIdentity{Token: token}, nil
}

func validHookWorktreeIdentityToken(token string) bool {
	if len(token) != hookWorktreeIdentityBytes*2 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}
