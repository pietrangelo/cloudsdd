// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import (
	"fmt"
	"sort"
	"time"
)

// Action is what a Rule does to the resource when it fires.
type Action string

const (
	ActionStart Action = "start"
	ActionStop  Action = "stop"
)

// Rule is one provider-agnostic scheduling instruction.
//
// A Rule stays structured rather than carrying a cron string: AWS uses a
// six-field cron() expression with a mandatory year field, GCP and Azure
// use five-field unix cron. Rendering the expression belongs to each
// provider; deciding when things happen belongs here (RFC 012 §2.2).
//
// ValidFrom and ValidTo describe the half-open interval [ValidFrom,
// ValidTo) during which the rule is live; nil means unbounded on that
// side. Providers whose API cannot express a bounded interval must reject
// such a rule rather than dropping the bound (RFC 012 §1.3).
type Rule struct {
	// Name is stable and deterministic for a given Schedule, so that
	// re-applying an unchanged Specification updates the existing
	// scheduling resources instead of creating duplicates.
	Name   string
	Action Action

	// Days lists the weekdays the rule fires on. Empty means every day.
	Days []Weekday

	Hour, Min int

	// Timezone is the IANA zone name the wall-clock time is interpreted
	// in. It is passed through to the provider primitive verbatim;
	// CloudSDD never precomputes a UTC offset, which would be correct for
	// only half the year (RFC 012 §7).
	Timezone string

	ValidFrom *time.Time
	ValidTo   *time.Time
}

// Bounded reports whether the rule is restricted to a time interval, which
// is what a provider without validity-window support has to reject.
func (r Rule) Bounded() bool { return r.ValidFrom != nil || r.ValidTo != nil }

// window is a validated exception window, resolved to instants.
type window struct {
	mode ScheduleMode
	// start is midnight at the beginning of From; end is midnight at the
	// beginning of the day *after* To, making the interval half-open and
	// the inclusive user-facing dates exact.
	start, end time.Time
}

// Compile turns a Schedule into the rule set that realizes it.
//
// It is a pure function: no clock is read, no environment consulted, no
// I/O performed. A disabled or nil Schedule compiles to no rules rather
// than to an error, so callers can hand it the result of
// spec.EffectiveSchedule unconditionally.
func Compile(s *Schedule) ([]Rule, error) {
	if !s.IsEnabled() {
		return nil, nil
	}

	if s.Timezone == "" {
		return nil, ErrTimezoneRequired
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || s.Timezone == "Local" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTimezone, s.Timezone)
	}

	if s.Start == "" {
		return nil, ErrStartRequired
	}
	if s.Stop == "" {
		return nil, ErrStopRequired
	}
	startHour, startMin, ok := parseClock(s.Start)
	if !ok {
		return nil, fmt.Errorf("%w: start %q", ErrInvalidClockTime, s.Start)
	}
	stopHour, stopMin, ok := parseClock(s.Stop)
	if !ok {
		return nil, fmt.Errorf("%w: stop %q", ErrInvalidClockTime, s.Stop)
	}

	// A start later than a stop is an overnight window ("22:00" to
	// "06:00") and is deliberately accepted: the start fires in the
	// evening and the stop the following morning.
	days := s.EffectiveDays()
	if len(days) == 0 {
		return nil, ErrNoActiveDays
	}

	windows, err := normalizeWindows(s.Exceptions, loc)
	if err != nil {
		return nil, err
	}

	rules := make([]Rule, 0, 2*(len(windows)+1)+len(windows))

	// The weekly rhythm, segmented around the exception windows: each
	// window suspends the base rules for its duration rather than
	// competing with them (RFC 012 §2.2 step 3).
	var segStart *time.Time
	for i := 0; i <= len(windows); i++ {
		var segEnd *time.Time
		if i < len(windows) {
			segEnd = &windows[i].start
		}
		if segStart != nil && segEnd != nil && !segStart.Before(*segEnd) {
			// Adjacent windows leave a zero-length segment behind.
			segStart = &windows[i].end
			continue
		}
		rules = append(rules,
			Rule{
				Name:      fmt.Sprintf("start-base-%d", i),
				Action:    ActionStart,
				Days:      days,
				Hour:      startHour,
				Min:       startMin,
				Timezone:  s.Timezone,
				ValidFrom: segStart,
				ValidTo:   segEnd,
			},
			Rule{
				Name:      fmt.Sprintf("stop-base-%d", i),
				Action:    ActionStop,
				Days:      days,
				Hour:      stopHour,
				Min:       stopMin,
				Timezone:  s.Timezone,
				ValidFrom: segStart,
				ValidTo:   segEnd,
			},
		)
		if i < len(windows) {
			segStart = &windows[i].end
		}
	}

	for i, w := range windows {
		rules = append(rules, windowRule(i, w, s.Timezone))
	}

	return rules, nil
}

// windowRule builds the single rule that realizes an exception window.
//
// The two modes are deliberately asymmetric:
//
//   - always_on fires once, at the first midnight of the window. Nothing
//     stops the resource for the rest of the window — the base stop rules
//     are suspended over it — so repeating the start would only produce a
//     daily failed invocation against an already-running resource.
//
//   - always_off fires *daily* for the whole window. AWS automatically
//     restarts an RDS instance that has been stopped for more than seven
//     days, so a one-shot stop at the beginning of a two-week company
//     shutdown would leave the instance running, and billing, for the
//     second week. A daily stop re-stops it the morning after each
//     automatic restart (RFC 012 §2.2 step 5).
//
// Both fire at midnight, and both leave Days empty, meaning every day.
func windowRule(i int, w window, timezone string) Rule {
	if w.mode == ModeAlwaysOn {
		firstDayEnd := w.start.AddDate(0, 0, 1)
		return Rule{
			Name:      fmt.Sprintf("start-window-%d", i),
			Action:    ActionStart,
			Hour:      0,
			Min:       0,
			Timezone:  timezone,
			ValidFrom: &w.start,
			ValidTo:   &firstDayEnd,
		}
	}
	return Rule{
		Name:      fmt.Sprintf("stop-window-%d", i),
		Action:    ActionStop,
		Hour:      0,
		Min:       0,
		Timezone:  timezone,
		ValidFrom: &w.start,
		ValidTo:   &w.end,
	}
}

// normalizeWindows validates the exception windows and resolves them to
// instants in loc, sorted by start date.
//
// Sorting makes the compiled rule names a function of the *set* of
// windows rather than of the order the translator happened to emit them
// in, so reordering an unchanged specification does not churn the
// provisioned schedules.
func normalizeWindows(exceptions []Window, loc *time.Location) ([]window, error) {
	if len(exceptions) > MaxWindows {
		return nil, fmt.Errorf("%w: %d declared, at most %d allowed", ErrTooManyWindows, len(exceptions), MaxWindows)
	}

	windows := make([]window, 0, len(exceptions))
	for _, e := range exceptions {
		switch e.Mode {
		case ModeAlwaysOn, ModeAlwaysOff:
		default:
			return nil, fmt.Errorf("%w: %q", ErrInvalidWindowMode, e.Mode)
		}

		from, ok := parseDate(e.From, loc)
		if !ok {
			return nil, fmt.Errorf("%w: from %q", ErrInvalidWindowDate, e.From)
		}
		to, ok := parseDate(e.To, loc)
		if !ok {
			return nil, fmt.Errorf("%w: to %q", ErrInvalidWindowDate, e.To)
		}
		if to.Before(from) {
			return nil, fmt.Errorf("%w: %q to %q", ErrInvertedWindow, e.From, e.To)
		}

		windows = append(windows, window{
			mode:  e.Mode,
			start: from,
			// To is inclusive for the user, so the half-open interval
			// ends at midnight of the following day.
			end: to.AddDate(0, 0, 1),
		})
	}

	sort.Slice(windows, func(i, j int) bool { return windows[i].start.Before(windows[j].start) })

	for i := 1; i < len(windows); i++ {
		if windows[i].start.Before(windows[i-1].end) {
			return nil, fmt.Errorf("%w: %s and %s",
				ErrOverlappingWindows,
				windows[i-1].start.Format(dateLayout),
				windows[i].start.Format(dateLayout))
		}
	}

	return windows, nil
}

// parseDate resolves a YYYY-MM-DD date to midnight in loc.
//
// On the handful of zones that shift their clock at midnight, that instant
// may not exist; time.Date normalizes it to the nearest real one, which is
// the behavior a scheduler would apply anyway.
func parseDate(v string, loc *time.Location) (time.Time, bool) {
	if !ValidDate(v) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(dateLayout, v, loc)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc), true
}
