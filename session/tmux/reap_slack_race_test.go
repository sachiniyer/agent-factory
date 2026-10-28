//go:build race

package tmux

// reapTestSlack allows scheduler and process-snapshot overhead on loaded runners.
const reapTestSlack = 10
