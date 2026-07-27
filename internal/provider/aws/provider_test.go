// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"errors"
	"path/filepath"
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
		Properties: map[string]any{"bucket_name": "app-data", "region": "eu-central-1"},
	}
	validRole := spec.Resource{
		ID:         "partner-access",
		Type:       spec.ResourceTypeCrossAccountRole,
		Provider:   spec.ProviderAWS,
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
			name: "unsupported resource type",
			resource: spec.Resource{
				ID: "db", Type: spec.ResourceTypeRelationalDatabase, Provider: spec.ProviderAWS,
				Properties: map[string]any{"engine": "postgres"},
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
			Properties: map[string]any{"bucket_name": "app-data", "region": "eu-central-1"},
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
			ID: "db", Type: spec.ResourceTypeRelationalDatabase, Provider: spec.ProviderAWS,
			Properties: map[string]any{"engine": "postgres"},
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
