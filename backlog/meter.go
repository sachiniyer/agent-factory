package backlog

import (
	"fmt"
	"maps"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

// UnmeterableAfter is how long a started run may have no transcript before it
// counts as unmeterable.
const UnmeterableAfter = 2 * time.Minute

// RunMeter is one metering of a run's transcripts.
type RunMeter struct {
	Meter   map[string]Meter
	Changed bool
	// Unmeterable says why af cannot meter the run; "" when it can.
	Unmeterable string
	// Entries are the messages this pass counted, for time buckets.
	Entries []planusage.Entry
	// FileEntries are the same messages by the transcript file they came from, so a
	// caller can attribute each to the account that incurred it.
	FileEntries map[string][]planusage.Entry
}

// Cost sums the run's per-file costs.
func (m RunMeter) Cost() float64 { return RunRef{Meter: m.Meter}.Cost() }

// MeterRun reads the run's transcript files from their stored offsets. seen
// holds the keys of the run's messages counted so far, from any of its files,
// and gains what this pass counts; a message Claude Code copied into a new
// transcript (resume, fork) counts once. A run with no files is unmeterable
// once it has run for UnmeterableAfter.
func MeterRun(id int, r RunRef, files []string, seen map[string]int64, overrides map[string]planusage.Price, now time.Time) RunMeter {
	out := RunMeter{Meter: maps.Clone(r.Meter)}
	if out.Meter == nil {
		out.Meter = map[string]Meter{}
	}
	for _, f := range files {
		old := out.Meter[f]
		res, err := planusage.ReadTranscript(f, planusage.FileMark{Offset: old.Offset, Last: old.Last}, planusage.ReadOptions{Overrides: overrides, Seen: seen})
		out.Entries = append(out.Entries, res.Entries...)
		if len(res.Entries) > 0 {
			if out.FileEntries == nil {
				out.FileEntries = map[string][]planusage.Entry{}
			}
			out.FileEntries[f] = append(out.FileEntries[f], res.Entries...)
		}
		next := Meter{Offset: res.Mark.Offset, Cost: old.Cost + res.Cost(), Last: res.Mark.Last}
		if _, known := r.Meter[f]; next != old || !known {
			out.Meter[f], out.Changed = next, true
		}
		switch {
		case res.Unpriced != "":
			out.Unmeterable = fmt.Sprintf("no price for model %s; add [backlog.prices.%q] to config.toml", res.Unpriced, res.Unpriced)
		case res.Bad && out.Unmeterable == "":
			out.Unmeterable = fmt.Sprintf("af cannot read a transcript of %s's session (%s); start %s by hand with \"af backlog start %d\"", FormatID(id), f, FormatID(id), id)
		case err != nil && out.Unmeterable == "" && res.Mark.Offset == old.Offset:
			out.Unmeterable = fmt.Sprintf("af cannot read a transcript of %s's session: %v", FormatID(id), err)
		}
	}
	if len(files) == 0 && now.Sub(r.StartedAt) >= UnmeterableAfter {
		out.Unmeterable = fmt.Sprintf("af cannot find the transcripts of %s's session; start %s by hand with \"af backlog start %d\"", FormatID(id), FormatID(id), id)
	}
	return out
}

// UnmeterableProgram is why a run of a program other than Claude cannot be
// metered.
func UnmeterableProgram(id int, program string) string {
	return fmt.Sprintf("the runner can't meter %s; start %s by hand with \"af backlog start %d\"", program, FormatID(id), id)
}

// SetRunMeters stores per-file meters on the runs of the given sessions. It
// returns whether anything changed.
func (b *Backlog) SetRunMeters(meters map[string]map[string]Meter) bool {
	changed := false
	for i := range b.Items {
		for j := range b.Items[i].Runs {
			r := &b.Items[i].Runs[j]
			if m, ok := meters[r.SessionID]; ok && r.SessionID != "" && !maps.Equal(r.Meter, m) {
				r.Meter = maps.Clone(m)
				changed = true
			}
		}
	}
	return changed
}
