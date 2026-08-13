#!/usr/bin/env bash
set -euo pipefail

# #3293 acceptance: the one-shot automatic redelivery keeps its boundary on
# REAL tmux panes. The unverified-outcome guard proves a prompt that delivered
# while reporting sent-unverified is never redelivered (a wrong retry shows up
# as the receiver logging the instruction twice), and the strand-probe pair
# proves the clear-observe property the retry's safety is built on: the
# pre-paste clear drains a stranded draft without eating a healthy prompt.
cd /src
go test ./session/tmux \
  -run '^(TestSentUnverifiedRealPaneDeliversExactlyOnce|TestStrandedDraftDoesNotConcatenateWithNextPrompt|TestDeliveryStillSucceedsWithNoStrandedDraft)$' \
  -count=1 -v
echo "PASS: #3293 redelivery boundary and clear-observe idempotency verified against real tmux panes on an isolated socket"
