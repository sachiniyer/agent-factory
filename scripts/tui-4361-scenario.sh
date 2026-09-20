#!/usr/bin/env bash
# Real-TUI drive for #4361: the config overlay's Usage section — the daemon's
# usage-limit report rendered between the config tiers and Accounts — plus the
# same report through `af quota` (local read), POST /v1/QuotaReport on the
# daemon's loopback listener, and `af quota --daemon-url` at a dead target
# (a remote read that must REFUSE rather than fall back to this machine).
#
# Runs inside the #1130 container sandbox:
#   scripts/testbox.sh scenario scripts/tui-4361-scenario.sh
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=110 AF_DRIVER_ROWS=44
BIN="$HOME/bin/af"

af_reset_sandbox

# Config lands BEFORE the first boot so the daemon the TUI claims already has
# the loopback HTTP listener bound when the scenario reaches for it.
cat > "$AGENT_FACTORY_HOME/config.toml" <<'EOF'
[program_overrides]
claude = 'bash'
[network]
listen_addr = '127.0.0.1:8899'
EOF

af_boot

# --- TUI: the Usage section renders inside the config overlay ----------------
af_open_config
af_assert_screen 'Usage' 'Usage section heading in the config overlay'
af_assert_screen 'QUOTA is what the provider reports' 'the QUOTA/OBSERVED framing note'
af_assert_screen 'not reported' 'provider entitlement column reads not reported'

# The section sits between the config tiers and Accounts and must scroll: walk
# the cursor down until Accounts enters the viewport, proving both order and
# reachability of a section taller than the window.
deadline=$(( $(date +%s) + ${AF_DRIVER_TIMEOUT:-20} ))
while :; do
	af_capture | grep -q 'Accounts' && break
	af_send j
	[ "$(date +%s)" -ge "$deadline" ] && { af_capture >&2; exit 1; }
	sleep "$AF_DRIVER_POLL"
done
af_assert_screen 'Accounts' 'Accounts section follows Usage (scroll reached it)'
af_assert_screen 'claude' 'a per-agent usage row rendered'
af_assert_screen 'no sessions' 'observed column reads no sessions'
af_close_config

# --- CLI: the same report on the same home -----------------------------------
"$BIN" quota > /tmp/quota.out 2>&1
grep -q 'QUOTA is what the provider reports' /tmp/quota.out
grep -q 'not reported' /tmp/quota.out
echo "assert OK: af quota prints the note and per-agent rows"

# --- HTTP: the daemon's own route answers the same shape ---------------------
deadline=$(( $(date +%s) + ${AF_DRIVER_TIMEOUT:-20} ))
while :; do
	body="$(curl -sf -X POST http://127.0.0.1:8899/v1/QuotaReport \
		-H 'Content-Type: application/json' -d '{}' 2>/dev/null)" && break
	[ "$(date +%s)" -ge "$deadline" ] && { echo "FAIL: /v1/QuotaReport never answered" >&2; exit 1; }
	sleep "$AF_DRIVER_POLL"
done
printf '%s' "$body" | grep -q '"rows"'
printf '%s' "$body" | grep -q '"note"'
echo "assert OK: POST /v1/QuotaReport returns the report JSON"

# --- Remote: an unreachable target refuses, never falls back -----------------
if "$BIN" quota --daemon-url http://127.0.0.1:9 >/tmp/quota-remote.out 2>&1; then
	echo "FAIL: af quota --daemon-url to a dead target exited 0" >&2
	cat /tmp/quota-remote.out >&2
	exit 1
fi
grep -qiE 'refus|cannot|daemon' /tmp/quota-remote.out
echo "assert OK: af quota --daemon-url to an unreachable target refuses without a local fallback"

af_quit
echo "PLAY-TEST PASSED: #4361 usage report — TUI section, CLI, HTTP route, remote refusal"
