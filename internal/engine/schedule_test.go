// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

func boolPtr(b bool) *bool { return &b }

func workWeekSchedule() *schedule.Schedule {
	return &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
}

// scheduledSpec builds a single-resource Specification of the given type,
// with the schedule declared where the test asks for it.
func scheduledSpec(t spec.ResourceType, resourceSchedule, policySchedule *schedule.Schedule) spec.Specification {
	return spec.Specification{
		SDDVersion: "1.0",
		Intent:     spec.IntentDeploy,
		Resources: []spec.Resource{
			{
				ID:         "app-db",
				Type:       t,
				Provider:   spec.ProviderAWS,
				Schedule:   resourceSchedule,
				Properties: map[string]any{"engine": "postgres"},
			},
		},
		Policies: spec.Policies{Schedule: policySchedule},
	}
}

func TestDefaultEngineValidatesSchedule(t *testing.T) {
	badTimezone := workWeekSchedule()
	badTimezone.Timezone = "Middle/Earth"

	overlapping := workWeekSchedule()
	overlapping.Exceptions = []schedule.Window{
		{From: "2026-09-12", To: "2026-09-20", Mode: schedule.ModeAlwaysOn},
		{From: "2026-09-18", To: "2026-09-25", Mode: schedule.ModeAlwaysOff},
	}

	missingTimes := &schedule.Schedule{Enabled: boolPtr(true), Timezone: "Europe/Rome"}

	// Semantically wrong but syntactically fine: every field passes its
	// struct tag, so only Compile can catch it.
	optedOutButIncoherent := &schedule.Schedule{
		Enabled:  boolPtr(false),
		Timezone: "Europe/Rome",
		Exceptions: []schedule.Window{
			{From: "2026-09-12", To: "2026-09-20", Mode: schedule.ModeAlwaysOn},
			{From: "2026-09-18", To: "2026-09-25", Mode: schedule.ModeAlwaysOff},
		},
	}

	tests := []struct {
		name string
		spec spec.Specification
		// wantErr is checked with errors.Is; wantAnErr covers the
		// failures the schema layer catches first, whose error is a
		// validator report rather than a sentinel.
		wantErr   error
		wantAnErr bool
		wantOK    bool
	}{
		{
			name:   "no schedule",
			spec:   scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, nil),
			wantOK: true,
		},
		{
			name:   "valid inherited schedule",
			spec:   scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, workWeekSchedule()),
			wantOK: true,
		},
		{
			name:   "valid resource schedule",
			spec:   scheduledSpec(spec.ResourceTypeRelationalDatabase, workWeekSchedule(), nil),
			wantOK: true,
		},
		{
			// Syntax is caught one layer earlier, by the schema tags,
			// before the engine ever compiles anything.
			name:      "unknown timezone",
			spec:      scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, badTimezone),
			wantAnErr: true,
		},
		{
			// Semantics are the engine's job: no struct tag can see that
			// two individually valid windows overlap.
			name:    "overlapping exception windows",
			spec:    scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, overlapping),
			wantErr: schedule.ErrOverlappingWindows,
		},
		{
			name:    "enabled without times",
			spec:    scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, missingTimes),
			wantErr: schedule.ErrStartRequired,
		},
		{
			// An explicit schedule on a resource that cannot honor it is
			// a request the user made and will not get.
			name:    "explicit schedule on an object store",
			spec:    scheduledSpec(spec.ResourceTypeObjectStorage, workWeekSchedule(), nil),
			wantErr: ErrResourceNotSchedulable,
		},
		{
			// An inherited one is merely inapplicable: without this,
			// no Specification could hold both a bucket and a database.
			name:   "inherited schedule on an object store is skipped",
			spec:   scheduledSpec(spec.ResourceTypeObjectStorage, nil, workWeekSchedule()),
			wantOK: true,
		},
		{
			// A schedule nobody will act on is not compiled: an opted-out
			// resource must not be held to rules that never fire.
			name:   "an opted-out resource is not compiled",
			spec:   scheduledSpec(spec.ResourceTypeRelationalDatabase, optedOutButIncoherent, workWeekSchedule()),
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: &mockProvider{name: "aws"}})

			err := e.Validate(context.Background(), tt.spec)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if tt.wantAnErr {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "app-db") {
				t.Errorf("Validate() error = %q, want it to name the offending resource", err)
			}
		})
	}
}

func TestDefaultEngineRejectsBadScheduleBeforeReachingTheProvider(t *testing.T) {
	// A schedule that cannot be compiled must never reach a provider: the
	// point of validating centrally is that nothing is provisioned first.
	bad := workWeekSchedule()
	bad.Stop = "25:00"

	p := &mockProvider{name: "aws"}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: p})

	if err := e.Validate(context.Background(), scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, bad)); err == nil {
		t.Fatal("Validate() = nil, want an error for an out-of-range stop time")
	}
	if p.validateCalls != 0 {
		t.Errorf("provider Validate called %d times, want 0", p.validateCalls)
	}
}

func TestScheduleStatuses(t *testing.T) {
	tests := []struct {
		name        string
		spec        spec.Specification
		wantIDs     []string
		wantApplied []bool
	}{
		{
			name: "unscheduled specification reports nothing",
			spec: scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, nil),
		},
		{
			name:        "a scheduled database",
			spec:        scheduledSpec(spec.ResourceTypeRelationalDatabase, nil, workWeekSchedule()),
			wantIDs:     []string{"app-db"},
			wantApplied: []bool{true},
		},
		{
			name:        "an inapplicable inherited schedule is still reported",
			spec:        scheduledSpec(spec.ResourceTypeObjectStorage, nil, workWeekSchedule()),
			wantIDs:     []string{"app-db"},
			wantApplied: []bool{false},
		},
		{
			name: "an opted-out resource reports nothing",
			spec: scheduledSpec(spec.ResourceTypeRelationalDatabase,
				&schedule.Schedule{Enabled: boolPtr(false)}, workWeekSchedule()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScheduleStatuses(tt.spec)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("ScheduleStatuses() = %+v, want %d entries", got, len(tt.wantIDs))
			}
			for i := range tt.wantIDs {
				if got[i].ResourceID != tt.wantIDs[i] {
					t.Errorf("entry %d resource = %q, want %q", i, got[i].ResourceID, tt.wantIDs[i])
				}
				if got[i].Applied != tt.wantApplied[i] {
					t.Errorf("entry %d applied = %v, want %v", i, got[i].Applied, tt.wantApplied[i])
				}
				if got[i].Summary == "" {
					t.Errorf("entry %d has an empty summary", i)
				}
			}
		})
	}
}

func TestScheduleStatusesExplainsAnInapplicableSchedule(t *testing.T) {
	got := ScheduleStatuses(scheduledSpec(spec.ResourceTypeObjectStorage, nil, workWeekSchedule()))
	if len(got) != 1 {
		t.Fatalf("ScheduleStatuses() = %+v, want one entry", got)
	}
	if !strings.Contains(got[0].Summary, "stays up") {
		t.Errorf("summary = %q, want it to say the resource stays up", got[0].Summary)
	}
}
