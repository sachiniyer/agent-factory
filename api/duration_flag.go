package api

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// ageDurationFlag binds a time.Duration flag whose input accepts the day/week
// units of session.ParseAgeDuration — pflag's own DurationVar parses only Go
// units, so "--max-age 7d" was refused where prune's age flags should take it
// (#6770). Type() stays "duration" so generated docs and shells see the same
// value kind as before.
type ageDurationFlag struct{ target *time.Duration }

func (f ageDurationFlag) String() string { return f.target.String() }

func (f ageDurationFlag) Set(value string) error {
	d, err := session.ParseAgeDuration(value)
	if err != nil {
		return err
	}
	*f.target = d
	return nil
}

func (f ageDurationFlag) Type() string { return "duration" }
