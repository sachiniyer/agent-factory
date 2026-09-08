package task

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Status polling parses unchanged banners every second. Keep diagnostic state
// separate from parsing results, with no daemon/session plumbing or disk I/O.
// FIFO eviction bounds memory to the last 64 distinct warned keys; an evicted
// banner can warn again. Concurrent callers must claim a key before logging it.
var claudeTimezoneWarnings timezoneWarningCache

type timezoneWarningCache struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order [64]string
	next  int
}

func (c *timezoneWarningCache) first(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return false
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{}, len(c.order))
	}
	delete(c.seen, c.order[c.next])
	c.seen[key] = struct{}{}
	c.order[c.next] = key
	c.next = (c.next + 1) % len(c.order)
	return true
}

func warnClaudeTimezoneOnce(content string, rejected []string, loc *time.Location) {
	banner, _, _ := strings.Cut(content, "\n")
	// Quote both fields to preserve exact text and candidate boundaries without
	// collisions from delimiters that may themselves appear in the banner.
	key := fmt.Sprintf("%q %q", rejected, banner)
	if claudeTimezoneWarnings.first(key) {
		log.WarningLog.Printf("Claude usage-limit banner %q: cannot load timezone candidates %q; falling back to daemon zone %q", banner, rejected, loc.String())
	}
}
