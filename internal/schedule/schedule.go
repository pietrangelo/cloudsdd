// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package schedule models when a resource is powered on, and compiles that
// model into a provider-agnostic set of Rules (RFC 012).
//
// Every decision about time is made here, by pure functions with no clock,
// no environment, and no I/O, so the whole temporal behavior of CloudSDD is
// table-testable without a cloud account. Providers are left with a
// mechanical translation of an already-validated rule set into their native
// scheduling primitive.
package schedule

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Weekday identifies a day of the week in the Specification. Lowercase
// three-letter names, rather than Go's time.Weekday, because they are what
// the user writes and what the JSON Schema publishes.
type Weekday string

const (
	Mon Weekday = "mon"
	Tue Weekday = "tue"
	Wed Weekday = "wed"
	Thu Weekday = "thu"
	Fri Weekday = "fri"
	Sat Weekday = "sat"
	Sun Weekday = "sun"
)

// weekOrder lists the weekdays in calendar order starting from Monday. It
// is the canonical ordering used when normalizing Days, so a Rule compiled
// from ["fri","mon"] is identical to one compiled from ["mon","fri"] and a
// re-apply does not churn the provisioned schedules.
var weekOrder = []Weekday{Mon, Tue, Wed, Thu, Fri, Sat, Sun}

// goWeekday maps a Weekday to Go's time.Weekday, for providers that need it.
var goWeekday = map[Weekday]time.Weekday{
	Mon: time.Monday,
	Tue: time.Tuesday,
	Wed: time.Wednesday,
	Thu: time.Thursday,
	Fri: time.Friday,
	Sat: time.Saturday,
	Sun: time.Sunday,
}

// ToTime converts a Weekday to Go's time.Weekday. The second return value
// is false for an unrecognized value.
func (w Weekday) ToTime() (time.Weekday, bool) {
	d, ok := goWeekday[w]
	return d, ok
}

// workWeek is the default value of Days: the environment is powered on
// Monday to Friday (RFC 012 §2.1). The weekend needs no rule of its own —
// no start fires on Saturday or Sunday, and Friday's stop leaves the
// environment down until Monday.
var workWeek = []Weekday{Mon, Tue, Wed, Thu, Fri}

// ScheduleMode is what an exception window does to the weekly rhythm.
type ScheduleMode string

const (
	// ModeAlwaysOn keeps the resource powered on for the whole window,
	// e.g. a release weekend.
	ModeAlwaysOn ScheduleMode = "always_on"
	// ModeAlwaysOff keeps the resource powered off for the whole window,
	// e.g. a company shutdown.
	ModeAlwaysOff ScheduleMode = "always_off"
)

// Window suspends the weekly rhythm over an inclusive range of dates,
// interpreted in the Schedule's Timezone.
type Window struct {
	From string       `json:"from" validate:"required,scheduledate"`
	To   string       `json:"to" validate:"required,scheduledate"`
	Mode ScheduleMode `json:"mode" validate:"required,oneof=always_on always_off"`

	// Reason is free-form documentation carried through to the plan
	// output. It originates from a natural language prompt and is echoed
	// back through the ledger, so it is length-capped here and sanitized
	// before display (see Describe).
	Reason string `json:"reason,omitempty" validate:"omitempty,max=128"`
}

// Schedule describes when a resource is powered on. It is declared once in
// spec.Policies and inherited by every schedulable resource in the
// Specification; a resource may override it wholesale (RFC 012 §2.1).
type Schedule struct {
	// Enabled turns scheduling on. Absent means false: an environment
	// nobody asked to shut down never shuts down.
	Enabled *bool `json:"enabled,omitempty"`

	// Timezone is an IANA zone name (e.g. "Europe/Rome"). Fixed offsets
	// are rejected: they drift by an hour across a DST boundary, which
	// would power the environment up an hour late for half the year.
	Timezone string `json:"timezone,omitempty" validate:"omitempty,ianatz"`

	// Start and Stop are wall-clock times in Timezone, "HH:MM". Start
	// later than Stop describes an overnight window and is valid.
	Start string `json:"start,omitempty" validate:"omitempty,clocktime"`
	Stop  string `json:"stop,omitempty" validate:"omitempty,clocktime"`

	// Days lists the weekdays the environment is powered on. Absent means
	// Monday to Friday.
	Days []Weekday `json:"days,omitempty" validate:"omitempty,max=7,unique,dive,oneof=mon tue wed thu fri sat sun"`

	// Exceptions are date ranges over which the weekly rhythm is
	// suspended. Capped at MaxWindows, which bounds the number of
	// scheduling resources a single Specification can provision.
	Exceptions []Window `json:"exceptions,omitempty" validate:"omitempty,max=12,dive"`
}

// MaxWindows is the cap on Exceptions, mirrored by the validate tag on the
// field. With it, Compile emits at most 2*(MaxWindows+1)+MaxWindows rules.
const MaxWindows = 12

// IsEnabled reports whether scheduling is active. A nil Schedule and an
// absent Enabled both mean "no scheduling", so callers can pass the result
// of spec.EffectiveSchedule straight in.
func (s *Schedule) IsEnabled() bool {
	return s != nil && boolOrDefault(s.Enabled, false)
}

// EffectiveDays returns Days, or the Monday-to-Friday default when absent,
// normalized into calendar order and deduplicated.
func (s *Schedule) EffectiveDays() []Weekday {
	if s == nil || len(s.Days) == 0 {
		return append([]Weekday(nil), workWeek...)
	}
	return normalizeDays(s.Days)
}

// normalizeDays returns days in calendar order without duplicates, so
// compiled Rules are independent of the order the user wrote.
func normalizeDays(days []Weekday) []Weekday {
	present := make(map[Weekday]bool, len(days))
	for _, d := range days {
		present[d] = true
	}
	out := make([]Weekday, 0, len(present))
	for _, d := range weekOrder {
		if present[d] {
			out = append(out, d)
		}
	}
	return out
}

// clockTimePattern constrains Start and Stop to a 24-hour wall-clock time.
// Anchored and exhaustive: it is the only thing standing between a prompt
// and an environment that powers down at the wrong hour.
var clockTimePattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// ValidClockTime reports whether v is a well-formed "HH:MM" wall-clock
// time. Exported so internal/spec can register it as a validator tag
// without duplicating the pattern.
func ValidClockTime(v string) bool { return clockTimePattern.MatchString(v) }

// dateLayout is the date format used by Window.From and Window.To.
const dateLayout = "2006-01-02"

// ValidDate reports whether v is a well-formed calendar date. It rejects
// the abbreviated forms time.Parse otherwise accepts ("2026-1-2"), so the
// on-the-wire format is exactly the one the schema documents.
func ValidDate(v string) bool {
	if len(v) != len(dateLayout) {
		return false
	}
	_, err := time.Parse(dateLayout, v)
	return err == nil
}

// ValidTimezone reports whether v names a real IANA zone.
//
// "Local" and "UTC" are treated differently on purpose: "Local" is
// rejected because it means "wherever this process happens to run", which
// for a schedule provisioned into a cloud account is not a location at
// all; "UTC" is accepted because it is an unambiguous, DST-free choice
// somebody may legitimately want.
func ValidTimezone(v string) bool {
	if v == "" || v == "Local" {
		return false
	}
	_, err := time.LoadLocation(v)
	return err == nil
}

// parseClock splits a validated "HH:MM" into its components.
func parseClock(v string) (hour, min int, ok bool) {
	if !ValidClockTime(v) {
		return 0, 0, false
	}
	parts := strings.SplitN(v, ":", 2)
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return h, m, true
}

// boolOrDefault resolves a tri-state *bool. Deliberately a local copy
// rather than an import of internal/provider/decode: this package sits
// below the provider layer and must not depend on it.
func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
