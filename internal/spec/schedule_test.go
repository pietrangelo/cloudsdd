// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"strings"
	"testing"

	"cloudsdd/internal/schedule"
)

func boolPtr(b bool) *bool { return &b }

func enabledSchedule() *schedule.Schedule {
	return &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
}

func TestEffectiveSchedule(t *testing.T) {
	policy := enabledSchedule()
	override := &schedule.Schedule{Enabled: boolPtr(true), Timezone: "UTC", Start: "06:00", Stop: "22:00"}
	optOut := &schedule.Schedule{Enabled: boolPtr(false)}

	tests := []struct {
		name     string
		resource *schedule.Schedule
		policies *schedule.Schedule
		want     *schedule.Schedule
	}{
		{name: "neither declared", want: nil},
		{name: "inherited from policies", policies: policy, want: policy},
		{name: "resource with no policy default", resource: override, want: override},
		{name: "resource overrides policies", resource: override, policies: policy, want: override},
		{
			// The reviewable opt-out: a production resource must be able
			// to refuse a specification-wide schedule.
			name:     "resource opts out of an inherited schedule",
			resource: optOut,
			policies: policy,
			want:     optOut,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Resource{ID: "app-db", Schedule: tt.resource}
			p := Policies{Schedule: tt.policies}

			if got := EffectiveSchedule(r, p); got != tt.want {
				t.Errorf("EffectiveSchedule() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEffectiveScheduleOverrideIsWholesale(t *testing.T) {
	// A resource-level schedule replaces the inherited one entirely. A
	// field-level merge would let this resource silently inherit the
	// policy's 19:00 stop, which its author never wrote down.
	policies := Policies{Schedule: enabledSchedule()}
	r := Resource{ID: "app-db", Schedule: &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "UTC",
		Start:    "06:00",
		Stop:     "22:00",
	}}

	got := EffectiveSchedule(r, policies)
	if got.Stop != "22:00" {
		t.Errorf("Stop = %q, want the resource's own 22:00", got.Stop)
	}
	if got.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want the resource's own UTC", got.Timezone)
	}
}

func TestResourceTypeSupportsSchedule(t *testing.T) {
	tests := []struct {
		in   ResourceType
		want bool
	}{
		{in: ResourceTypeRelationalDatabase, want: true},
		// Declared by the schema but implemented by no provider yet.
		// Listed so that the day one lands, scheduling is not silently
		// rejected.
		{in: ResourceTypeComputeInstance, want: true},
		{in: ResourceTypeContainerService, want: true},
		// An object store cannot be switched off, and an IAM role costs
		// nothing to leave in place.
		{in: ResourceTypeObjectStorage, want: false},
		{in: ResourceTypeCrossAccountRole, want: false},
		{in: "", want: false},
		{in: "quantum_computer", want: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			if got := tt.in.SupportsSchedule(); got != tt.want {
				t.Errorf("%q.SupportsSchedule() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateSchedule(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *Specification)
		wantErr bool
	}{
		{
			name:    "no schedule at all",
			mutate:  func(s *Specification) {},
			wantErr: false,
		},
		{
			name:    "valid policy schedule",
			mutate:  func(s *Specification) { s.Policies.Schedule = enabledSchedule() },
			wantErr: false,
		},
		{
			name:    "valid resource schedule",
			mutate:  func(s *Specification) { s.Resources[0].Schedule = enabledSchedule() },
			wantErr: false,
		},
		{
			name: "valid schedule with exception windows",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = []schedule.Window{
					{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff, Reason: "shutdown"},
				}
				s.Policies.Schedule = sch
			},
			wantErr: false,
		},
		{
			name: "malformed start time",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Start = "8am"
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "out-of-range stop time",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Stop = "25:00"
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "fixed offset instead of an IANA zone",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Timezone = "+02:00"
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "unknown weekday",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Days = []schedule.Weekday{"caturday"}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "duplicate weekdays",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Days = []schedule.Weekday{schedule.Mon, schedule.Mon}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "window with a malformed date",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = []schedule.Window{{From: "24/12/2026", To: "2027-01-06", Mode: schedule.ModeAlwaysOff}}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "window with an unknown mode",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = []schedule.Window{{From: "2026-12-24", To: "2027-01-06", Mode: "maybe"}}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "window missing a bound",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = []schedule.Window{{From: "2026-12-24", Mode: schedule.ModeAlwaysOff}}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "more windows than the cap",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = make([]schedule.Window, schedule.MaxWindows+1)
				for i := range sch.Exceptions {
					sch.Exceptions[i] = schedule.Window{From: "2026-12-24", To: "2026-12-24", Mode: schedule.ModeAlwaysOff}
				}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
		{
			name: "oversized reason",
			mutate: func(s *Specification) {
				sch := enabledSchedule()
				sch.Exceptions = []schedule.Window{{
					From:   "2026-12-24",
					To:     "2027-01-06",
					Mode:   schedule.ModeAlwaysOff,
					Reason: strings.Repeat("a", 129),
				}}
				s.Policies.Schedule = sch
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)

			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseRejectsUnknownScheduleFields(t *testing.T) {
	// Mass-assignment defense reaches into the nested schedule block: a
	// property the engine does not understand means the user asked for
	// something they are not going to get.
	const payload = `{
	  "sdd_version": "1.0",
	  "intent": "deploy",
	  "resources": [
	    {"id": "app-db", "type": "relational_database", "provider": "aws", "properties": {"engine": "postgres"}}
	  ],
	  "policies": {
	    "schedule": {"enabled": true, "start": "08:00", "stop": "19:00", "timezone": "Europe/Rome", "grace_period": "1h"}
	  }
	}`

	if _, err := Parse(strings.NewReader(payload)); err == nil {
		t.Fatal("Parse() accepted an unknown field inside schedule, want an error")
	}
}

func TestParseAcceptsAScheduledSpecification(t *testing.T) {
	const payload = `{
	  "sdd_version": "1.0",
	  "intent": "deploy",
	  "resources": [
	    {
	      "id": "app-db",
	      "type": "relational_database",
	      "provider": "aws",
	      "scope": {"environment": "dev", "region": "eu-central-1"},
	      "properties": {"engine": "postgres", "version": "15"}
	    },
	    {
	      "id": "prod-db",
	      "type": "relational_database",
	      "provider": "aws",
	      "scope": {"environment": "prod", "region": "eu-central-1"},
	      "schedule": {"enabled": false},
	      "properties": {"engine": "postgres", "version": "15"}
	    }
	  ],
	  "policies": {
	    "allowed_regions": ["eu-central-1"],
	    "schedule": {
	      "enabled": true,
	      "timezone": "Europe/Rome",
	      "start": "08:00",
	      "stop": "19:00",
	      "days": ["mon", "tue", "wed", "thu", "fri"],
	      "exceptions": [
	        {"from": "2026-12-24", "to": "2027-01-06", "mode": "always_off", "reason": "company shutdown"}
	      ]
	    }
	  }
	}`

	s, err := ParseAndValidate(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseAndValidate() error = %v", err)
	}

	if got := EffectiveSchedule(s.Resources[0], s.Policies); !got.IsEnabled() {
		t.Error("the first resource should inherit the enabled policy schedule")
	}
	if got := EffectiveSchedule(s.Resources[1], s.Policies); got.IsEnabled() {
		t.Error("the second resource declares enabled:false and must not be scheduled")
	}
}

func TestValidateProviderPreference(t *testing.T) {
	tests := []struct {
		name    string
		in      []Provider
		wantErr bool
	}{
		{name: "absent", in: nil},
		{name: "single", in: []Provider{ProviderAWS}},
		{name: "ordered list", in: []Provider{ProviderGCP, ProviderAWS, ProviderAzure}},
		{
			// A preference that could name "agnostic" would be circular:
			// it is the value resolution exists to replace (RFC 014 §2.1).
			name:    "agnostic is circular",
			in:      []Provider{ProviderAgnostic},
			wantErr: true,
		},
		{name: "unknown provider", in: []Provider{"oracle"}, wantErr: true},
		{
			// A duplicate is either a typo or a misunderstanding of how
			// the order is read; neither should pass silently.
			name:    "duplicates",
			in:      []Provider{ProviderAWS, ProviderAWS},
			wantErr: true,
		},
		{
			name:    "more entries than there are providers",
			in:      []Provider{ProviderAWS, ProviderGCP, ProviderAzure, "oracle"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			s.Policies.ProviderPreference = tt.in

			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
