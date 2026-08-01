// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import (
	"errors"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// baseSchedule is the schedule the RFC's motivating example describes:
// Monday to Friday, 08:00 to 19:00, Rome time, weekend off.
func baseSchedule() Schedule {
	return Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
}

// ruleByName finds a compiled rule, failing the test if it is absent.
func ruleByName(t *testing.T, rules []Rule, name string) Rule {
	t.Helper()
	for _, r := range rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rule %q not found in %v", name, ruleNames(rules))
	return Rule{}
}

func ruleNames(rules []Rule) []string {
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

func TestCompileDisabled(t *testing.T) {
	tests := []struct {
		name string
		in   *Schedule
	}{
		{name: "nil schedule", in: nil},
		{name: "absent enabled", in: &Schedule{Timezone: "Europe/Rome", Start: "08:00", Stop: "19:00"}},
		{name: "explicitly disabled", in: &Schedule{Enabled: boolPtr(false)}},
		{
			name: "explicitly disabled with times",
			in:   &Schedule{Enabled: boolPtr(false), Timezone: "Europe/Rome", Start: "08:00", Stop: "19:00"},
		},
		{
			// An opted-out resource must not be validated as if it were
			// scheduled: a production database with {"enabled": false}
			// has no reason to carry a timezone.
			name: "disabled with nothing else set compiles rather than erroring",
			in:   &Schedule{Enabled: boolPtr(false), Exceptions: []Window{{From: "not-a-date"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := Compile(tt.in)
			if err != nil {
				t.Fatalf("Compile() error = %v, want nil", err)
			}
			if len(rules) != 0 {
				t.Errorf("Compile() = %d rules, want 0", len(rules))
			}
		})
	}
}

func TestCompileBaseRhythm(t *testing.T) {
	s := baseSchedule()
	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	if len(rules) != 2 {
		t.Fatalf("Compile() = %d rules, want 2: %v", len(rules), ruleNames(rules))
	}

	start := ruleByName(t, rules, "start-base-0")
	stop := ruleByName(t, rules, "stop-base-0")

	if start.Action != ActionStart || stop.Action != ActionStop {
		t.Errorf("actions = %q/%q, want start/stop", start.Action, stop.Action)
	}
	if start.Hour != 8 || start.Min != 0 {
		t.Errorf("start time = %02d:%02d, want 08:00", start.Hour, start.Min)
	}
	if stop.Hour != 19 || stop.Min != 0 {
		t.Errorf("stop time = %02d:%02d, want 19:00", stop.Hour, stop.Min)
	}
	for _, r := range rules {
		if r.Timezone != "Europe/Rome" {
			t.Errorf("rule %q timezone = %q, want Europe/Rome", r.Name, r.Timezone)
		}
		if r.Bounded() {
			t.Errorf("rule %q is bounded, want unbounded with no exceptions", r.Name)
		}
	}

	// The default weekday set, and with it the weekend-off behavior: no
	// start fires on Saturday or Sunday, and Friday's stop leaves the
	// environment down until Monday.
	want := []Weekday{Mon, Tue, Wed, Thu, Fri}
	for _, r := range rules {
		if len(r.Days) != len(want) {
			t.Fatalf("rule %q days = %v, want %v", r.Name, r.Days, want)
		}
		for i, d := range want {
			if r.Days[i] != d {
				t.Fatalf("rule %q days = %v, want %v", r.Name, r.Days, want)
			}
		}
	}
}

func TestCompileDays(t *testing.T) {
	tests := []struct {
		name string
		days []Weekday
		want []Weekday
	}{
		{name: "absent defaults to the work week", days: nil, want: []Weekday{Mon, Tue, Wed, Thu, Fri}},
		{name: "explicit set is honored", days: []Weekday{Mon, Wed, Fri}, want: []Weekday{Mon, Wed, Fri}},
		{
			// Order and duplicates must not change the compiled output,
			// otherwise a reordered translation churns the provisioned
			// schedules on every apply.
			name: "out-of-order input is normalized",
			days: []Weekday{Fri, Mon, Fri, Wed},
			want: []Weekday{Mon, Wed, Fri},
		},
		{name: "weekend only", days: []Weekday{Sat, Sun}, want: []Weekday{Sat, Sun}},
		{name: "all seven days", days: weekOrder, want: weekOrder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSchedule()
			s.Days = tt.days

			rules, err := Compile(&s)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			got := ruleByName(t, rules, "start-base-0").Days
			if len(got) != len(tt.want) {
				t.Fatalf("days = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("days = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestCompileOvernightWindowIsAccepted(t *testing.T) {
	// A start later than a stop describes an environment that runs
	// overnight. The naive "stop must be after start" check would reject
	// a legitimate request, so this is asserted explicitly.
	s := baseSchedule()
	s.Start = "22:00"
	s.Stop = "06:00"

	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v, want an overnight schedule to compile", err)
	}
	if got := ruleByName(t, rules, "start-base-0").Hour; got != 22 {
		t.Errorf("start hour = %d, want 22", got)
	}
	if got := ruleByName(t, rules, "stop-base-0").Hour; got != 6 {
		t.Errorf("stop hour = %d, want 6", got)
	}
}

func TestCompileRejectsIncompleteSchedule(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *Schedule)
		wantErr error
	}{
		{
			name:    "missing timezone is never defaulted to the local one",
			mutate:  func(s *Schedule) { s.Timezone = "" },
			wantErr: ErrTimezoneRequired,
		},
		{
			name:    "unknown timezone",
			mutate:  func(s *Schedule) { s.Timezone = "Middle/Earth" },
			wantErr: ErrInvalidTimezone,
		},
		{
			// A fixed offset is correct for half the year and an hour
			// wrong for the other half.
			name:    "fixed offset is not an IANA zone",
			mutate:  func(s *Schedule) { s.Timezone = "+02:00" },
			wantErr: ErrInvalidTimezone,
		},
		{
			name:    "Local means nothing in a cloud account",
			mutate:  func(s *Schedule) { s.Timezone = "Local" },
			wantErr: ErrInvalidTimezone,
		},
		{
			name:    "missing start",
			mutate:  func(s *Schedule) { s.Start = "" },
			wantErr: ErrStartRequired,
		},
		{
			name:    "missing stop",
			mutate:  func(s *Schedule) { s.Stop = "" },
			wantErr: ErrStopRequired,
		},
		{
			name:    "malformed start",
			mutate:  func(s *Schedule) { s.Start = "8am" },
			wantErr: ErrInvalidClockTime,
		},
		{
			name:    "out-of-range hour",
			mutate:  func(s *Schedule) { s.Stop = "25:00" },
			wantErr: ErrInvalidClockTime,
		},
		{
			name:    "out-of-range minute",
			mutate:  func(s *Schedule) { s.Stop = "19:60" },
			wantErr: ErrInvalidClockTime,
		},
		{
			name:    "unrecognized weekday leaves no active day",
			mutate:  func(s *Schedule) { s.Days = []Weekday{"caturday"} },
			wantErr: ErrNoActiveDays,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSchedule()
			tt.mutate(&s)

			if _, err := Compile(&s); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Compile() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompileAlwaysOffWindowStopsDaily(t *testing.T) {
	// The RDS seven-day auto-restart: AWS starts an instance that has
	// been stopped for more than a week. A one-shot stop at the start of
	// a two-week shutdown would leave it running, and billing, for the
	// second week (RFC 012 §2.2 step 5).
	s := baseSchedule()
	s.Exceptions = []Window{{From: "2026-12-24", To: "2027-01-06", Mode: ModeAlwaysOff, Reason: "company shutdown"}}

	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	stop := ruleByName(t, rules, "stop-window-0")
	if len(stop.Days) != 0 {
		t.Errorf("window stop days = %v, want empty (every day)", stop.Days)
	}
	if stop.Action != ActionStop {
		t.Errorf("window rule action = %q, want stop", stop.Action)
	}
	if stop.ValidFrom == nil || stop.ValidTo == nil {
		t.Fatalf("window stop must be bounded, got from=%v to=%v", stop.ValidFrom, stop.ValidTo)
	}
	// The window covers the whole of its final day, so the half-open
	// interval ends at midnight of the following one.
	if got, want := stop.ValidTo.Format(time.RFC3339), "2027-01-07T00:00:00+01:00"; got != want {
		t.Errorf("window stop ValidTo = %s, want %s", got, want)
	}
	if got := stop.ValidTo.Sub(*stop.ValidFrom); got < 14*24*time.Hour {
		t.Errorf("window spans %v, want at least 14 days of daily stops", got)
	}

	// No start rule may fire inside an always_off window.
	for _, r := range rules {
		if r.Action != ActionStart {
			continue
		}
		if r.ValidFrom != nil && r.ValidTo != nil &&
			r.ValidFrom.Before(*stop.ValidTo) && stop.ValidFrom.Before(*r.ValidTo) {
			t.Errorf("start rule %q overlaps the always_off window", r.Name)
		}
	}
}

func TestCompileAlwaysOnWindowSuspendsTheStop(t *testing.T) {
	s := baseSchedule()
	s.Exceptions = []Window{{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn, Reason: "release weekend"}}

	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	start := ruleByName(t, rules, "start-window-0")
	if start.Action != ActionStart {
		t.Errorf("window rule action = %q, want start", start.Action)
	}
	// Fires once, at the first midnight: nothing stops the resource for
	// the rest of the window, so repeating the start would only produce a
	// daily failed invocation against an already-running resource.
	if start.ValidFrom == nil || start.ValidTo == nil {
		t.Fatalf("window start must be bounded, got from=%v to=%v", start.ValidFrom, start.ValidTo)
	}
	if got := start.ValidTo.Sub(*start.ValidFrom); got != 24*time.Hour {
		t.Errorf("always_on start spans %v, want a single 24h firing window", got)
	}

	// The base rhythm is segmented around the window, not competing with
	// it: no stop rule is live while the window is open.
	windowEnd := start.ValidFrom.AddDate(0, 0, 3)
	for _, r := range rules {
		if r.Action != ActionStop {
			continue
		}
		if r.ValidFrom != nil && r.ValidTo != nil &&
			r.ValidFrom.Before(windowEnd) && start.ValidFrom.Before(*r.ValidTo) {
			t.Errorf("stop rule %q is live during the always_on window", r.Name)
		}
		if r.ValidTo != nil && r.ValidTo.After(*start.ValidFrom) && r.ValidFrom == nil {
			t.Errorf("stop rule %q extends into the always_on window", r.Name)
		}
	}
}

func TestCompileSegmentsBaseRhythm(t *testing.T) {
	tests := []struct {
		name       string
		exceptions []Window
		wantRules  []string
	}{
		{
			name:      "no windows leaves one unbounded segment",
			wantRules: []string{"start-base-0", "stop-base-0"},
		},
		{
			name:       "one window splits the rhythm in two",
			exceptions: []Window{{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn}},
			wantRules: []string{
				"start-base-0", "stop-base-0",
				"start-base-1", "stop-base-1",
				"start-window-0",
			},
		},
		{
			name: "two windows split the rhythm in three",
			exceptions: []Window{
				{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn},
				{From: "2026-12-24", To: "2027-01-06", Mode: ModeAlwaysOff},
			},
			wantRules: []string{
				"start-base-0", "stop-base-0",
				"start-base-1", "stop-base-1",
				"start-base-2", "stop-base-2",
				"start-window-0", "stop-window-1",
			},
		},
		{
			// Back-to-back windows leave a zero-length gap between them,
			// which must not produce a segment that can never fire.
			name: "adjacent windows leave no empty segment",
			exceptions: []Window{
				{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn},
				{From: "2026-09-15", To: "2026-09-20", Mode: ModeAlwaysOff},
			},
			wantRules: []string{
				"start-base-0", "stop-base-0",
				"start-base-2", "stop-base-2",
				"start-window-0", "stop-window-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSchedule()
			s.Exceptions = tt.exceptions

			rules, err := Compile(&s)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}

			got := ruleNames(rules)
			if len(got) != len(tt.wantRules) {
				t.Fatalf("rules = %v, want %v", got, tt.wantRules)
			}
			for i := range tt.wantRules {
				if got[i] != tt.wantRules[i] {
					t.Fatalf("rules = %v, want %v", got, tt.wantRules)
				}
			}
		})
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	// Rule names are provider resource names. If they depended on the
	// order the translator happened to emit the windows in, an unchanged
	// specification would churn the provisioned schedules on re-apply.
	forward := baseSchedule()
	forward.Exceptions = []Window{
		{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn},
		{From: "2026-12-24", To: "2027-01-06", Mode: ModeAlwaysOff},
	}
	reversed := baseSchedule()
	reversed.Exceptions = []Window{forward.Exceptions[1], forward.Exceptions[0]}

	a, err := Compile(&forward)
	if err != nil {
		t.Fatalf("Compile(forward) error = %v", err)
	}
	b, err := Compile(&reversed)
	if err != nil {
		t.Fatalf("Compile(reversed) error = %v", err)
	}

	if len(a) != len(b) {
		t.Fatalf("rule counts differ: %v vs %v", ruleNames(a), ruleNames(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Action != b[i].Action {
			t.Fatalf("rule %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestCompileRejectsBadWindows(t *testing.T) {
	tooMany := make([]Window, MaxWindows+1)
	for i := range tooMany {
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i*2)
		tooMany[i] = Window{
			From: day.Format(dateLayout),
			To:   day.Format(dateLayout),
			Mode: ModeAlwaysOff,
		}
	}

	tests := []struct {
		name       string
		exceptions []Window
		wantErr    error
	}{
		{
			name:       "inverted window",
			exceptions: []Window{{From: "2026-09-14", To: "2026-09-12", Mode: ModeAlwaysOn}},
			wantErr:    ErrInvertedWindow,
		},
		{
			name: "overlapping windows are ambiguous",
			exceptions: []Window{
				{From: "2026-09-12", To: "2026-09-20", Mode: ModeAlwaysOn},
				{From: "2026-09-18", To: "2026-09-25", Mode: ModeAlwaysOff},
			},
			wantErr: ErrOverlappingWindows,
		},
		{
			name: "windows sharing a single day still overlap",
			exceptions: []Window{
				{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn},
				{From: "2026-09-14", To: "2026-09-16", Mode: ModeAlwaysOff},
			},
			wantErr: ErrOverlappingWindows,
		},
		{
			name:       "malformed from date",
			exceptions: []Window{{From: "12/09/2026", To: "2026-09-14", Mode: ModeAlwaysOn}},
			wantErr:    ErrInvalidWindowDate,
		},
		{
			name:       "abbreviated date is not the documented format",
			exceptions: []Window{{From: "2026-9-2", To: "2026-09-14", Mode: ModeAlwaysOn}},
			wantErr:    ErrInvalidWindowDate,
		},
		{
			name:       "impossible date",
			exceptions: []Window{{From: "2026-02-30", To: "2026-03-01", Mode: ModeAlwaysOn}},
			wantErr:    ErrInvalidWindowDate,
		},
		{
			name:       "malformed to date",
			exceptions: []Window{{From: "2026-09-12", To: "", Mode: ModeAlwaysOn}},
			wantErr:    ErrInvalidWindowDate,
		},
		{
			name:       "unknown mode",
			exceptions: []Window{{From: "2026-09-12", To: "2026-09-14", Mode: "maybe"}},
			wantErr:    ErrInvalidWindowMode,
		},
		{
			name:       "more windows than the cap",
			exceptions: tooMany,
			wantErr:    ErrTooManyWindows,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSchedule()
			s.Exceptions = tt.exceptions

			if _, err := Compile(&s); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Compile() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompileAdjacentWindowsAreNotAnOverlap(t *testing.T) {
	// A window ending on the 14th and the next starting on the 15th share
	// no day. Rejecting them would make consecutive shutdown periods
	// inexpressible.
	s := baseSchedule()
	s.Exceptions = []Window{
		{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn},
		{From: "2026-09-15", To: "2026-09-20", Mode: ModeAlwaysOff},
	}

	if _, err := Compile(&s); err != nil {
		t.Fatalf("Compile() error = %v, want adjacent windows to be accepted", err)
	}
}

func TestCompileKeepsWallClockAcrossDST(t *testing.T) {
	// The rules carry the zone name and the wall-clock time, never a
	// precomputed offset: 08:00 in Rome must stay 08:00 in Rome on both
	// sides of the March transition.
	s := baseSchedule()
	s.Exceptions = []Window{
		{From: "2026-03-28", To: "2026-03-30", Mode: ModeAlwaysOff},
	}

	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	before := ruleByName(t, rules, "stop-base-0")
	after := ruleByName(t, rules, "stop-base-1")
	if before.Hour != after.Hour || before.Min != after.Min {
		t.Errorf("wall-clock time drifted across the window: %02d:%02d then %02d:%02d",
			before.Hour, before.Min, after.Hour, after.Min)
	}
	if before.Timezone != "Europe/Rome" || after.Timezone != "Europe/Rome" {
		t.Errorf("timezone = %q/%q, want Europe/Rome on both segments", before.Timezone, after.Timezone)
	}

	// The window itself spans the transition, so its two bounds sit at
	// different UTC offsets while both being local midnight.
	stop := ruleByName(t, rules, "stop-window-0")
	_, fromOffset := stop.ValidFrom.Zone()
	_, toOffset := stop.ValidTo.Zone()
	if fromOffset == toOffset {
		t.Errorf("expected the DST transition to change the offset, both were %d", fromOffset)
	}
	if h, m, s := stop.ValidFrom.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("window start = %02d:%02d:%02d, want local midnight", h, m, s)
	}
}

func TestCompileRuleCountIsBounded(t *testing.T) {
	// The MaxWindows cap exists to bound how many scheduling resources a
	// single specification can provision.
	exceptions := make([]Window, MaxWindows)
	for i := range exceptions {
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i*2)
		exceptions[i] = Window{
			From: day.Format(dateLayout),
			To:   day.Format(dateLayout),
			Mode: ModeAlwaysOff,
		}
	}
	s := baseSchedule()
	s.Exceptions = exceptions

	rules, err := Compile(&s)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if max := 2*(MaxWindows+1) + MaxWindows; len(rules) > max {
		t.Errorf("Compile() = %d rules, want at most %d", len(rules), max)
	}
}
