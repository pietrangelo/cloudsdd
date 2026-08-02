// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const testDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// containerSpec is a Specification holding one container_service.
func containerSpec(image string, allowed []string) spec.Specification {
	s := specWithProvider(spec.ProviderAWS)
	s.Resources[0] = spec.Resource{
		ID:       "api",
		Type:     spec.ResourceTypeContainerService,
		Provider: spec.ProviderAWS,
		Scope:    spec.Scope{Region: "eu-central-1"},
		Properties: map[string]any{
			"image": image,
			"port":  8080,
			"size":  "small",
		},
	}
	s.Policies = spec.Policies{AllowedRegistries: allowed}
	return s
}

// TestEngineEnforcesAllowedRegistries is the RFC 017 §5.3 test, in the
// shape RFC 011 §2.3 established for AllowedRegions: the provider accepts
// anything, so only the Engine's own check can fail these.
//
// A check that lives only inside providers is a check the next provider
// forgets, and this is the one property in the schema whose value decides
// what code runs.
func TestEngineEnforcesAllowedRegistries(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		allowed []string
		wantErr error
	}{
		{
			name:  "a digest is accepted with no allowlist",
			image: "ghcr.io/acme/api" + testDigest,
		},
		{
			name:    "a tag is refused with no allowlist",
			image:   "ghcr.io/acme/api:2.1",
			wantErr: container.ErrImageMutable,
		},
		{
			name:    "a tag is accepted from an allow-listed registry",
			image:   "ghcr.io/acme/api:2.1",
			allowed: []string{"ghcr.io"},
		},
		{
			name:    "a registry outside the allowlist is refused",
			image:   "evil.example/acme/api:2.1",
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrRegistryNotAllowed,
		},
		{
			name:    "a digest does not buy passage past the allowlist",
			image:   "evil.example/acme/api" + testDigest,
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrRegistryNotAllowed,
		},
		{
			name:    "latest is refused however the policy is set",
			image:   "ghcr.io/acme/api:latest",
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrImageLatest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(map[spec.Provider]provider.CloudProvider{
				spec.ProviderAWS: &mockProvider{name: "aws"},
			})

			err := e.Validate(context.Background(), containerSpec(tt.image, tt.allowed))

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want it to wrap %v", err, tt.wantErr)
			}
			// The error has to name the resource, or an operator with
			// twenty services cannot tell which image was refused.
			if !strings.Contains(err.Error(), `"api"`) {
				t.Errorf("error %q does not name the resource", err)
			}
		})
	}
}

// TestEngineRefusesAnUnreadableImageProperty: the check fails closed.
//
// A missing or non-string `image` would be rejected a moment later by the
// provider's own decoder, so returning nil here would not ship broken
// infrastructure. It would still be wrong: a policy check that passes when
// it cannot read its input passes hardest exactly when something is off,
// and this one is the backstop for every provider.
func TestEngineRefusesAnUnreadableImageProperty(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]any
	}{
		{name: "absent", props: map[string]any{"port": 8080, "size": "small"}},
		{name: "not a string", props: map[string]any{"image": 42, "port": 8080, "size": "small"}},
		{name: "null", props: map[string]any{"image": nil, "port": 8080, "size": "small"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := containerSpec("ghcr.io/acme/api"+testDigest, nil)
			s.Resources[0].Properties = tt.props

			e := New(map[spec.Provider]provider.CloudProvider{
				spec.ProviderAWS: &mockProvider{name: "aws"},
			})

			if err := e.Validate(context.Background(), s); !errors.Is(err, ErrImagePropertyMissing) {
				t.Fatalf("Validate() = %v, want ErrImagePropertyMissing", err)
			}
		})
	}
}

// TestImagePolicyIgnoresOtherResourceTypes: nothing but a container_service
// has an image, and a database must not be asked for one.
func TestImagePolicyIgnoresOtherResourceTypes(t *testing.T) {
	for _, rt := range []spec.ResourceType{
		spec.ResourceTypeRelationalDatabase,
		spec.ResourceTypeObjectStorage,
		spec.ResourceTypeComputeInstance,
		spec.ResourceTypeCrossAccountRole,
	} {
		t.Run(string(rt), func(t *testing.T) {
			r := spec.Resource{ID: "x", Type: rt, Properties: map[string]any{}}
			policies := spec.Policies{AllowedRegistries: []string{"ghcr.io"}}

			if err := validateImagePolicy(r, policies); err != nil {
				t.Errorf("validateImagePolicy() = %v, want nil for a %s", err, rt)
			}
		})
	}
}

// TestScheduleIsProviderAware covers RFC 017 §2.5 at the Engine.
//
// `container_service` is schedulable as a type and is not on Cloud Run,
// which idles to zero on its own. The Engine has to know that for two
// reasons: an explicit schedule must be refused before anything is
// provisioned, and an inherited one must be *reported* as inapplicable
// rather than listed in the plan as a power-down that will not happen —
// exactly the surprise RFC 012 §1.2 exists to prevent.
func TestScheduleIsProviderAware(t *testing.T) {
	sch := &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}

	scheduled := func(p spec.Provider, own *schedule.Schedule) spec.Specification {
		s := containerSpec("ghcr.io/acme/api"+testDigest, nil)
		s.Resources[0].Provider = p
		s.Resources[0].Schedule = own
		s.Policies.Schedule = sch
		return s
	}

	t.Run("an explicit schedule on Cloud Run is refused", func(t *testing.T) {
		e := New(map[spec.Provider]provider.CloudProvider{
			spec.ProviderGCP: &mockProvider{name: "gcp"},
		})

		err := e.Validate(context.Background(), scheduled(spec.ProviderGCP, sch))
		if !errors.Is(err, ErrResourceNotSchedulable) {
			t.Fatalf("Validate() = %v, want ErrResourceNotSchedulable", err)
		}
	})

	t.Run("an explicit schedule on AWS is accepted", func(t *testing.T) {
		e := New(map[spec.Provider]provider.CloudProvider{
			spec.ProviderAWS: &mockProvider{name: "aws"},
		})

		if err := e.Validate(context.Background(), scheduled(spec.ProviderAWS, sch)); err != nil {
			t.Fatalf("Validate() = %v, want a scheduled ECS service to be accepted", err)
		}
	})

	t.Run("an inherited schedule is reported, not refused", func(t *testing.T) {
		s := scheduled(spec.ProviderGCP, nil)

		e := New(map[spec.Provider]provider.CloudProvider{
			spec.ProviderGCP: &mockProvider{name: "gcp"},
		})
		if err := e.Validate(context.Background(), s); err != nil {
			t.Fatalf("Validate() = %v, want an inherited schedule to be tolerated", err)
		}

		statuses := ScheduleStatuses(s)
		if len(statuses) != 1 {
			t.Fatalf("got %d schedule statuses, want 1", len(statuses))
		}
		if statuses[0].Applied {
			t.Error("the status says the schedule applies; the Cloud Run service will stay up")
		}
		if !strings.Contains(statuses[0].Summary, "stays up") {
			t.Errorf("summary = %q, want it to say the resource stays up", statuses[0].Summary)
		}
	})

	t.Run("the same schedule on AWS is reported as applied", func(t *testing.T) {
		statuses := ScheduleStatuses(scheduled(spec.ProviderAWS, nil))
		if len(statuses) != 1 {
			t.Fatalf("got %d schedule statuses, want 1", len(statuses))
		}
		if !statuses[0].Applied {
			t.Error("the status says the schedule does not apply; ECS can scale to zero")
		}
	})
}
