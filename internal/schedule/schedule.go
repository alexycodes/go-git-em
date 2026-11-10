// Package schedule decides when backups run. It is the only package that uses
// robfig/cron, so the cron library can be replaced in one place.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule says when a repository is due.
type Schedule interface {
	// Next returns the first time after t when the schedule is due.
	Next(t time.Time) time.Time
}

// Parse parses a schedule: a standard 5-field cron expression such as
// "0 3 * * *", or a descriptor such as "@daily" or "@every 6h". A
// "CRON_TZ=<zone> " prefix sets the time zone; the default is the local one.
//
// Schedules that robfig/cron would quietly change or never run are errors:
// "@every" needs a whole number of seconds, at least one, and a cron
// expression must match some date.
func Parse(spec string) (s Schedule, err error) {
	// robfig/cron v3.0.1 panics on some input it doesn't expect.
	defer func() {
		if r := recover(); r != nil {
			s, err = nil, errors.New("not a valid schedule")
		}
	}()

	rest := spec
	if strings.HasPrefix(rest, "TZ=") || strings.HasPrefix(rest, "CRON_TZ=") {
		_, after, ok := strings.Cut(rest, " ")
		if !ok {
			return nil, errors.New(`a time zone needs a schedule after it, such as "CRON_TZ=UTC 0 3 * * *"`)
		}
		rest = strings.TrimSpace(after)
	}
	// robfig/cron rounds anything under a second, zero and negative
	// durations included, up to one second, and drops fractions of a second.
	if d, ok := strings.CutPrefix(rest, "@every "); ok {
		if every, err := time.ParseDuration(d); err == nil && (every < time.Second || every%time.Second != 0) {
			return nil, fmt.Errorf("@every needs a whole number of seconds, at least 1s, got %v", every)
		}
	}

	cs, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, err
	}
	if cs.Next(time.Now()).IsZero() {
		return nil, errors.New("never fires: no date matches it")
	}
	return cs, nil
}

// Run calls due with the indices of the schedules that are due, in order, each
// time some are, from their first times after start until ctx is done. The
// calls are made one at a time, from Run's goroutine: a schedule that falls
// due more than once during a call is due once when it returns. A schedule
// whose next time is the zero time, as cron gives when it finds none within 5
// years, isn't due again unless the clock is set back.
//
// cron's own runner isn't used: it starts each due entry in a goroutine of its
// own, so entries due at the same time would come in no particular order.
func Run(ctx context.Context, scheds []Schedule, start time.Time, due func([]int)) {
	// Times are on the wall clock alone: Round(0) drops the monotonic reading,
	// which stops while the host sleeps. Timers stop then too, and miss clock
	// changes, so Run reads the clock at least once a minute. A schedule is
	// then at most a minute late, and an @every one also by as much as the
	// clock went back.
	last := start.Round(0)
	next := make([]time.Time, len(scheds))
	for i, s := range scheds {
		next[i] = s.Next(last).Round(0)
	}
	for {
		wait, now := time.Minute, clock()
		for _, t := range next {
			if !t.IsZero() {
				wait = min(wait, t.Sub(now))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if ctx.Err() != nil { // both were ready
			return
		}
		now = clock().Round(0)
		var dueNow []int
		for i, s := range scheds {
			switch {
			case now.Before(last):
				// The clock went back: take the earlier time, so no cron time in
				// between is skipped, an @every schedule doesn't wait a whole
				// interval again, and one that had no time left can come back.
				if n := s.Next(now).Round(0); !n.IsZero() && (next[i].IsZero() || n.Before(next[i])) {
					next[i] = n
				}
			case next[i].IsZero():
			case !next[i].After(now):
				dueNow = append(dueNow, i)
				next[i] = s.Next(now).Round(0)
			}
		}
		last = now
		if dueNow != nil {
			due(dueNow)
		}
	}
}

// clock reads the wall clock. Tests move it, as a clock change would.
var clock = time.Now
