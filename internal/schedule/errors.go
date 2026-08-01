// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package schedule

import "errors"

// ErrTimezoneRequired indicates that scheduling was enabled without an
// IANA timezone (RFC 012 §1.2). CloudSDD never falls back to the machine's
// local zone: the schedule runs in a cloud account, not on this host.
var ErrTimezoneRequired = errors.New("schedule: timezone is required when scheduling is enabled")

// ErrInvalidTimezone indicates a timezone that is not a resolvable IANA
// zone name.
var ErrInvalidTimezone = errors.New("schedule: timezone must be an IANA zone name (e.g. Europe/Rome)")

// ErrStartRequired and ErrStopRequired indicate that scheduling was
// enabled without the times that say when. They are never defaulted: an
// invented stop time is an outage (RFC 012 §1.2).
var (
	ErrStartRequired = errors.New("schedule: start time is required when scheduling is enabled")
	ErrStopRequired  = errors.New("schedule: stop time is required when scheduling is enabled")
)

// ErrInvalidClockTime indicates a start or stop time that is not "HH:MM".
var ErrInvalidClockTime = errors.New("schedule: time must be in HH:MM 24-hour format")

// ErrNoActiveDays indicates a schedule whose Days resolved to an empty
// set. An enabled schedule that never powers the environment on is a
// destroy in disguise; the user must say so explicitly instead.
var ErrNoActiveDays = errors.New("schedule: at least one active day is required")

// ErrInvalidWindowDate indicates an exception window bound that is not a
// YYYY-MM-DD calendar date.
var ErrInvalidWindowDate = errors.New("schedule: exception window dates must be in YYYY-MM-DD format")

// ErrInvertedWindow indicates an exception window whose end precedes its
// start.
var ErrInvertedWindow = errors.New("schedule: exception window ends before it starts")

// ErrOverlappingWindows indicates two exception windows covering a common
// day. Rather than inventing a precedence rule the user would have to
// guess at, ambiguity is rejected (RFC 012 §2.2 step 2).
var ErrOverlappingWindows = errors.New("schedule: exception windows overlap")

// ErrInvalidWindowMode indicates an exception window with an unrecognized
// mode.
var ErrInvalidWindowMode = errors.New("schedule: exception window mode must be always_on or always_off")

// ErrTooManyWindows indicates more exception windows than MaxWindows,
// which bounds the number of scheduling resources one Specification can
// provision.
var ErrTooManyWindows = errors.New("schedule: too many exception windows")
