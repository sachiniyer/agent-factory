package daemon

import "fmt"

// unscopedInstanceMissError is the none-held miss for a title looked up with
// no repo scope — the request carried no RepoID, so the search already covered
// every project the daemon holds (#6882). A bare "not found" reads as "the
// session is gone" when the truth is "no project holds it"; the remedies are
// the two ways to give the next request a scope. It is phrased in scope terms
// rather than cwd terms because the daemon cannot see the client's working
// directory: a remote request arrives with no RepoID for the same reason a
// local outside-a-repo one does. Unwrap keeps errSessionNotFound reachable for
// the errors.Is checks that classify a clean miss (deleteproject's idempotent
// delete, the task target fence).
type unscopedInstanceMissError struct{ title string }

func (e unscopedInstanceMissError) Error() string {
	return fmt.Sprintf("instance %q is held by no project — the request carried no repo scope, so every project was searched; pass --repo <path> to scope it, or run inside a project's repository", e.title)
}

func (e unscopedInstanceMissError) Unwrap() error { return errSessionNotFound }

// scopedInstanceMissError is the clean miss for a title looked up inside one
// repo scope: the request carried a RepoID and that project holds no row with
// the title. Unwrap keeps errSessionNotFound reachable for the same errors.Is
// callers the unscoped miss serves.
type scopedInstanceMissError struct{ title string }

func (e scopedInstanceMissError) Error() string { return ScopedInstanceMissText(e.title) }

func (e scopedInstanceMissError) Unwrap() error { return errSessionNotFound }

// ScopedInstanceMissText is the scoped miss's text, and it is exported because
// on the wire the text is the whole error: net/rpc and the HTTP envelope carry
// only the message, so no sentinel reaches a client. The CLI matches on it to
// name the project it scoped the request to (#7092), which the daemon cannot
// do itself. It sees an opaque repo ID, not the path the user would recognize,
// and not whether --repo or the current directory chose it. Changing this text
// stops clients built against it from recognizing the miss, so they fall back
// to printing it bare.
func ScopedInstanceMissText(title string) string {
	return fmt.Sprintf("instance %q not found", title)
}
