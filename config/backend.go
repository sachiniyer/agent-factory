package config

// Backend selection (#1592 Phase 4 PR3). A repo declares which runtime its
// sessions run on via the in-repo `backend` key, alongside the docker/ssh
// sections that parameterize the two sandboxed runtimes. The canonical values
// are the four registered runtimes; validation of the VALUE lives at
// backend-resolution time in the session package (mirroring how RemoteHooks are
// validated when a backend is resolved, not at config load), so the config
// layer only carries the raw strings and structs.
//
// The four canonical `backend` values. `local` (or empty) is the default —
// today's in-process tmux+worktree runtime, unchanged. `hook` is the existing
// remote-hook backend. `docker` and `ssh` are the first-class sandboxed
// runtimes (implemented in Phase 4 PR4/PR5).
const (
	BackendLocal  = "local"
	BackendDocker = "docker"
	BackendSSH    = "ssh"
	BackendHook   = "hook"
)

// DockerConfig parameterizes the docker runtime (#1592 Phase 4 PR3 config
// surface; the runtime that consumes it lands in PR4). A session with
// `backend = "docker"` runs its workspace + agent in a container started from
// Image, with RunArgs appended to the `docker run` invocation.
type DockerConfig struct {
	// Image is the container image the session's sandbox is started from. It
	// must carry git + the agent CLIs + the `af` binary (or have `af` copied in
	// per the BYO-image recipe). Required when the docker runtime is selected;
	// the requirement is enforced by the runtime, not at config load.
	Image string `json:"image,omitempty" toml:"image,omitempty"`
	// RunArgs are extra arguments appended verbatim to `docker run` (e.g. extra
	// mounts, env, or resource limits). Optional.
	RunArgs []string `json:"run_args,omitempty" toml:"run_args,omitempty"`
}

// SSHConfig parameterizes the ssh runtime (#1592 Phase 4 PR3 config surface; the
// runtime that consumes it lands in PR5). A session with `backend = "ssh"` runs
// its workspace + agent on Host over an ssh connection, with the agent-server
// reached through a local-forward tunnel.
type SSHConfig struct {
	// Host is the ssh target (`host` or `host:port`) the session is provisioned
	// on. Required when the ssh runtime is selected; enforced by the runtime.
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	// User is the ssh login user. Optional — empty defers to the ssh client's
	// default (the current user or an ssh_config Match).
	User string `json:"user,omitempty" toml:"user,omitempty"`
	// Port is the ssh port. Optional — 0 means the default (22).
	Port int `json:"port,omitempty" toml:"port,omitempty"`
	// IdentityFile is the path to the private key used for auth. Optional —
	// empty defers to the agent/default keys.
	IdentityFile string `json:"identity_file,omitempty" toml:"identity_file,omitempty"`
	// KnownHosts is the path to the OpenSSH known_hosts file the runtime verifies
	// the remote's host key against (#1592 Phase 4 PR5). Optional — empty defers
	// to the user's ~/.ssh/known_hosts. Host-key verification is always on
	// (secure by default, mirroring the TLS TOFU pin on the agent-server); an
	// unknown or changed host key fails the connection with an actionable error.
	// Point this at a dedicated file for ephemeral/CI hosts whose keys you seed
	// out of band.
	KnownHosts string `json:"known_hosts,omitempty" toml:"known_hosts,omitempty"`
}
