#!/usr/bin/env bash
# Driver transport and screen predicates, confined to the SAME 1000-row daemon.
set -euo pipefail
[ -f /.dockerenv ] || [ -f /run/.containerenv ] || exit 1
[ "$AGENT_FACTORY_HOME" = /work/demo-home/.agent-factory ] || exit 1
export AF_DRIVER_BIN=/work/bin/af AF_DRIVER_REPO=/work/todo-cli
export AF_DRIVER_SESSION=perf-drive AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
export AF_DRIVER_POLL=0.005
export AF_DRIVER_LAUNCH_ENV="AGENT_FACTORY_HOME=$AGENT_FACTORY_HOME AGENT_FACTORY_AUTO_UPDATE=false"
source /work/scripts/tui-driver.sh
# Startup is outside the measured intervals. All measurement completion uses
# the rendered footer, not tmux's acknowledgement that send-keys succeeded.
af_boot
af_wait_for 'Sessions \(1000\)' 60 '1000 sessions loaded in the TUI'
node /work/scripts/perf/tui.mjs
