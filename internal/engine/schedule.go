// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"fmt"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// validateSchedule resolves and compiles the power schedule governing r,
// before any provider is reached (RFC 012 §2.3).
//
// Compiling at validation time means an unrepresentable schedule — an
// unknown timezone, overlapping exception windows, a stop time that is
// not a time — is reported while nothing has been provisioned, rather
// than surfacing as infrastructure that powers down at the wrong hour.
func validateSchedule(r spec.Resource, policies spec.Policies) error {
	sch := spec.EffectiveSchedule(r, policies)
	if !sch.IsEnabled() {
		return nil
	}

	if !r.Type.SupportsSchedule() {
		// An explicit schedule on a resource that cannot honor it is a
		// request the user made and will not get. An inherited one is
		// merely inapplicable: the CLI reports it in the plan output
		// (see ScheduleStatus) and the resource is left running.
		if r.Schedule != nil {
			return fmt.Errorf("%w: resource %q is a %q", ErrResourceNotSchedulable, r.ID, r.Type)
		}
		return nil
	}

	if _, err := schedule.Compile(sch); err != nil {
		return fmt.Errorf("engine: resource %q: %w", r.ID, err)
	}
	return nil
}

// ScheduleStatus describes how the effective schedule applies to a
// resource, for display alongside the plan.
type ScheduleStatus struct {
	ResourceID string
	// Summary is the human-readable rendering of the schedule.
	Summary string
	// Applied is false when a schedule inherited from Policies cannot be
	// honored by this resource type. It is reported rather than silently
	// dropped: the user asked for an environment to power down, and needs
	// to know which parts of it will not.
	Applied bool
}

// ScheduleStatuses summarizes the effective schedule of every resource
// that has one, in Specification order. Resources with no schedule are
// omitted entirely, so an unscheduled Specification produces no output.
func ScheduleStatuses(s spec.Specification) []ScheduleStatus {
	var out []ScheduleStatus
	for _, r := range s.Resources {
		sch := spec.EffectiveSchedule(r, s.Policies)
		if !sch.IsEnabled() {
			continue
		}
		applied := r.Type.SupportsSchedule()
		summary := schedule.Describe(sch)
		if !applied {
			summary = fmt.Sprintf("not applicable to %s, this resource stays up", r.Type)
		}
		out = append(out, ScheduleStatus{ResourceID: r.ID, Summary: summary, Applied: applied})
	}
	return out
}
