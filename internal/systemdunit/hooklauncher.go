package systemdunit

// HookLauncher is a live process that is ABOUT to register a hook scope and has
// not managed to yet — a `systemd-run` between its own execve and the reply to
// its StartTransientUnit call (#3667).
//
// For that interval the scope does not exist, so a successor daemon sweeping by
// unit prefix gets an empty answer from the manager while a hook is very much
// still coming. Unit is the name the launcher is about to claim, read out of its
// own argv, which is the only handle anything has on it in that window: its
// parent daemon may already be gone, so ancestry says nothing.
type HookLauncher struct {
	PID  int
	Unit string
}
