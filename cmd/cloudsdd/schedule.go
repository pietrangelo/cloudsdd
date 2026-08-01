// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Scheduling flags. They exist so the feature is usable from CI, where
// there is nobody to answer a prompt (RFC 012 §5).
var (
	scheduleStart    string
	scheduleStop     string
	scheduleTimezone string
	scheduleDays     string
	noSchedule       bool
)

// registerScheduleFlags attaches the scheduling flags to a command.
func registerScheduleFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&scheduleStart, "schedule-start", "", "power-on time for scheduled resources, HH:MM")
	f.StringVar(&scheduleStop, "schedule-stop", "", "power-off time for scheduled resources, HH:MM")
	f.StringVar(&scheduleTimezone, "schedule-timezone", "", "IANA timezone the schedule runs in, e.g. Europe/Rome")
	f.StringVar(&scheduleDays, "schedule-days", "", "comma-separated days the resources are powered on, e.g. mon,tue,wed,thu,fri (default mon-fri)")
	f.BoolVar(&noSchedule, "no-schedule", false, "drop any power schedule the translation proposed")
}

// ErrScheduleTimesRequired indicates that scheduling was requested but the
// times were neither supplied on the command line nor available to be
// asked for.
//
// CloudSDD does not guess here. A default start time would be a harmless
// mistake; a default stop time takes an environment down while people are
// working in it (RFC 012 §1.2).
var ErrScheduleTimesRequired = errors.New("a power schedule was requested but its times are unknown")

// resolveSchedules settles the power schedule before the plan runs, so
// that the Specification the user approves already contains the times
// that will be provisioned.
//
// Order matters: --no-schedule wins over everything, then explicit flags,
// then whatever is still missing is asked for.
func resolveSchedules(cmd *cobra.Command, in *bufio.Reader, s *spec.Specification, intent spec.Intent) error {
	if noSchedule {
		stripSchedules(s)
		return nil
	}

	// Destroying a resource removes its schedules along with it: they
	// live in the same Pulumi stack, and Pulumi tears down what the state
	// records rather than what the program declares. Carrying an
	// incomplete schedule into a destroy would mean interrogating the
	// user about the working hours of something they are deleting.
	if intent == spec.IntentDestroy {
		stripSchedules(s)
		return nil
	}

	applyScheduleFlags(s)

	pending := incompleteSchedules(s)
	if len(pending) == 0 {
		return nil
	}
	return elicitSchedule(cmd, in, s, pending)
}

// stripSchedules removes every power schedule from the Specification.
func stripSchedules(s *spec.Specification) {
	s.Policies.Schedule = nil
	for i := range s.Resources {
		s.Resources[i].Schedule = nil
	}
}

// applyScheduleFlags writes the command-line values into the
// Specification.
//
// A flag is an explicit instruction, so it overrides whatever the
// translator proposed rather than merely filling a gap, and it applies to
// every enabled schedule in the Specification: "--schedule-stop 20:00"
// means everything scheduled stops at 20:00. Supplying any scheduling
// flag on a Specification with no schedule at all creates one, so a
// schedule can be requested entirely from the command line.
func applyScheduleFlags(s *spec.Specification) {
	if scheduleStart == "" && scheduleStop == "" && scheduleTimezone == "" && scheduleDays == "" {
		return
	}

	targets := enabledSchedules(s)
	if len(targets) == 0 {
		enabled := true
		s.Policies.Schedule = &schedule.Schedule{Enabled: &enabled}
		targets = []*schedule.Schedule{s.Policies.Schedule}
	}

	for _, sch := range targets {
		if scheduleStart != "" {
			sch.Start = scheduleStart
		}
		if scheduleStop != "" {
			sch.Stop = scheduleStop
		}
		if scheduleTimezone != "" {
			sch.Timezone = scheduleTimezone
		}
		if scheduleDays != "" {
			sch.Days = parseDays(scheduleDays)
		}
	}
}

// enabledSchedules collects every schedule in the Specification that is
// switched on, policy-wide first.
func enabledSchedules(s *spec.Specification) []*schedule.Schedule {
	var out []*schedule.Schedule
	if s.Policies.Schedule.IsEnabled() {
		out = append(out, s.Policies.Schedule)
	}
	for i := range s.Resources {
		if s.Resources[i].Schedule.IsEnabled() {
			out = append(out, s.Resources[i].Schedule)
		}
	}
	return out
}

// activeSchedules returns the distinct schedules that will actually be
// provisioned: enabled, and governing at least one resource whose type
// has a power state.
//
// The distinction from enabledSchedules matters at the prompt. A
// Specification-wide schedule over nothing but object stores is enabled
// but inapplicable, and asking the user for the working hours of an
// environment that will never power down is worse than not asking.
func activeSchedules(s *spec.Specification) []*schedule.Schedule {
	var out []*schedule.Schedule
	seen := map[*schedule.Schedule]bool{}
	for _, r := range s.Resources {
		if !r.Type.SupportsSchedule() {
			continue
		}
		sch := spec.EffectiveSchedule(r, s.Policies)
		if !sch.IsEnabled() || seen[sch] {
			continue
		}
		seen[sch] = true
		out = append(out, sch)
	}
	return out
}

// incompleteSchedules returns the schedules that will be provisioned but
// are still missing a value only the user can supply.
func incompleteSchedules(s *spec.Specification) []*schedule.Schedule {
	var out []*schedule.Schedule
	for _, sch := range activeSchedules(s) {
		if sch.Start == "" || sch.Stop == "" || sch.Timezone == "" {
			out = append(out, sch)
		}
	}
	return out
}

// maxPromptAttempts bounds the re-ask loop, so a non-interactive stream
// feeding garbage cannot spin forever.
const maxPromptAttempts = 3

// elicitSchedule asks the user for the times the translator was
// deliberately not allowed to invent.
func elicitSchedule(cmd *cobra.Command, in *bufio.Reader, s *spec.Specification, pending []*schedule.Schedule) error {
	out := cmd.OutOrStdout()

	if assumeYes {
		return fmt.Errorf("%w: pass --schedule-start, --schedule-stop and --schedule-timezone, or --no-schedule", ErrScheduleTimesRequired)
	}

	fmt.Fprintf(out, "\nThis deployment includes a power schedule: %s will be shut down outside working hours.\n",
		strings.Join(scheduledResourceIDs(s), ", "))

	start, err := ask(in, out, "  Start time (HH:MM)", "", schedule.ValidClockTime)
	if err != nil {
		return err
	}
	stop, err := ask(in, out, "  Stop time (HH:MM)", "", schedule.ValidClockTime)
	if err != nil {
		return err
	}
	// The operator's own zone is the most likely answer, but it is shown
	// for confirmation rather than assumed: the schedule runs in a cloud
	// account, not on this host.
	timezone, err := ask(in, out, "  Timezone", localTimezone(), schedule.ValidTimezone)
	if err != nil {
		return err
	}
	days, err := ask(in, out, "  Days", "mon,tue,wed,thu,fri", validDayList)
	if err != nil {
		return err
	}

	for _, sch := range pending {
		if sch.Start == "" {
			sch.Start = start
		}
		if sch.Stop == "" {
			sch.Stop = stop
		}
		if sch.Timezone == "" {
			sch.Timezone = timezone
		}
		if len(sch.Days) == 0 {
			sch.Days = parseDays(days)
		}
	}

	return nil
}

// ask prompts for a single value, re-asking on invalid input.
//
// An empty answer accepts def when there is one; where there is no
// default — the start and stop times — an empty answer is invalid, which
// is what keeps "just press enter" from silently scheduling an outage.
func ask(in *bufio.Reader, out io.Writer, label, def string, valid func(string) bool) (string, error) {
	for attempt := 0; attempt < maxPromptAttempts; attempt++ {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(out, "%s: ", label)
		}

		line, err := in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" && def != "" {
			answer = def
		}
		if valid(answer) {
			return answer, nil
		}
		if err != nil {
			// EOF with nothing usable: a non-interactive invocation that
			// did not pass the flags.
			return "", fmt.Errorf("%w: %s could not be read; pass the --schedule-* flags or --no-schedule", ErrScheduleTimesRequired, strings.TrimSpace(label))
		}
		fmt.Fprintf(out, "    %q is not valid.\n", answer)
	}
	return "", fmt.Errorf("%w: %s was not provided", ErrScheduleTimesRequired, strings.TrimSpace(label))
}

// localTimezone returns this host's IANA zone name, or empty when it
// cannot be expressed as one (a fixed offset, or a stripped container).
func localTimezone() string {
	name := time.Local.String()
	if !schedule.ValidTimezone(name) {
		return ""
	}
	return name
}

// parseDays splits a comma-separated day list into weekdays. Unrecognized
// entries are dropped here and rejected downstream: schedule.Compile
// fails with ErrNoActiveDays rather than quietly powering the environment
// on every day of the week.
func parseDays(v string) []schedule.Weekday {
	var out []schedule.Weekday
	for _, part := range strings.Split(v, ",") {
		day := schedule.Weekday(strings.ToLower(strings.TrimSpace(part)))
		if _, ok := day.ToTime(); ok {
			out = append(out, day)
		}
	}
	return out
}

// validDayList reports whether every entry in a comma-separated list is a
// weekday, so a typo is caught at the prompt instead of silently
// shrinking the week.
func validDayList(v string) bool {
	parts := strings.Split(v, ",")
	for _, part := range parts {
		day := schedule.Weekday(strings.ToLower(strings.TrimSpace(part)))
		if _, ok := day.ToTime(); !ok {
			return false
		}
	}
	return len(parts) > 0
}

// scheduledResourceIDs lists the resources a schedule will actually be
// provisioned for, for the prompt's opening line. It is only reached when
// incompleteSchedules found at least one, so it is never empty.
func scheduledResourceIDs(s *spec.Specification) []string {
	var ids []string
	for _, r := range s.Resources {
		if r.Type.SupportsSchedule() && spec.EffectiveSchedule(r, s.Policies).IsEnabled() {
			ids = append(ids, r.ID)
		}
	}
	return ids
}
