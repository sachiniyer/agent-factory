package session

// SetOnSandboxRetired registers a one-shot hook that reprovisionRemote will
// call after reapRemoteRuntimeForReplacement returns without error — the moment
// the old sandbox is provably gone. The hook fires at most once per registration
// and is cleared after firing. A nil hook clears any existing registration.
func (i *Instance) SetOnSandboxRetired(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onSandboxRetired = fn
}

// takeOnSandboxRetired atomically retrieves and clears the one-shot hook
// registered by SetOnSandboxRetired. Returns nil if none is registered.
// Called by reprovisionRemote immediately after the old sandbox is retired.
func (i *Instance) takeOnSandboxRetired() func() {
	i.mu.Lock()
	defer i.mu.Unlock()
	fn := i.onSandboxRetired
	i.onSandboxRetired = nil
	return fn
}

// FireOnSandboxRetired calls the hook registered by SetOnSandboxRetired (if
// any), then clears it. It is the exported counterpart of takeOnSandboxRetired,
// intended for use by backend implementations (including test fakes) that
// perform sandbox retirement themselves rather than going through
// reprovisionRemote. Calling it is a semantic claim: "the predecessor sandbox
// is provably gone."
func (i *Instance) FireOnSandboxRetired() {
	if fn := i.takeOnSandboxRetired(); fn != nil {
		fn()
	}
}
