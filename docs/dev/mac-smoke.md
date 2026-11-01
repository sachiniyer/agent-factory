# macOS smoke test

For maintainers, and for anyone with a Mac who wants to report a regression with
evidence. `scripts/mac-smoke.sh` runs the real `af` binary through its core
journey and prints a pass/fail report you can paste on
[#6384](https://github.com/sachiniyer/agent-factory/issues/6384).

## Why a script, not just CI

GitHub's hosted macOS runners have System Integrity Protection **disabled**.
Every user's Mac has it enabled. Behaviour that depends on SIP — for example
reading the environment of Apple system binaries ([#3584](https://github.com/sachiniyer/agent-factory/issues/3584)) —
is invisible to every CI job, and PR CI skips macOS entirely. This script is
how SIP-on behaviour is covered: you run it on your Mac.

## Run it

```bash
git clone https://github.com/sachiniyer/agent-factory && cd agent-factory
go build -o /tmp/af-smoke .          # or point --af at an installed binary
scripts/mac-smoke.sh --af /tmp/af-smoke
```

Needs `git`, `tmux` (3.2+, `brew install tmux`) and `curl`. It runs under the
stock `/bin/bash` 3.2. Add `--keep` to leave its throwaway directory (and
`smoke.log`) behind for inspection.

## What it touches

Nothing of your real install. Everything lives under one `mktemp` directory in
`/tmp` (kept short: unix sockets are limited to 104 bytes on macOS):

- `AGENT_FACTORY_HOME` and `HOME` point inside it — your config, state, daemon
  and `~/Library/LaunchAgents` are neither read nor written.
- tmux runs on a private server (`TMUX_TMPDIR` inside the directory, `$TMUX`
  unset) and is stopped by socket path at the end — never `kill-server` on the
  default socket.
- The web UI binds a free loopback port in 18443–18542, not 8443.
- The agent is a stand-in shell script named `claude`; no credentials are used.

It stops only the daemon and tmux server it started.

## The journey

The binary's `codesign -v` and architecture · `af version` · stand-in agent and
port configured · the daemon starts on first use and the web UI answers 200 ·
create a session · create an account-scoped session and check that its new
windows go through the launch-capability shim ([#4731](https://github.com/sachiniyer/agent-factory/issues/4731)) and run under the
account · open a terminal tab · restart the daemon and check the same window
still works · archive and restore · `af doctor --setup` and `af doctor`.

The report starts with the environment (macOS version, SIP state, bash and tmux
versions, `$TMPDIR` length). Paste the whole output.

## In CI

`.github/workflows/mac-e2e.yml` runs the same script on every push to `master`
and on `workflow_dispatch`, on `macos-latest` (arm64), `macos-15-intel`, and
`ubuntu-24.04` (not a Mac: it catches defects in the script itself). It also
installs the latest release with `install.sh` and checks its architecture and
signature, and probes whether a launchd user agent can be registered on the
runner. The jobs are report-only for now.
