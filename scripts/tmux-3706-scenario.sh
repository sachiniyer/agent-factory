#!/usr/bin/env bash
set -euo pipefail

# #3706 acceptance: the blind vanished-session refusal must say something an
# operator can act on, WITHOUT telling them to kill a possibly-legitimate
# same-name replacement.
#
# Driven against REAL tmux panes on an isolated socket, because the property is
# about what a genuine vanished-session sweep produces: a marked process that
# outlives its tmux session, a sweep with no captured predecessor to place its
# generation against, and the message that refusal leaves behind. The -v output
# prints that message verbatim (see the t.Logf in the first test) so the
# play-test report can quote the operator-facing text rather than assert about it.
#
# The self-process case is the safety half: refreshOrphanCandidates walks the
# whole process table for AF_SESSION matches with no self-exclusion, so the sweep
# can find its OWN process carrying the vanished session's markers. It must
# refuse without ever offering that pid as safe to kill.
cd /src
go test ./session/tmux \
  -run '^(TestBlindVanishedSessionRefusalTellsOperatorHowToTell|TestBlindGenerationRefusalNamesEveryPidAndItsGeneration|TestBlindGenerationRefusalKeepsPercentInSessionName|TestBlindRefusalDoesNotOfferTheSweepsOwnProcessAsSafeToKill|TestBlindVanishedSessionSweepDoesNotAdoptReplacementGeneration)$' \
  -count=1 -v
echo "PASS: #3706 blind vanished-session refusal names its unplaceable pids and generations, gives the live-session comparison, and never offers the sweep's own process as safe to kill — verified against real tmux panes on an isolated socket"
