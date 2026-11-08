// Package schedule decides when backups run. It is the only package that uses
// robfig/cron, so the cron library can be replaced in one place.
package schedule

import (
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
