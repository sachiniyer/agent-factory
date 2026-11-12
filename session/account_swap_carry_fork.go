package session

import (
	"bytes"
	"encoding/json"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Codex's /fork of a paginated thread — the default history mode of its local
// thread store — goes through thread/fork → prepare_fork (codex-rs
// thread-store local/paginated_fork.rs). The fork gets its own thread id and a
// plain-named rollout, rollout-<ts>-<fork>.jsonl, which holds only the turns
// after the fork. Its session_meta.history_base names the parent rollout's
// prefix by rollout id, and `codex resume <fork>` reads those turns from the
// parent's file (local/rollout_lineage.rs), failing with "missing source
// rollout" when it is absent. A carry copies one file, so it cannot take such
// a thread; it starts fresh with a stated reason instead (#7153).
//
// A plain-named rollout's history_base always reaches another rollout: its
// rollout id is its own thread id, and a base naming itself is a cycle Codex
// refuses. Older Codex, and legacy-history threads, write a fork whole, with
// no history_base; those carry as before.

// codexForkCarryReason is why a carry does not take a paginated fork.
const codexForkCarryReason = "it was forked from another Codex conversation, and af does not carry the history a fork shares with its parent yet"

// codexRolloutHistoryBase reports whether the rollout at path continues
// another rollout's prefix: its session_meta header sets history_base. A
// first line that is not a session_meta record is no fork — Codex cannot
// resume that file either — but a file that cannot be read is an error.
func codexRolloutHistoryBase(path string) (bool, error) {
	line, err := readCodexRolloutFirstLine(path)
	if err != nil {
		return false, err
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			HistoryBase json.RawMessage `json:"history_base"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return false, nil
	}
	base := bytes.TrimSpace(meta.Payload.HistoryBase)
	return len(base) > 0 && !bytes.Equal(base, []byte("null")), nil
}

// codexForkReason is why this carry cannot take its thread because the rollout
// it copies is a paginated fork's, or "". Only a rollout copied from the
// source is judged: when the carry takes the new account's own file — a
// committed carry whose replacement forked there — the parent is the carried
// thread, already in that account.
func (c *conversationCarry) codexForkReason(fromSource bool) string {
	if c.agent != tmux.ProgramCodex || !fromSource {
		return ""
	}
	based, err := codexRolloutHistoryBase(filepath.Join(c.srcHome, c.transcript))
	switch {
	case err != nil:
		return "af could not read its rollout's session metadata to confirm the rollout holds the whole conversation"
	case based:
		return codexForkCarryReason
	}
	return ""
}
