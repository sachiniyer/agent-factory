package apiproto

import "errors"

// ErrorCodeDaemonRestarting says the daemon refused the request, without
// running it, because it is restarting: whoever stopped it starts its
// successor (`af upgrade`, `af daemon restart`, `af daemon adopt`, the CLI
// auto-update, a `systemctl --user restart` of its unit). A client retries the
// request into that successor (#7474).
const ErrorCodeDaemonRestarting = "daemon_restarting"

// LifecycleRefusal is a request the daemon refused, without running it,
// because of the state its lifecycle is in. Code names that state, and a client
// classifies the refusal by Code, never by Message, so the wording can change
// without changing what a client retries (#7474). Message is the daemon's own
// text, for the user.
type LifecycleRefusal struct {
	Code    string
	Message string
}

func (e *LifecycleRefusal) Error() string { return e.Message }

// IsLifecycleRefusal reports whether err carries a lifecycle refusal with the
// given code anywhere in its chain.
func IsLifecycleRefusal(err error, code string) bool {
	var refusal *LifecycleRefusal
	return errors.As(err, &refusal) && refusal.Code == code
}
