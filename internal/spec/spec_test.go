// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import "testing"

func TestScope_EffectiveSealed(t *testing.T) {
	sealed := true
	unsealed := false

	tests := []struct {
		name string
		s    Scope
		want bool
	}{
		{name: "absent defaults to sealed", s: Scope{}, want: true},
		{name: "explicit true", s: Scope{Sealed: &sealed}, want: true},
		{name: "explicit false", s: Scope{Sealed: &unsealed}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.EffectiveSealed(); got != tt.want {
				t.Errorf("EffectiveSealed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSupportsScheduleOn covers the one provider-specific exception to
// SupportsSchedule (RFC 017 §2.5).
//
// The exception is not an implementation gap: Cloud Run bills per request
// and idles to zero between them, so there is no running state for a
// schedule to switch off and no saving left for it to deliver.
func TestSupportsScheduleOn(t *testing.T) {
	tests := []struct {
		name     string
		resource ResourceType
		provider Provider
		want     bool
	}{
		{
			name:     "container_service on Cloud Run",
			resource: ResourceTypeContainerService,
			provider: ProviderGCP,
			want:     false,
		},
		{
			name:     "container_service on ECS",
			resource: ResourceTypeContainerService,
			provider: ProviderAWS,
			want:     true,
		},
		{
			name:     "container_service on Container Apps",
			resource: ResourceTypeContainerService,
			provider: ProviderAzure,
			want:     true,
		},
		{
			// Resolution runs before anything consults this, so an
			// unresolved provider should not be answered for: reporting a
			// GCP limitation for a cloud that has not been chosen would be
			// wrong more often than right.
			name:     "container_service still agnostic",
			resource: ResourceTypeContainerService,
			provider: ProviderAgnostic,
			want:     true,
		},
		{
			// The GCP exception is about Cloud Run specifically, not about
			// GCP. Cloud SQL has a real power state.
			name:     "relational_database on GCP",
			resource: ResourceTypeRelationalDatabase,
			provider: ProviderGCP,
			want:     true,
		},
		{
			name:     "compute_instance on GCP",
			resource: ResourceTypeComputeInstance,
			provider: ProviderGCP,
			want:     true,
		},
		{
			// A type that cannot be scheduled anywhere stays that way on
			// every provider: this narrows SupportsSchedule, never widens
			// it.
			name:     "object_storage on AWS",
			resource: ResourceTypeObjectStorage,
			provider: ProviderAWS,
			want:     false,
		},
		{
			name:     "cross_account_role on AWS",
			resource: ResourceTypeCrossAccountRole,
			provider: ProviderAWS,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resource.SupportsScheduleOn(tt.provider); got != tt.want {
				t.Errorf("%s.SupportsScheduleOn(%s) = %v, want %v",
					tt.resource, tt.provider, got, tt.want)
			}
			// It must never report more than SupportsSchedule does.
			if got := tt.resource.SupportsScheduleOn(tt.provider); got && !tt.resource.SupportsSchedule() {
				t.Errorf("%s is schedulable on %s but not as a type", tt.resource, tt.provider)
			}
		})
	}
}
