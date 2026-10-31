package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOffBoxAccountNoticeStatesTheFreshConversationAndCarriesTheTask(t *testing.T) {
	n := offBoxAccountNotice("claude", "work", "alt", " after the previous identity reached its usage limit", "  fix the bug  ")
	assert.Contains(t, n, `claude account "work"`)
	assert.Contains(t, n, `claude account "alt"`)
	assert.Contains(t, n, "usage limit")
	assert.Contains(t, n, "fresh conversation", "the new agent is told its earlier messages are gone")
	assert.Contains(t, n, "stays in the previous account's store")
	assert.Contains(t, n, "\n\nfix the bug")

	assert.Contains(t, offBoxAccountNotice("claude", "work", "alt", "", ""), "\n\ncontinue")
}
