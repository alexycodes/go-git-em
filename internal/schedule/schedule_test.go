package schedule

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	// CRON_TZ needs time zone data, which the test host may not have.
	_ "time/tzdata"
)

func TestParse(t *testing.T) {
	// Without CRON_TZ, a schedule is in the time zone of the time given to
	// Next, so these results don't depend on the machine's time zone.
	from := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		spec string
		want time.Time // the next time after from
	}{
		{"0 * * * *", time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)},
		{"15 3 * * 1", time.Date(2026, 11, 2, 3, 15, 0, 0, time.UTC)}, // Monday
		{"@hourly", time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		{"@every 6h", from.Add(6 * time.Hour)},
		{"@every 1s", from.Add(time.Second)},
		{"@every 90m", from.Add(90 * time.Minute)},
		{"CRON_TZ=UTC @every 1h", from.Add(time.Hour)},
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)}, // the next leap day
		// 03:00 in Paris is 02:00 UTC in November.
		{"CRON_TZ=Europe/Paris 0 3 * * *", time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		s, err := Parse(tt.spec)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.spec, err)
			continue
		}
		if got := s.Next(from); !got.Equal(tt.want) {
			t.Errorf("Parse(%q).Next(%v) = %v, want %v", tt.spec, from, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		spec    string
		wantErr string
	}{
		{"0 * * *", "expected exactly 5 fields, found 4"},
		{"0 0 * * * *", "expected exactly 5 fields, found 6"},
		{"61 * * * *", "end of range (61) above maximum (59)"},
		{"@fortnightly", "unrecognized descriptor"},
		{"@every soon", "failed to parse duration"},
		{"CRON_TZ=Mars/Olympus 0 3 * * *", "provided bad location Mars/Olympus"},
		{"", "empty spec string"},
		// robfig/cron v3.0.1 panics on these.
		{"CRON_TZ=UTC", "a time zone needs a schedule after it"},
		{"TZ=", "a time zone needs a schedule after it"},
		// robfig/cron would run these every second.
		{"@every 0s", "@every needs a whole number of seconds, at least 1s, got 0s"},
		{"@every -1h", "@every needs a whole number of seconds, at least 1s, got -1h0m0s"},
		{"@every 500ms", "@every needs a whole number of seconds, at least 1s, got 500ms"},
		{"@every 1.5s", "@every needs a whole number of seconds, at least 1s, got 1.5s"},
		{"CRON_TZ=UTC @every 0s", "@every needs a whole number of seconds"},
		// robfig/cron accepts these, and they never fire.
		{"0 0 30 2 *", "never fires: no date matches it"},
		{"0 0 31 4 *", "never fires: no date matches it"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.spec)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("Parse(%q) error = %v, want it to contain %q", tt.spec, err, tt.wantErr)
		}
	}
}

// The Run tests use synctest's fake clock, which moves on only when every
// goroutine is waiting, so the times are exact.

func parseAll(t *testing.T, specs ...string) []Schedule {
	t.Helper()
	var scheds []Schedule
	for _, spec := range specs {
		s, err := Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		scheds = append(scheds, s)
	}
	return scheds
}

// A call is one call of Run's due.
type call struct {
	at  time.Duration // from the start of runFor
	due []int
}

// runFor runs Run for d from now, and returns its calls.
func runFor(t *testing.T, scheds []Schedule, d time.Duration, due func([]int)) []call {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	var calls []call
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		Run(ctx, scheds, start, func(i []int) {
			calls = append(calls, call{time.Since(start), i})
			due(i)
		})
	}()
	time.Sleep(d)
	cancel()
	<-returned // Run has returned, so calls is safe to read
	return calls
}

func times(calls []call) []time.Duration {
	var ts []time.Duration
	for _, c := range calls {
		ts = append(ts, c.at)
	}
	return ts
}

// moveClock makes Run's clock read the fake clock plus an offset, which the
// returned function changes, as setting the clock would.
func moveClock(t *testing.T) (move func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	var offset time.Duration
	clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Now().Add(offset)
	}
	t.Cleanup(func() { clock = time.Now })
	return func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		offset += d
	}
}

func TestRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := runFor(t, parseAll(t, "@every 1s", "@every 2s", "* * * * *"), 2*time.Minute+500*time.Millisecond, func([]int) {})
		counts := make([]int, 3)
		for _, c := range calls {
			for _, i := range c.due {
				counts[i]++
			}
		}
		if want := []int{120, 60, 2}; !slices.Equal(counts, want) {
			t.Errorf("calls per schedule = %v, want %v", counts, want)
		}
		// Schedules due at the same time come together, in order.
		for _, want := range []call{{time.Second, []int{0}}, {2 * time.Second, []int{0, 1}}, {time.Minute, []int{0, 1, 2}}} {
			if !slices.ContainsFunc(calls, func(c call) bool { return c.at == want.at && slices.Equal(c.due, want.due) }) {
				t.Errorf("no call with %v at %v", want.due, want.at)
			}
		}
	})
}

// A schedule that falls due several times during a call is due once after it.
// The stop comes during the last call: when it returns, the stop and an overdue
// time are both ready, and Run must take the stop, whatever select picks. So
// the test runs 20 times.
func TestRunSlowCall(t *testing.T) {
	for range 20 {
		synctest.Test(t, func(t *testing.T) {
			calls := runFor(t, parseAll(t, "@every 1s"), 7*time.Second, func([]int) { time.Sleep(2500 * time.Millisecond) })
			if got, want := times(calls), []time.Duration{time.Second, 3500 * time.Millisecond, 6 * time.Second}; !slices.Equal(got, want) {
				t.Errorf("calls at %v, want %v", got, want)
			}
		})
	}
}

func TestRunNoSchedules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if calls := runFor(t, nil, time.Hour, func([]int) {}); len(calls) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})
}

// never is a schedule with no next time, like a cron schedule with no date in
// the next 5 years.
type never struct{}

func (never) Next(time.Time) time.Time { return time.Time{} }

func TestRunNever(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := runFor(t, append([]Schedule{never{}}, parseAll(t, "@every 1s")...), 3500*time.Millisecond, func([]int) {})
		if got, want := times(calls), []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("calls at %v, want %v", got, want)
		}
		for _, c := range calls {
			if !slices.Equal(c.due, []int{1}) {
				t.Errorf("due at %v: %v, want [1]", c.at, c.due)
			}
		}
	})
}

// After the clock jumps forward past a due time, as after the host slept, the
// schedule is due within a minute, not when the timer set before would fire.
func TestRunClockJumpsForward(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		move := moveClock(t)
		go func() {
			time.Sleep(30*time.Minute + 30*time.Second) // fake time starts at midnight UTC
			move(3 * time.Hour)
		}()
		calls := runFor(t, parseAll(t, "CRON_TZ=UTC 0 3 * * *"), time.Hour, func([]int) {})
		if got, want := times(calls), []time.Duration{31 * time.Minute}; !slices.Equal(got, want) {
			t.Errorf("calls at %v, want %v", got, want)
		}
	})
}

// After the clock is set back, the schedule starts over from the new time,
// rather than waiting for the time it was due at before.
func TestRunClockSetBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		move := moveClock(t)
		go func() {
			time.Sleep(time.Hour) // 01:00 on January 1
			move(-48 * time.Hour) // 01:00 on December 30
		}()
		calls := runFor(t, parseAll(t, "CRON_TZ=UTC 0 3 * * *"), 4*time.Hour, func([]int) {})
		if got, want := times(calls), []time.Duration{3 * time.Hour}; !slices.Equal(got, want) {
			t.Errorf("calls at %v, want %v (03:00 on December 30)", got, want)
		}
	})
}

// before is a schedule due every second until end, after which it has no time
// left, like a cron schedule with no date before its horizon.
type before struct{ end time.Time }

func (b before) Next(t time.Time) time.Time {
	if !t.Before(b.end) {
		return time.Time{}
	}
	return t.Truncate(time.Second).Add(time.Second)
}

// A schedule left with no time while the clock was too far ahead comes back
// once the clock is set right.
func TestRunClockSetBackRevives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		move := moveClock(t)
		end := time.Now().Add(time.Hour)
		go func() {
			time.Sleep(2500 * time.Millisecond)
			move(2 * time.Hour) // past end: due once more, then no time left
			time.Sleep(10 * time.Second)
			move(-2 * time.Hour)
		}()
		calls := runFor(t, []Schedule{before{end}}, 2*time.Minute, func([]int) {})
		// The minute-long wait after 3s finds the clock set back at 63s.
		if got := times(calls); len(got) < 4 || got[2] != 3*time.Second || got[3] != 64*time.Second {
			t.Errorf("calls at %v, want 1s, 2s, 3s, then 64s on", got)
		}
	})
}

// An @every schedule keeps the time it has already waited when the clock is
// set back a little, as NTP may: it's late by the step, not a whole interval.
func TestRunClockSetBackEvery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		move := moveClock(t)
		go func() {
			time.Sleep(50*time.Minute + 30*time.Second)
			move(-2 * time.Minute)
		}()
		calls := runFor(t, parseAll(t, "@every 1h"), 90*time.Minute, func([]int) {})
		if got, want := times(calls), []time.Duration{62 * time.Minute}; !slices.Equal(got, want) {
			t.Errorf("calls at %v, want %v", got, want)
		}
	})
}

// after is a schedule due every second from begin on, with no time before it,
// as a cron schedule whose next date is past its horizon from an earlier time.
type after struct{ begin time.Time }

func (a after) Next(t time.Time) time.Time {
	if t.Before(a.begin) {
		return time.Time{}
	}
	return t.Truncate(time.Second).Add(time.Second)
}

// A clock set back to where the schedule has no time keeps the time it had.
func TestRunClockSetBackKeepsTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		move := moveClock(t)
		go func() {
			time.Sleep(2500 * time.Millisecond)
			move(-time.Hour) // before begin
		}()
		calls := runFor(t, []Schedule{after{time.Now()}}, 2*time.Hour, func([]int) {})
		// Due at 3s on the wall clock, which now comes an hour later.
		if got := times(calls); len(got) < 3 || got[2] != time.Hour+3*time.Second {
			t.Errorf("calls at %v, want 1s, 2s, then 1h0m3s on", got)
		}
	})
}
