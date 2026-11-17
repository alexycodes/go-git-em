// Package schedule decides when backups run. It is the only package that uses
// robfig/cron, so the cron library can be replaced in one place.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule says when a repository is due.
type Schedule interface {
	// Next returns the first time after t when the schedule is due.
	Next(t time.Time) time.Time
}

// AcceptEvery makes Parse also accept "@every <duration>", in whole seconds, so
// the daemon's tests run in seconds. Only tests set it.
var AcceptEvery bool

// shortcuts are the descriptors that Vixie cron has too, with their fields.
var shortcuts = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// maxes are the highest values of the 5 fields in robfig/cron.
var maxes = [5]int{59, 23, 31, 12, 6}

// standardItem matches an item of a field's list as Vixie cron reads it: "*",
// a value or a range of values, then maybe a step. Values are numbers or names.
var standardItem = regexp.MustCompile(`^(\*|[0-9a-zA-Z]+(-[0-9a-zA-Z]+)?)(/[0-9]+)?$`)

// starBit is robfig/cron's unexported mark of a field it read as "*".
const starBit = 1 << 63

// vixieDays returns robfig/cron's bits for a day field, marked as "*" if, and
// only if, the field starts with it, as Vixie cron decides.
func vixieDays(bits uint64, field string) uint64 {
	if strings.HasPrefix(field, "*") {
		return bits | starBit
	}
	return bits &^ starBit
}

// Parse parses a schedule in the 5 fields of standard cron, such as
// "0 3 * * *", as Vixie cron reads them. It's in the time zone of the times
// given to Next: for the daemon, the process's. robfig/cron's extensions are
// errors, as is a schedule that matches no date, which robfig/cron would
// accept and never run.
func Parse(spec string) (s Schedule, err error) {
	// robfig/cron v3.0.1 panics on some input it doesn't expect.
	defer func() {
		if r := recover(); r != nil {
			s, err = nil, errors.New("not a valid schedule")
		}
	}()

	switch every, isEvery := strings.CutPrefix(spec, "@every "); {
	case strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ="):
		return nil, errors.New("a time zone prefix isn't supported: schedules follow the process's time zone")
	case strings.Contains(spec, "?"):
		return nil, errors.New(`"?" isn't supported: use "*"`)
	case isEvery && AcceptEvery:
		// robfig/cron rounds anything under a second, zero and negative
		// durations included, up to one second, and drops fractions of a second.
		if d, err := time.ParseDuration(every); err == nil && (d < time.Second || d%time.Second != 0) {
			return nil, fmt.Errorf("@every needs a whole number of seconds, at least 1s, got %v", d)
		}
	case shortcuts[spec] != "":
		return nil, fmt.Errorf("shortcuts aren't supported: write %q", shortcuts[spec])
	case strings.HasPrefix(spec, "@"):
		return nil, errors.New(`only the 5 cron fields are supported, such as "0 3 * * *"`)
	}
	cs, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, err
	}
	if sched, ok := cs.(*cron.SpecSchedule); ok { // not @every
		fields := strings.Fields(spec) // 5, as robfig/cron has checked
		for i, field := range fields {
			for item := range strings.SplitSeq(field, ",") {
				switch value, step, hasStep := strings.Cut(item, "/"); {
				case !standardItem.MatchString(item): // such as "+5", "*-5" or ""
					return nil, fmt.Errorf("%q in %q isn't standard cron", item, field)
				case hasStep && value != "*" && !strings.Contains(value, "-"):
					// robfig/cron reads "5/20" as "5-59/20".
					return nil, fmt.Errorf("%q isn't standard cron: write %q", item, fmt.Sprintf("%s-%d/%s", value, maxes[i], step))
				}
			}
		}
		// Vixie cron runs the days that match both day fields if either starts
		// with "*", as "*/2" does, and otherwise those that match either.
		// robfig/cron decides by any "*" in the field's list, but not "*/2".
		sched.Dom = vixieDays(sched.Dom, fields[2])
		sched.Dow = vixieDays(sched.Dow, fields[4])
	}
	if cs.Next(time.Now()).IsZero() { // robfig/cron looks 5 years ahead
		return nil, errors.New("matches no date in the next 5 years")
	}
	return cs, nil
}

// Run calls due, from Run's goroutine, with the indices of the schedules due,
// in order, from their first times after start until ctx is done. A schedule
// that falls due more than once during a call is due once after it. One with
// no next time (the zero time) isn't due again unless the clock is set back.
//
// cron's own runner isn't used: it would run entries due together in no
// particular order.
func Run(ctx context.Context, scheds []Schedule, start time.Time, due func([]int)) {
	// Round(0) drops the monotonic reading, which stops while the host sleeps.
	// Timers stop too, and miss clock changes, so Run reads the clock at least
	// once a minute.
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
