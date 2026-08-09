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

// TestBuildPipelineNotSchedulable: a pipeline has no power state to switch
// off. It runs when a deployment runs it and costs nothing in between, so
// there is no saving a schedule could deliver (RFC 012 §3, RFC 018 §2.1).
func TestBuildPipelineNotSchedulable(t *testing.T) {
	if ResourceTypeBuildPipeline.SupportsSchedule() {
		t.Error("build_pipeline reports a power state it does not have")
	}
	for _, p := range []Provider{ProviderAWS, ProviderGCP, ProviderAzure, ProviderAgnostic} {
		if ResourceTypeBuildPipeline.SupportsScheduleOn(p) {
			t.Errorf("build_pipeline reports as schedulable on %s", p)
		}
	}
}

// TestResolved_CommitOr covers the fallback every provider needs when it is
// driven directly rather than through the Engine (RFC 019 §2.2).
//
// The nil receiver is the case that matters. A provider used on its own
// never sees a Resolved at all, and refusing there would make the provider
// unusable outside the Engine — so absence selects the revision as written
// rather than an error.
func TestResolved_CommitOr(t *testing.T) {
	tests := []struct {
		name     string
		resolved *Resolved
		fallback string
		want     string
	}{
		{
			name:     "no reference falls back to the revision as written",
			resolved: nil,
			fallback: "main",
			want:     "main",
		},
		{
			name:     "a resolved commit wins over the revision",
			resolved: &Resolved{ImageName: "acme/api", Commit: "0f3a1c"},
			fallback: "main",
			want:     "0f3a1c",
		},
		{
			name:     "a reference carrying no commit falls back",
			resolved: &Resolved{ImageName: "acme/api"},
			fallback: "main",
			want:     "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resolved.CommitOr(tt.fallback); got != tt.want {
				t.Errorf("CommitOr(%q) = %q, want %q", tt.fallback, got, tt.want)
			}
		})
	}
}

// TestResolved_Complete covers the guard each provider's pipelineImage opens
// with (RFC 019 §2.2).
//
// Both halves of the RFC 018 §2.4.1 hand-off are required, and a half-filled
// reference is refused rather than completed by guessing: the two plausible
// inventions — the pipeline's resource id, or `latest` — are respectively
// wrong and the one thing RFC 017 §2.4 refuses outright.
func TestResolved_Complete(t *testing.T) {
	tests := []struct {
		name     string
		resolved *Resolved
		want     bool
	}{
		{name: "no reference at all", resolved: nil, want: false},
		{name: "a commit with no image name", resolved: &Resolved{Commit: "0f3a1c"}, want: false},
		{name: "an image name with no commit", resolved: &Resolved{ImageName: "acme/api"}, want: false},
		{name: "both halves present", resolved: &Resolved{ImageName: "acme/api", Commit: "0f3a1c"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.resolved.Complete(); got != tt.want {
				t.Errorf("Complete() = %v, want %v", got, tt.want)
			}
		})
	}
}
