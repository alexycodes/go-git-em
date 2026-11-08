package schedule

import (
	"strings"
	"testing"
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
