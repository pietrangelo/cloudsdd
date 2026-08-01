// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

func boolPtr(b bool) *bool { return &b }

// resetScheduleFlags restores the package-level scheduling flags after a
// test has set them, the same way newHarness restores assumeYes.
func resetScheduleFlags(t *testing.T) {
	t.Helper()
	start, stop, tz, days, none := scheduleStart, scheduleStop, scheduleTimezone, scheduleDays, noSchedule
	t.Cleanup(func() {
		scheduleStart, scheduleStop, scheduleTimezone, scheduleDays, noSchedule = start, stop, tz, days, none
	})
	scheduleStart, scheduleStop, scheduleTimezone, scheduleDays, noSchedule = "", "", "", "", false
}

// dbSpec builds a Specification with a schedulable resource, since the
// object stores used by the other CLI tests have no power state.
func dbSpec(intent spec.Intent, sch *schedule.Schedule) spec.Specification {
	return spec.Specification{
		SDDVersion: "1.0",
		Intent:     intent,
		Resources: []spec.Resource{{
			ID:         "app-db",
			Type:       spec.ResourceTypeRelationalDatabase,
			Provider:   spec.ProviderAWS,
			Scope:      spec.Scope{Environment: "dev", Region: "eu-central-1"},
			Properties: map[string]any{"engine": "postgres", "version": "15"},
		}},
		Policies: spec.Policies{Schedule: sch},
	}
}

// proposedSchedule is what the translator emits for "shut it down outside
// working hours" when the user never said which hours: the intent to
// schedule, and nothing invented (RFC 012 §6).
func proposedSchedule() *schedule.Schedule {
	return &schedule.Schedule{Enabled: boolPtr(true)}
}

func TestRunIntentElicitsScheduleTimes(t *testing.T) {
	resetScheduleFlags(t)

	// start, stop, timezone, days (default accepted), confirmation.
	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil, "08:00\n19:00\nEurope/Rome\n\ny\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database", "off", "outside", "working", "hours"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	// The answers must reach the Specification the user approves, not be
	// filled in somewhere later.
	for _, want := range []string{`"start": "08:00"`, `"stop": "19:00"`, `"timezone": "Europe/Rome"`} {
		if !strings.Contains(got, want) {
			t.Errorf("printed specification is missing %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off") {
		t.Errorf("plan output does not describe the schedule in prose:\n%s", got)
	}
	if len(h.providers[spec.ProviderAWS].applied) != 1 {
		t.Error("the resource was not applied")
	}
}

func TestRunIntentDoesNotPromptForACompleteSchedule(t *testing.T) {
	resetScheduleFlags(t)

	complete := &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
	h := newHarness(t, dbSpec(spec.IntentDeploy, complete), nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}
	if strings.Contains(h.out.String(), "Start time") {
		t.Error("the user was asked for times the specification already carried")
	}
}

func TestRunIntentRefusesToInventTimesNonInteractively(t *testing.T) {
	resetScheduleFlags(t)

	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil, "")
	assumeYes = true

	err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy)
	if !errors.Is(err, ErrScheduleTimesRequired) {
		t.Fatalf("runIntent() error = %v, want %v", err, ErrScheduleTimesRequired)
	}
	if !strings.Contains(err.Error(), "--schedule-start") {
		t.Errorf("error = %q, want it to name the flags that would resolve it", err)
	}
	// A stop time nobody chose is an outage: nothing may be applied.
	if len(h.providers[spec.ProviderAWS].applied) != 0 {
		t.Error("resources were applied despite an unresolved schedule")
	}
}

func TestRunIntentFailsOnEOFRatherThanGuessing(t *testing.T) {
	resetScheduleFlags(t)

	// Interactive mode, but stdin is closed: a piped invocation without
	// the flags.
	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil, "")

	err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy)
	if !errors.Is(err, ErrScheduleTimesRequired) {
		t.Fatalf("runIntent() error = %v, want %v", err, ErrScheduleTimesRequired)
	}
	if len(h.providers[spec.ProviderAWS].applied) != 0 {
		t.Error("resources were applied despite an unresolved schedule")
	}
}

func TestRunIntentScheduleFlagsBypassThePrompt(t *testing.T) {
	resetScheduleFlags(t)
	scheduleStart, scheduleStop, scheduleTimezone = "07:30", "20:00", "UTC"

	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	if strings.Contains(got, "Start time") {
		t.Error("the user was prompted despite the flags being supplied")
	}
	if !strings.Contains(got, "Mon-Fri 07:30 -> 20:00 (UTC), weekend off") {
		t.Errorf("flag values did not reach the schedule:\n%s", got)
	}
}

func TestRunIntentScheduleFlagsCreateASchedule(t *testing.T) {
	resetScheduleFlags(t)
	scheduleStart, scheduleStop, scheduleTimezone = "08:00", "19:00", "Europe/Rome"
	scheduleDays = "mon,wed,fri"

	// The prompt never mentioned scheduling, so the translator proposed
	// none. The flags alone must be enough.
	h := newHarness(t, dbSpec(spec.IntentDeploy, nil), nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}
	if !strings.Contains(h.out.String(), "Mon, Wed, Fri 08:00 -> 19:00 (Europe/Rome), weekend off") {
		t.Errorf("the flags did not create a schedule:\n%s", h.out.String())
	}
}

func TestRunIntentNoScheduleStripsTheProposal(t *testing.T) {
	resetScheduleFlags(t)
	noSchedule = true

	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	if strings.Contains(got, "Start time") {
		t.Error("the user was prompted for a schedule they explicitly declined")
	}
	if strings.Contains(got, "Power schedule:") {
		t.Errorf("a schedule survived --no-schedule:\n%s", got)
	}
}

func TestRunIntentDestroyIgnoresSchedules(t *testing.T) {
	resetScheduleFlags(t)

	// An incomplete schedule must not interrogate the user about the
	// working hours of something they are deleting.
	h := newHarness(t, dbSpec(spec.IntentDestroy, proposedSchedule()), nil, "y\n")

	if err := runIntent(h.cmd, []string{"remove", "the", "dev", "database"}, spec.IntentDestroy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}
	if strings.Contains(h.out.String(), "Start time") {
		t.Error("destroy prompted for schedule times")
	}
	if len(h.providers[spec.ProviderAWS].destroyed) != 1 {
		t.Error("the resource was not destroyed")
	}
}

func TestRunIntentReportsAnInapplicableSchedule(t *testing.T) {
	resetScheduleFlags(t)

	// A specification-wide schedule over a bucket: inapplicable, but the
	// user asked for an environment to power down and must be told which
	// parts of it will not.
	s := testSpec(spec.IntentDeploy)
	s.Policies.Schedule = &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
	h := newHarness(t, s, nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}
	if !strings.Contains(h.out.String(), "stays up") {
		t.Errorf("output does not report the inapplicable schedule:\n%s", h.out.String())
	}
}

func TestRunIntentDoesNotPromptWhenNothingIsSchedulable(t *testing.T) {
	resetScheduleFlags(t)

	// A specification-wide schedule over nothing but object stores. It is
	// enabled, but no resource can honor it, so asking the user for the
	// working hours of an environment that will never power down would be
	// worse than not asking.
	s := testSpec(spec.IntentDeploy)
	s.Policies.Schedule = proposedSchedule()
	h := newHarness(t, s, nil, "y\n")

	if err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}
	if strings.Contains(h.out.String(), "Start time") {
		t.Errorf("the user was prompted for a schedule nothing can honor:\n%s", h.out.String())
	}
	if len(h.providers[spec.ProviderAWS].applied) != 1 {
		t.Error("the resource was not applied")
	}
}

func TestElicitScheduleRejectsThenAcceptsInput(t *testing.T) {
	resetScheduleFlags(t)

	// Two malformed answers, then a good one.
	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil,
		"8am\n25:00\n08:00\n19:00\nEurope/Rome\nmon,tue\ny\n")

	if err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	if !strings.Contains(got, `"8am" is not valid`) {
		t.Errorf("output does not report the rejected input:\n%s", got)
	}
	if !strings.Contains(got, "Mon, Tue 08:00 -> 19:00 (Europe/Rome), weekend off") {
		t.Errorf("the eventual answers did not take effect:\n%s", got)
	}
}

func TestElicitScheduleGivesUpAfterRepeatedBadInput(t *testing.T) {
	resetScheduleFlags(t)

	h := newHarness(t, dbSpec(spec.IntentDeploy, proposedSchedule()), nil,
		"8am\n9am\n10am\n11am\ny\n")

	err := runIntent(h.cmd, []string{"a", "dev", "database"}, spec.IntentDeploy)
	if !errors.Is(err, ErrScheduleTimesRequired) {
		t.Fatalf("runIntent() error = %v, want %v", err, ErrScheduleTimesRequired)
	}
}

func TestParseDays(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []schedule.Weekday
	}{
		{name: "work week", in: "mon,tue,wed,thu,fri", want: []schedule.Weekday{schedule.Mon, schedule.Tue, schedule.Wed, schedule.Thu, schedule.Fri}},
		{name: "spaces and case", in: " MON , Fri ", want: []schedule.Weekday{schedule.Mon, schedule.Fri}},
		{name: "unknown entries are dropped", in: "mon,caturday", want: []schedule.Weekday{schedule.Mon}},
		{name: "empty", in: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDays(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("parseDays(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("parseDays(%q) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}

func TestValidDayList(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "mon", want: true},
		{in: "mon,tue,wed,thu,fri", want: true},
		{in: " sat , sun ", want: true},
		{in: "", want: false},
		{in: "mon,caturday", want: false},
		{in: "mon-fri", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := validDayList(tt.in); got != tt.want {
				t.Errorf("validDayList(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStripSchedules(t *testing.T) {
	s := dbSpec(spec.IntentDeploy, proposedSchedule())
	s.Resources[0].Schedule = proposedSchedule()

	stripSchedules(&s)

	if s.Policies.Schedule != nil {
		t.Error("the policy schedule survived stripSchedules")
	}
	if s.Resources[0].Schedule != nil {
		t.Error("the resource schedule survived stripSchedules")
	}
}
