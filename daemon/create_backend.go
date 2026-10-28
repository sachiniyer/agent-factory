package daemon

import "github.com/sachiniyer/agent-factory/session"

// The create's backend/target decision lives HERE and nowhere else (#5778).
//
// It used to be re-derived three times from partial views of the request — the
// reservation, the launch, and validate-before-reserve — and each round of fixes
// (#5608, #5743/#5744, #5778) found a copy that had dropped a field: validate
// read only Backend, so a remote `--host` create in a local-default repo demanded
// tmux and the agent on the DAEMON host for a session that runs on the ssh host.
//
// backendSelection is the full set of request fields session.resolveBackendKind
// reads: an explicit Backend, a listed Host, the legacy ForceRemote selector and
// InPlace (the in-place/off-box contradiction). Every consumer takes it from
// this method, so a new selector added to the request is added once.
func (r CreateSessionRequest) backendSelection() session.InstanceOptions {
	return session.InstanceOptions{
		Backend:     session.BackendKind(r.Backend),
		Host:        r.Host,
		ForceRemote: r.ForceRemote,
		InPlace:     r.InPlace,
	}
}

// applyBackendSelection stamps the selection onto the options a launch builds,
// so NewInstance resolves the backend from exactly what validation resolved.
func (r CreateSessionRequest) applyBackendSelection(opts *session.InstanceOptions) {
	sel := r.backendSelection()
	opts.Backend, opts.Host, opts.ForceRemote, opts.InPlace = sel.Backend, sel.Host, sel.ForceRemote, sel.InPlace
}

// createBackendKind is the runtime this request will launch on, against the
// workspace it resolves to.
func createBackendKind(req CreateSessionRequest, workspace string) (session.BackendKind, error) {
	return backendKindForCreate(req.backendSelection(), workspace)
}

// createNeedsDaemonHostPrereqs reports whether the session will run its agent on
// the daemon's own host — the only case where tmux and the agent binary on the
// daemon's PATH decide the create. A docker/ssh/hook create runs both inside its
// sandbox or on its listed host, so the daemon host is not checked for them.
func createNeedsDaemonHostPrereqs(req CreateSessionRequest, workspace string) (bool, error) {
	kind, err := createBackendKind(req, workspace)
	if err != nil {
		return false, err
	}
	return kind == session.BackendLocal, nil
}
