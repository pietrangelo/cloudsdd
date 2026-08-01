// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// dayLabels are the display names for the weekdays, in calendar order.
var dayLabels = map[Weekday]string{
	Mon: "Mon", Tue: "Tue", Wed: "Wed", Thu: "Thu",
	Fri: "Fri", Sat: "Sat", Sun: "Sun",
}

// Describe renders a Schedule as the prose summary shown before the
// confirmation gate.
//
// The user approves a schedule in words, not in cron: the whole point of
// the gate is that somebody can tell at a glance that an environment is
// about to start powering down at 19:00, and a six-field cron expression
// does not make that legible (RFC 012 §5).
func Describe(s *Schedule) string {
	if !s.IsEnabled() {
		return "no schedule (always on)"
	}

	days := s.EffectiveDays()

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %s (%s)", describeDays(days), s.Start, s.Stop, s.Timezone)
	if !containsDay(days, Sat) && !containsDay(days, Sun) {
		b.WriteString(", weekend off")
	}

	exceptions := append([]Window(nil), s.Exceptions...)
	sort.SliceStable(exceptions, func(i, j int) bool { return exceptions[i].From < exceptions[j].From })
	for _, e := range exceptions {
		mode := "always off"
		if e.Mode == ModeAlwaysOn {
			mode = "always on"
		}
		fmt.Fprintf(&b, "\n  %s to %s: %s", e.From, e.To, mode)
		if reason := sanitize(e.Reason); reason != "" {
			fmt.Fprintf(&b, " (%s)", reason)
		}
	}

	return b.String()
}

// describeDays collapses a day set into runs ("Mon-Fri", "Mon-Wed, Fri").
func describeDays(days []Weekday) string {
	if len(days) == 0 {
		return "no days"
	}
	if len(days) == len(weekOrder) {
		return "every day"
	}

	var (
		parts []string
		run   []Weekday
	)
	flush := func() {
		switch len(run) {
		case 0:
			return
		case 1:
			parts = append(parts, dayLabels[run[0]])
		case 2:
			parts = append(parts, dayLabels[run[0]], dayLabels[run[1]])
		default:
			parts = append(parts, dayLabels[run[0]]+"-"+dayLabels[run[len(run)-1]])
		}
		run = nil
	}

	// days arrives in calendar order from EffectiveDays, so a run is
	// simply a maximal stretch of adjacent entries in weekOrder.
	prev := -2
	for _, d := range days {
		idx := indexOfDay(d)
		if idx != prev+1 {
			flush()
		}
		run = append(run, d)
		prev = idx
	}
	flush()

	return strings.Join(parts, ", ")
}

func indexOfDay(d Weekday) int {
	for i, w := range weekOrder {
		if w == d {
			return i
		}
	}
	return -1
}

func containsDay(days []Weekday, want Weekday) bool {
	for _, d := range days {
		if d == want {
			return true
		}
	}
	return false
}

// sanitize strips control characters from text that originated in a
// natural language prompt and travels through the ledger.
//
// Window.Reason is echoed straight to the operator's terminal. Without
// this, a reason containing ANSI escape sequences could repaint or hide
// the very plan output the confirmation gate exists to let the user read
// (RFC 012 §7).
func sanitize(v string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	return strings.TrimSpace(cleaned)
}
