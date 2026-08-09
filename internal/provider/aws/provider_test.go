// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cloudsdd/internal/spec"
)

func TestNewProvider(t *testing.T) {
	t.Run("fails without passphrase env var", func(t *testing.T) {
		t.Setenv(passphraseEnv, "")
		if _, err := NewProvider(); !errors.Is(err, ErrMissingPassphrase) {
			t.Fatalf("NewProvider() error = %v, want ErrMissingPassphrase", err)
		}
	})

	t.Run("succeeds with passphrase and custom state dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		t.Setenv(passphraseEnv, "test-passphrase")
		t.Setenv(stateDirEnv, dir)

		p, err := NewProvider()
		if err != nil {
			t.Fatalf("NewProvider() unexpected error: %v", err)
		}
		if p.stateDir != dir {
			t.Errorf("stateDir = %q, want %q", p.stateDir, dir)
		}
		if p.passphrase != "test-passphrase" {
			t.Errorf("passphrase not propagated from env")
		}
	})
}

func TestAWSProvider_Name(t *testing.T) {
	p := &AWSProvider{}
	if p.Name() != "aws" {
		t.Errorf("Name() = %q, want %q", p.Name(), "aws")
	}
}

func TestAWSProvider_Validate(t *testing.T) {
	p := &AWSProvider{}

	validS3 := spec.Resource{
		ID:         "app-data",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderAWS,
		Scope:      spec.Scope{Region: "eu-central-1"},
		Properties: map[string]any{"bucket_name": "app-data"},
	}
	validRole := spec.Resource{
		ID:         "partner-access",
		Type:       spec.ResourceTypeCrossAccountRole,
		Provider:   spec.ProviderAWS,
		Scope:      spec.Scope{Sealed: boolPtr(false)},
		Properties: validCrossAccountRoleProps(),
	}

	tests := []struct {
		name     string
		resource spec.Resource
		policies spec.Policies
		wantErr  error // checked with errors.Is when non-nil
		wantAny  bool  // when wantErr is nil, just assert error/no-error
	}{
		{
			name:     "valid object_storage",
			resource: validS3,
			wantAny:  false,
		},
		{
			name:     "object_storage region not allowed",
			resource: validS3,
			policies: spec.Policies{AllowedRegions: []string{"us-east-1"}},
			wantErr:  ErrRegionNotAllowed,
		},
		{
			name:     "cross_account_role without allowed_regions is fail-closed",
			resource: validRole,
			policies: spec.Policies{},
			wantErr:  ErrMissingRegionPolicy,
		},
		{
			name:     "cross_account_role with allowed_regions passes",
			resource: validRole,
			policies: spec.Policies{AllowedRegions: []string{"eu-central-1"}},
			wantAny:  false,
		},
		{
			name: "cross_account_role without sealed=false is rejected (sealed by default)",
			resource: spec.Resource{
				ID: "partner-access", Type: spec.ResourceTypeCrossAccountRole, Provider: spec.ProviderAWS,
				Properties: validCrossAccountRoleProps(),
			},
			policies: spec.Policies{AllowedRegions: []string{"eu-central-1"}},
			wantErr:  ErrSealedCrossAccountRole,
		},
		{
			name: "cross_account_role with a region set is rejected (IAM is global)",
			resource: spec.Resource{
				ID: "partner-access", Type: spec.ResourceTypeCrossAccountRole, Provider: spec.ProviderAWS,
				Scope:      spec.Scope{Sealed: boolPtr(false), Region: "eu-central-1"},
				Properties: validCrossAccountRoleProps(),
			},
			policies: spec.Policies{AllowedRegions: []string{"eu-central-1"}},
			wantErr:  ErrGlobalResourceScoped,
		},
		{
			name: "object_storage without scope.region is rejected",
			resource: spec.Resource{
				ID: "app-data", Type: spec.ResourceTypeObjectStorage, Provider: spec.ProviderAWS,
				Properties: map[string]any{"bucket_name": "app-data"},
			},
			wantErr: ErrRegionRequired,
		},
		{
			name: "object_storage with zones is rejected (not zone-aware)",
			resource: spec.Resource{
				ID: "app-data", Type: spec.ResourceTypeObjectStorage, Provider: spec.ProviderAWS,
				Scope:      spec.Scope{Region: "eu-central-1", Zones: []string{"eu-central-1a"}},
				Properties: map[string]any{"bucket_name": "app-data"},
			},
			wantErr: ErrZonesNotSupported,
		},
		{
			// AWS now implements every ResourceType the schema declares —
			// compute_instance since RFC 013 and container_service since
			// RFC 017 step 3 — so this case uses a type that is not in the
			// enum at all. The default branch is defence for the day the
			// schema grows a type before this provider does, which is
			// exactly what it was for the two above.
			name: "unsupported resource type",
			resource: spec.Resource{
				ID: "api", Type: spec.ResourceType("message_queue"), Provider: spec.ProviderAWS,
				Properties: map[string]any{"image": "nginx"},
			},
			wantErr: ErrUnsupportedResourceType,
		},
		{
			name: "invalid properties propagated",
			resource: spec.Resource{
				ID: "app-data", Type: spec.ResourceTypeObjectStorage, Provider: spec.ProviderAWS,
				Properties: map[string]any{"bucket_name": "ab"},
			},
			wantAny: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Validate(context.Background(), tt.resource, tt.policies)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if (err != nil) != tt.wantAny {
				t.Fatalf("Validate() error = %v, want error presence %v", err, tt.wantAny)
			}
		})
	}
}

// TestAWSProvider_ValidateRegionPath pins the premise of RFC 019 §2.3:
// every regional ResourceType reaches the same three region checks —
// required, well-formed, allowed by policy — so those checks belong once,
// after the switch, rather than repeated inside each case.
//
// The one asymmetry the hoist cannot absorb gets its own subtest:
// cross_account_role is global (IAM has no region, RFC 003 §2.3), so it
// refuses scoping instead of requiring it and must return from inside the
// switch, never reaching the hoisted block at all.
func TestAWSProvider_ValidateRegionPath(t *testing.T) {
	p := &AWSProvider{}

	// Each entry is a valid resource of its type in every respect except
	// the scope, which the region cases below supply.
	regional := []struct {
		name  string
		build func(spec.Scope) spec.Resource
	}{
		{
			name: "relational_database",
			build: func(scope spec.Scope) spec.Resource {
				return spec.Resource{
					ID: "app-db", Type: spec.ResourceTypeRelationalDatabase, Provider: spec.ProviderAWS,
					Scope:      scope,
					Properties: map[string]any{"engine": "postgres", "version": "15"},
				}
			},
		},
		{
			name: "compute_instance",
			build: func(scope spec.Scope) spec.Resource {
				return spec.Resource{
					ID: "build-agent", Type: spec.ResourceTypeComputeInstance, Provider: spec.ProviderAWS,
					Scope:      scope,
					Properties: map[string]any{"size": "small", "os": "ubuntu-22.04"},
				}
			},
		},
		{
			name: "container_service",
			build: func(scope spec.Scope) spec.Resource {
				return containerResource(func(r *spec.Resource) { r.Scope = scope })
			},
		},
		{
			name: "build_pipeline",
			build: func(scope spec.Scope) spec.Resource {
				return spec.Resource{
					ID: "api-build", Type: spec.ResourceTypeBuildPipeline, Provider: spec.ProviderAWS,
					Scope:      scope,
					Properties: pipelineProperties(),
				}
			},
		},
		{
			name: "object_storage",
			build: func(scope spec.Scope) spec.Resource {
				return spec.Resource{
					ID: "app-data", Type: spec.ResourceTypeObjectStorage, Provider: spec.ProviderAWS,
					Scope:      scope,
					Properties: map[string]any{"bucket_name": "app-data"},
				}
			},
		},
	}

	regionCases := []struct {
		name     string
		scope    spec.Scope
		policies spec.Policies
		wantErr  error  // matched with errors.Is when non-nil
		wantMsg  string // matched as a substring when wantErr is nil
	}{
		{
			name:    "no region at all",
			scope:   spec.Scope{},
			wantErr: ErrRegionRequired,
		},
		{
			// The format check has no sentinel of its own, so the message
			// is the only thing there is to assert on.
			name:    "a region in another cloud's format",
			scope:   spec.Scope{Region: "europe-west1"},
			wantMsg: "is not a valid AWS region",
		},
		{
			name:     "a region outside allowed_regions",
			scope:    spec.Scope{Region: "eu-central-1"},
			policies: spec.Policies{AllowedRegions: []string{"us-east-1"}},
			wantErr:  ErrRegionNotAllowed,
		},
		{
			name:     "a region the policy allows",
			scope:    spec.Scope{Region: "eu-central-1"},
			policies: spec.Policies{AllowedRegions: []string{"eu-central-1"}},
		},
	}

	for _, rt := range regional {
		for _, rc := range regionCases {
			t.Run(rt.name+"/"+rc.name, func(t *testing.T) {
				err := p.Validate(context.Background(), rt.build(rc.scope), rc.policies)

				switch {
				case rc.wantErr != nil:
					if !errors.Is(err, rc.wantErr) {
						t.Fatalf("Validate() error = %v, want errors.Is %v", err, rc.wantErr)
					}
				case rc.wantMsg != "":
					if err == nil || !strings.Contains(err.Error(), rc.wantMsg) {
						t.Fatalf("Validate() error = %v, want it to contain %q", err, rc.wantMsg)
					}
				default:
					if err != nil {
						t.Fatalf("Validate() error = %v, want nil", err)
					}
				}
			})
		}
	}

	t.Run("cross_account_role stays off the regional path", func(t *testing.T) {
		role := func(scope spec.Scope) spec.Resource {
			return spec.Resource{
				ID: "partner-access", Type: spec.ResourceTypeCrossAccountRole, Provider: spec.ProviderAWS,
				Scope:      scope,
				Properties: validCrossAccountRoleProps(),
			}
		}
		unsealed := spec.Scope{Sealed: boolPtr(false)}
		allowed := spec.Policies{AllowedRegions: []string{"eu-central-1"}}

		tests := []struct {
			name     string
			resource spec.Resource
			policies spec.Policies
			wantErr  error
		}{
			{
				name:     "a region is refused",
				resource: role(spec.Scope{Sealed: boolPtr(false), Region: "eu-central-1"}),
				policies: allowed,
				wantErr:  ErrGlobalResourceScoped,
			},
			{
				name:     "a region list is refused",
				resource: role(spec.Scope{Sealed: boolPtr(false), Regions: []string{"eu-central-1"}}),
				policies: allowed,
				wantErr:  ErrGlobalResourceScoped,
			},
			{
				name:     "zones are refused",
				resource: role(spec.Scope{Sealed: boolPtr(false), Zones: []string{"eu-central-1a"}}),
				policies: allowed,
				wantErr:  ErrGlobalResourceScoped,
			},
			{
				name:     "a missing region policy is refused",
				resource: role(unsealed),
				policies: spec.Policies{},
				wantErr:  ErrMissingRegionPolicy,
			},
			{
				// The proof that the hoisted block is never reached: a
				// role with no region of any kind validates cleanly.
				name:     "no region at all is accepted",
				resource: role(unsealed),
				policies: allowed,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := p.Validate(context.Background(), tt.resource, tt.policies)
				if tt.wantErr == nil {
					if err != nil {
						t.Fatalf("Validate() error = %v, want nil", err)
					}
					return
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want errors.Is %v", err, tt.wantErr)
				}
			})
		}
	})
}

// TestAWSProvider_resourceProgram exercises the construction of the
// inline Pulumi program (dispatch + decoding) without running it: actually
// running it would require the Pulumi Automation API (and therefore the
// pulumi CLI, not available in sandboxed environments) — covered instead
// by the integration tests behind the "integration" build tag.
func TestAWSProvider_resourceProgram(t *testing.T) {
	p := &AWSProvider{}

	t.Run("object_storage returns its region and a program", func(t *testing.T) {
		program, region, err := p.resourceProgram(spec.Resource{
			ID:         "app-data",
			Type:       spec.ResourceTypeObjectStorage,
			Provider:   spec.ProviderAWS,
			Scope:      spec.Scope{Region: "eu-central-1"},
			Properties: map[string]any{"bucket_name": "app-data"},
		}, spec.Policies{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if region != "eu-central-1" {
			t.Errorf("region = %q, want %q", region, "eu-central-1")
		}
		if program == nil {
			t.Errorf("expected a non-nil program")
		}
	})

	t.Run("cross_account_role has no region of its own (IAM is global)", func(t *testing.T) {
		program, region, err := p.resourceProgram(spec.Resource{
			ID:         "partner-access",
			Type:       spec.ResourceTypeCrossAccountRole,
			Provider:   spec.ProviderAWS,
			Properties: validCrossAccountRoleProps(),
		}, spec.Policies{AllowedRegions: []string{"eu-central-1"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if region != "" {
			t.Errorf("region = %q, want empty", region)
		}
		if program == nil {
			t.Errorf("expected a non-nil program")
		}
	})

	t.Run("unsupported resource type", func(t *testing.T) {
		_, _, err := p.resourceProgram(spec.Resource{
			ID: "api", Type: spec.ResourceType("message_queue"), Provider: spec.ProviderAWS,
			Properties: map[string]any{"image": "nginx"},
		}, spec.Policies{})
		if !errors.Is(err, ErrUnsupportedResourceType) {
			t.Fatalf("error = %v, want errors.Is ErrUnsupportedResourceType", err)
		}
	})

	t.Run("invalid properties propagated", func(t *testing.T) {
		_, _, err := p.resourceProgram(spec.Resource{
			ID: "app-data", Type: spec.ResourceTypeObjectStorage, Provider: spec.ProviderAWS,
			Properties: map[string]any{"bucket_name": "ab"},
		}, spec.Policies{})
		if err == nil {
			t.Fatal("expected error for invalid properties, got nil")
		}
	})
}
