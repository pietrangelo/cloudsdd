// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"strings"
	"testing"

	"cloudsdd/internal/spec"
)

func TestDecodeRelationalDatabaseProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
		check   func(t *testing.T, p relationalDatabaseProperties)
	}{
		{
			name:  "minimal postgres",
			props: map[string]any{"engine": "postgres", "version": "15"},
			check: func(t *testing.T, p relationalDatabaseProperties) {
				if p.Engine != "postgres" || p.Version != "15" {
					t.Errorf("decoded = %+v, want engine=postgres version=15", p)
				}
				if p.HighAvailability {
					t.Error("HighAvailability = true, want false by default")
				}
			},
		},
		{
			name:  "mysql",
			props: map[string]any{"engine": "mysql", "version": "8.0"},
			check: func(t *testing.T, p relationalDatabaseProperties) {
				if p.Engine != "mysql" {
					t.Errorf("Engine = %q, want mysql", p.Engine)
				}
			},
		},
		{
			name:  "mariadb",
			props: map[string]any{"engine": "mariadb", "version": "11"},
		},
		{
			name:  "high availability",
			props: map[string]any{"engine": "postgres", "version": "15", "high_availability": true},
			check: func(t *testing.T, p relationalDatabaseProperties) {
				if !p.HighAvailability {
					t.Error("HighAvailability = false, want true")
				}
			},
		},
		{
			name:    "unsupported engine",
			props:   map[string]any{"engine": "oracle", "version": "19"},
			wantErr: "property validation failed",
		},
		{
			name:    "missing engine",
			props:   map[string]any{"version": "15"},
			wantErr: "property validation failed",
		},
		{
			name:    "missing version",
			props:   map[string]any{"engine": "postgres"},
			wantErr: "property validation failed",
		},
		{
			// RFC 011 §1.1C: this decoder used to hand-roll a permissive
			// json.Unmarshal, so unknown properties were silently dropped
			// and credential-like keys sailed straight through.
			name:    "unknown property rejected",
			props:   map[string]any{"engine": "postgres", "version": "15", "instance_class": "db.r5.large"},
			wantErr: "unknown or malformed property",
		},
		{
			name:    "credential-like property rejected",
			props:   map[string]any{"engine": "postgres", "version": "15", "password": "hunter2"},
			wantErr: "looks like a credential",
		},
		{
			name:    "wrong type",
			props:   map[string]any{"engine": 15, "version": "15"},
			wantErr: "unknown or malformed property",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeRelationalDatabaseProperties(tt.props)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("decodeRelationalDatabaseProperties() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("decodeRelationalDatabaseProperties() unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, *p)
			}
		})
	}
}

// TestRelationalDatabaseSecureDefaults pins the RFC 011 §2.5 reversal:
// the provider previously shipped SkipFinalSnapshot hardcoded true "for
// simplified teardown during dev", so an accidental destroy discarded the
// database with no recovery point.
func TestRelationalDatabaseSecureDefaults(t *testing.T) {
	tests := []struct {
		name                   string
		props                  map[string]any
		wantDeletionProtection bool
		wantSkipFinalSnapshot  bool
	}{
		{
			name:                   "absent means protected",
			props:                  map[string]any{"engine": "postgres", "version": "15"},
			wantDeletionProtection: true,
			wantSkipFinalSnapshot:  false,
		},
		{
			name: "explicitly disposable",
			props: map[string]any{
				"engine": "postgres", "version": "15",
				"deletion_protection": false,
				"skip_final_snapshot": true,
			},
			wantDeletionProtection: false,
			wantSkipFinalSnapshot:  true,
		},
		{
			name: "explicitly protected",
			props: map[string]any{
				"engine": "postgres", "version": "15",
				"deletion_protection": true,
				"skip_final_snapshot": false,
			},
			wantDeletionProtection: true,
			wantSkipFinalSnapshot:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeRelationalDatabaseProperties(tt.props)
			if err != nil {
				t.Fatalf("decodeRelationalDatabaseProperties() error: %v", err)
			}

			if got := p.EffectiveDeletionProtection(); got != tt.wantDeletionProtection {
				t.Errorf("EffectiveDeletionProtection() = %v, want %v", got, tt.wantDeletionProtection)
			}
			if got := p.EffectiveSkipFinalSnapshot(); got != tt.wantSkipFinalSnapshot {
				t.Errorf("EffectiveSkipFinalSnapshot() = %v, want %v", got, tt.wantSkipFinalSnapshot)
			}
		})
	}
}

func TestValidateRelationalDatabase(t *testing.T) {
	dbResource := func(mutate func(*spec.Resource)) spec.Resource {
		r := spec.Resource{
			ID:       "app-db",
			Type:     spec.ResourceTypeRelationalDatabase,
			Provider: spec.ProviderAWS,
			Scope:    spec.Scope{Region: "eu-central-1"},
			Properties: map[string]any{
				"engine":  "postgres",
				"version": "15",
			},
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	tests := []struct {
		name     string
		resource spec.Resource
		policies spec.Policies
		wantErr  string
	}{
		{name: "valid", resource: dbResource(nil)},
		{
			name:     "missing region",
			resource: dbResource(func(r *spec.Resource) { r.Scope.Region = "" }),
			wantErr:  "scope.region",
		},
		{
			name:     "malformed region",
			resource: dbResource(func(r *spec.Resource) { r.Scope.Region = "europe-west1" }),
			wantErr:  "not a valid AWS region",
		},
		{
			name:     "region outside policy",
			resource: dbResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"us-east-1"}},
			wantErr:  "not in allowed_regions",
		},
		{
			name:     "invalid properties",
			resource: dbResource(func(r *spec.Resource) { r.Properties = map[string]any{} }),
			wantErr:  "property validation failed",
		},
	}

	p := &AWSProvider{stateDir: t.TempDir(), passphrase: "test"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Validate(context.Background(), tt.resource, tt.policies)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
