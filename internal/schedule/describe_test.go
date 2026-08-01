// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import (
	"strings"
	"testing"
)

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		in   *Schedule
		want string
	}{
		{
			name: "nil schedule",
			in:   nil,
			want: "no schedule (always on)",
		},
		{
			name: "explicitly disabled",
			in:   &Schedule{Enabled: boolPtr(false), Start: "08:00", Stop: "19:00"},
			want: "no schedule (always on)",
		},
		{
			name: "work week",
			in:   ptrTo(baseSchedule()),
			want: "Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off",
		},
		{
			name: "non-contiguous days",
			in: func() *Schedule {
				s := baseSchedule()
				s.Days = []Weekday{Mon, Wed, Fri}
				return &s
			}(),
			want: "Mon, Wed, Fri 08:00 -> 19:00 (Europe/Rome), weekend off",
		},
		{
			name: "a run of two days is listed rather than hyphenated",
			in: func() *Schedule {
				s := baseSchedule()
				s.Days = []Weekday{Mon, Tue}
				return &s
			}(),
			want: "Mon, Tue 08:00 -> 19:00 (Europe/Rome), weekend off",
		},
		{
			name: "a working weekend is not described as weekend off",
			in: func() *Schedule {
				s := baseSchedule()
				s.Days = []Weekday{Sat, Sun}
				return &s
			}(),
			want: "Sat, Sun 08:00 -> 19:00 (Europe/Rome)",
		},
		{
			name: "every day",
			in: func() *Schedule {
				s := baseSchedule()
				s.Days = weekOrder
				return &s
			}(),
			want: "every day 08:00 -> 19:00 (Europe/Rome)",
		},
		{
			name: "mixed runs",
			in: func() *Schedule {
				s := baseSchedule()
				s.Days = []Weekday{Mon, Tue, Wed, Sat}
				return &s
			}(),
			want: "Mon-Wed, Sat 08:00 -> 19:00 (Europe/Rome)",
		},
		{
			name: "exception windows are listed in date order",
			in: func() *Schedule {
				s := baseSchedule()
				s.Exceptions = []Window{
					{From: "2026-12-24", To: "2027-01-06", Mode: ModeAlwaysOff, Reason: "company shutdown"},
					{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn, Reason: "release weekend"},
				}
				return &s
			}(),
			want: "Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off" +
				"\n  2026-09-12 to 2026-09-14: always on (release weekend)" +
				"\n  2026-12-24 to 2027-01-06: always off (company shutdown)",
		},
		{
			name: "a window without a reason",
			in: func() *Schedule {
				s := baseSchedule()
				s.Exceptions = []Window{{From: "2026-09-12", To: "2026-09-14", Mode: ModeAlwaysOn}}
				return &s
			}(),
			want: "Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off" +
				"\n  2026-09-12 to 2026-09-14: always on",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Describe(tt.in); got != tt.want {
				t.Errorf("Describe() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestDescribeStripsControlCharactersFromReason(t *testing.T) {
	// Reason originates in a natural language prompt and is echoed back
	// through the ledger. Printed raw, an escape sequence could repaint
	// or hide the plan output the confirmation gate exists to show.
	s := baseSchedule()
	s.Exceptions = []Window{{
		From:   "2026-09-12",
		To:     "2026-09-14",
		Mode:   ModeAlwaysOn,
		Reason: "release\x1b[2J\x1b[H weekend\nstop: 03:00",
	}}

	got := Describe(&s)
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("Describe() leaked an escape character: %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("Describe() = %q, want the reason confined to a single line", got)
	}
}

func ptrTo(s Schedule) *Schedule { return &s }
