// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import (
	"testing"
	"time"
)

func TestValidClockTime(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "00:00", want: true},
		{in: "08:00", want: true},
		{in: "19:30", want: true},
		{in: "23:59", want: true},
		{in: "", want: false},
		{in: "8:00", want: false},
		{in: "24:00", want: false},
		{in: "19:60", want: false},
		{in: "19:5", want: false},
		{in: "08:00:00", want: false},
		{in: "8am", want: false},
		{in: " 08:00", want: false},
		{in: "08:00 ", want: false},
		// An anchored pattern is the point: a newline must not let a
		// second line smuggle a different time past the check.
		{in: "08:00\n25:00", want: false},
		{in: "25:00\n08:00", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ValidClockTime(tt.in); got != tt.want {
				t.Errorf("ValidClockTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidTimezone(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "IANA zone", in: "Europe/Rome", want: true},
		{name: "another IANA zone", in: "America/New_York", want: true},
		{name: "UTC is unambiguous and DST-free", in: "UTC", want: true},
		{name: "empty", in: "", want: false},
		{name: "Local is not a location", in: "Local", want: false},
		{name: "fixed offset", in: "+02:00", want: false},
		// CET is a real zone file in the tz database, with the Central
		// European DST rules attached, so it is accepted. "PST" is not,
		// which is why an abbreviation is not a portable way to write a
		// timezone and Europe/Rome is what the prompt recommends.
		{name: "abbreviation backed by a zone file", in: "CET", want: true},
		{name: "abbreviation with no zone file", in: "PST", want: false},
		{name: "unknown zone", in: "Middle/Earth", want: false},
		{name: "path traversal", in: "../../etc/passwd", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTimezone(tt.in); got != tt.want {
				t.Errorf("ValidTimezone(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidDate(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "2026-09-12", want: true},
		{in: "2026-02-28", want: true},
		{in: "2028-02-29", want: true},
		{in: "", want: false},
		{in: "2026-2-8", want: false},
		{in: "26-09-12", want: false},
		{in: "2026-13-01", want: false},
		{in: "2026-02-30", want: false},
		{in: "2026-09-12T00:00:00Z", want: false},
		{in: "12/09/2026", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ValidDate(tt.in); got != tt.want {
				t.Errorf("ValidDate(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestWeekdayToTime(t *testing.T) {
	tests := []struct {
		in     Weekday
		want   time.Weekday
		wantOk bool
	}{
		{in: Mon, want: time.Monday, wantOk: true},
		{in: Sun, want: time.Sunday, wantOk: true},
		{in: Sat, want: time.Saturday, wantOk: true},
		{in: "caturday", wantOk: false},
		{in: "", wantOk: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			got, ok := tt.in.ToTime()
			if ok != tt.wantOk {
				t.Fatalf("ToTime() ok = %v, want %v", ok, tt.wantOk)
			}
			if ok && got != tt.want {
				t.Errorf("ToTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveDays(t *testing.T) {
	tests := []struct {
		name string
		in   *Schedule
		want []Weekday
	}{
		{name: "nil schedule", in: nil, want: workWeek},
		{name: "absent days", in: &Schedule{}, want: workWeek},
		{name: "empty days", in: &Schedule{Days: []Weekday{}}, want: workWeek},
		{name: "explicit days", in: &Schedule{Days: []Weekday{Sun, Mon}}, want: []Weekday{Mon, Sun}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.EffectiveDays()
			if len(got) != len(tt.want) {
				t.Fatalf("EffectiveDays() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("EffectiveDays() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestEffectiveDaysDoesNotAliasTheDefault(t *testing.T) {
	// The default is a package-level slice; handing callers a reference to
	// it would let one compiled schedule mutate every later one.
	got := (*Schedule)(nil).EffectiveDays()
	got[0] = Sun
	if workWeek[0] != Mon {
		t.Fatalf("EffectiveDays() aliased the package default: workWeek = %v", workWeek)
	}
}

func TestIsEnabled(t *testing.T) {
	tests := []struct {
		name string
		in   *Schedule
		want bool
	}{
		{name: "nil", in: nil, want: false},
		{name: "absent", in: &Schedule{}, want: false},
		{name: "false", in: &Schedule{Enabled: boolPtr(false)}, want: false},
		{name: "true", in: &Schedule{Enabled: boolPtr(true)}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// FuzzCompile drives the parsing surface with arbitrary input, mirroring
// the fuzz coverage internal/spec already applies to the JSON parser
// (CLAUDE.md: treat every input as hostile). Compile must return a rule
// set or an error, never panic.
func FuzzCompile(f *testing.F) {
	f.Add("Europe/Rome", "08:00", "19:00", "2026-09-12", "2026-09-14", "always_on")
	f.Add("UTC", "00:00", "23:59", "2026-12-24", "2027-01-06", "always_off")
	f.Add("", "", "", "", "", "")
	f.Add("Local", "8am", "25:61", "2026-02-30", "2026-13-45", "maybe")

	f.Fuzz(func(t *testing.T, tz, start, stop, from, to, mode string) {
		s := Schedule{
			Enabled:    boolPtr(true),
			Timezone:   tz,
			Start:      start,
			Stop:       stop,
			Exceptions: []Window{{From: from, To: to, Mode: ScheduleMode(mode)}},
		}

		rules, err := Compile(&s)
		if err != nil {
			if rules != nil {
				t.Fatalf("Compile() returned %d rules alongside error %v", len(rules), err)
			}
			return
		}

		// A successful compile must never leave a rule that a provider
		// cannot render: an out-of-range time would become a malformed
		// cron expression in the target account.
		for _, r := range rules {
			if r.Hour < 0 || r.Hour > 23 || r.Min < 0 || r.Min > 59 {
				t.Fatalf("rule %q has an out-of-range time %02d:%02d", r.Name, r.Hour, r.Min)
			}
			if r.Timezone == "" {
				t.Fatalf("rule %q has no timezone", r.Name)
			}
			if r.ValidFrom != nil && r.ValidTo != nil && !r.ValidFrom.Before(*r.ValidTo) {
				t.Fatalf("rule %q has an empty validity interval", r.Name)
			}
		}

		// Describe runs on the same input the operator will see.
		_ = Describe(&s)
	})
}
