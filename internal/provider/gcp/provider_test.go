// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

func bucketResource(mutate func(*spec.Resource)) spec.Resource {
	r := spec.Resource{
		ID:       "my-bucket",
		Type:     spec.ResourceTypeObjectStorage,
		Provider: spec.ProviderGCP,
		Scope:    spec.Scope{Region: "europe-west1"},
		Properties: map[string]any{
			"bucket_name": "my-bucket",
		},
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func dbResource(mutate func(*spec.Resource)) spec.Resource {
	r := spec.Resource{
		ID:       "app-db",
		Type:     spec.ResourceTypeRelationalDatabase,
		Provider: spec.ProviderGCP,
		Scope:    spec.Scope{Region: "europe-west1"},
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

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		resource spec.Resource
		policies spec.Policies
		wantErr  string
	}{
		{
			name:     "valid bucket",
			resource: bucketResource(nil),
		},
		{
			name:     "valid database",
			resource: dbResource(nil),
		},
		{
			name:     "bucket missing region",
			resource: bucketResource(func(r *spec.Resource) { r.Scope.Region = "" }),
			wantErr:  "region required",
		},
		{
			name:     "malformed region",
			resource: bucketResource(func(r *spec.Resource) { r.Scope.Region = "eu-central-1x" }),
			wantErr:  "not a valid GCP region",
		},
		{
			name:     "missing bucket_name",
			resource: bucketResource(func(r *spec.Resource) { r.Properties = map[string]any{} }),
			wantErr:  "property validation failed",
		},
		{
			// RFC 011 §2.1: unknown properties must be rejected, not
			// silently dropped.
			name: "unknown property rejected",
			resource: bucketResource(func(r *spec.Resource) {
				r.Properties["not_a_real_property"] = true
			}),
			wantErr: "unknown or malformed property",
		},
		{
			name: "credential-like property rejected",
			resource: bucketResource(func(r *spec.Resource) {
				r.Properties["secret_key"] = "AKIA..."
			}),
			wantErr: "looks like a credential",
		},
		{
			// RFC 011 §1.1D1: AllowedRegions was previously unenforced
			// on GCP entirely.
			name:     "region outside allowed_regions",
			resource: bucketResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"us-east1"}},
			wantErr:  "not in allowed_regions",
		},
		{
			name:     "region inside allowed_regions",
			resource: bucketResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"europe-west1", "us-east1"}},
		},
		{
			// RFC 011 §1.1A2: an unsupported engine used to be silently
			// substituted with PostgreSQL.
			name:     "unsupported database engine rejected",
			resource: dbResource(func(r *spec.Resource) { r.Properties["engine"] = "oracle" }),
			wantErr:  "property validation failed",
		},
		{
			// container_service remains advertised by the schema and
			// implemented by nobody (RFC 013 §7.3); compute_instance used
			// to stand in here and is now supported.
			name:     "unsupported resource type",
			resource: bucketResource(func(r *spec.Resource) { r.Type = spec.ResourceTypeContainerService }),
			wantErr:  "unsupported resource type",
		},
	}

	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

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

func TestValidateRegionAllowedIsSentinel(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	err := p.Validate(context.Background(), bucketResource(nil),
		spec.Policies{AllowedRegions: []string{"us-east1"}})
	if !errors.Is(err, provider.ErrRegionNotAllowed) {
		t.Fatalf("Validate() error = %v, want it to wrap provider.ErrRegionNotAllowed", err)
	}
}

// TestObjectStorageSecureDefaults pins the tri-state defaults from
// RFC 011 §2.5: absent means secure, not the bool zero value.
func TestObjectStorageSecureDefaults(t *testing.T) {
	tests := []struct {
		name                  string
		props                 map[string]any
		wantVersioning        bool
		wantBlockPublicAccess bool
		wantForceDestroy      bool
	}{
		{
			name:                  "all absent",
			props:                 map[string]any{"bucket_name": "my-bucket"},
			wantVersioning:        false,
			wantBlockPublicAccess: true,  // secure default
			wantForceDestroy:      false, // secure default
		},
		{
			name: "explicit overrides",
			props: map[string]any{
				"bucket_name":         "my-bucket",
				"versioning":          true,
				"block_public_access": false,
				"force_destroy":       true,
			},
			wantVersioning:        true,
			wantBlockPublicAccess: false,
			wantForceDestroy:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeObjectStorageProperties(tt.props)
			if err != nil {
				t.Fatalf("decodeObjectStorageProperties() error: %v", err)
			}
			if got := p.EffectiveVersioning(); got != tt.wantVersioning {
				t.Errorf("EffectiveVersioning() = %v, want %v", got, tt.wantVersioning)
			}
			if got := p.EffectiveBlockPublicAccess(); got != tt.wantBlockPublicAccess {
				t.Errorf("EffectiveBlockPublicAccess() = %v, want %v", got, tt.wantBlockPublicAccess)
			}
			if got := p.EffectiveForceDestroy(); got != tt.wantForceDestroy {
				t.Errorf("EffectiveForceDestroy() = %v, want %v", got, tt.wantForceDestroy)
			}
		})
	}
}

func TestObjectStorageRejectsDisabledEncryption(t *testing.T) {
	_, err := decodeObjectStorageProperties(map[string]any{
		"bucket_name": "my-bucket",
		"encryption":  false,
	})
	if err == nil {
		t.Fatal("decodeObjectStorageProperties() accepted encryption:false")
	}
	if !strings.Contains(err.Error(), "cannot be disabled") {
		t.Errorf("error = %q, want an explanation that encryption cannot be disabled", err)
	}
}

func TestRelationalDatabaseDeletionProtectionDefaultsOn(t *testing.T) {
	p, err := decodeRelationalDatabaseProperties(map[string]any{
		"engine":  "postgres",
		"version": "15",
	})
	if err != nil {
		t.Fatalf("decodeRelationalDatabaseProperties() error: %v", err)
	}
	if !p.EffectiveDeletionProtection() {
		t.Error("EffectiveDeletionProtection() = false, want true when the property is absent (RFC 011 §2.5)")
	}
}

// TestCloudSQLDatabaseVersion is the direct regression test for RFC 011
// §1.1A2: the mapping used to be fmt.Sprintf("POSTGRES_%s", version)
// regardless of engine, so requesting MySQL got you PostgreSQL.
func TestCloudSQLDatabaseVersion(t *testing.T) {
	tests := []struct {
		name    string
		engine  string
		version string
		want    string
		wantErr bool
	}{
		{name: "postgres", engine: "postgres", version: "15", want: "POSTGRES_15"},
		{name: "postgres case-insensitive", engine: "Postgres", version: "15", want: "POSTGRES_15"},
		{name: "mysql", engine: "mysql", version: "8.0", want: "MYSQL_8_0"},
		{name: "mysql major only", engine: "mysql", version: "8", want: "MYSQL_8"},
		{name: "unsupported engine", engine: "oracle", version: "19", wantErr: true},
		{name: "empty engine", engine: "", version: "15", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cloudSQLDatabaseVersion(tt.engine, tt.version)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("cloudSQLDatabaseVersion(%q, %q) = %q, want error", tt.engine, tt.version, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("cloudSQLDatabaseVersion() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("cloudSQLDatabaseVersion(%q, %q) = %q, want %q", tt.engine, tt.version, got, tt.want)
			}
		})
	}
}

// TestResourceProgramUsesScopeRegion is the regression test for RFC 011
// §1.1A1: the bucket location was hardcoded to "EU" while Validate
// required a region, so a bucket asked for in us-east1 was created in
// Europe.
func TestResourceProgramUsesScopeRegion(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	for _, region := range []string{"us-east1", "europe-west4", "asia-northeast1"} {
		t.Run(region, func(t *testing.T) {
			r := bucketResource(func(r *spec.Resource) { r.Scope.Region = region })

			_, gotRegion, err := p.resourceProgram(r, spec.Policies{})
			if err != nil {
				t.Fatalf("resourceProgram() error: %v", err)
			}
			if gotRegion != region {
				t.Errorf("resourceProgram() region = %q, want the resource's scope region %q", gotRegion, region)
			}
		})
	}
}

// TestResourceProgramPropagatesDecodeErrors guards the shape that made
// the Azure twin panic (RFC 011 §1.1B1).
func TestResourceProgramPropagatesDecodeErrors(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	r := bucketResource(func(r *spec.Resource) { r.Properties = map[string]any{} })

	program, _, err := p.resourceProgram(r, spec.Policies{})
	if err == nil {
		t.Fatal("resourceProgram() error = nil, want a decode error")
	}
	if program != nil {
		t.Error("resourceProgram() returned a non-nil program alongside an error")
	}
}

func TestStackNameFor(t *testing.T) {
	tests := []struct {
		name                              string
		account, environment, region, res string
		want                              string
	}{
		{name: "fully scoped", account: "prod", environment: "live", region: "us-east1", res: "db", want: "prod::live::us-east1::db"},
		{name: "unscoped", res: "db", want: "db"},
		{name: "region only", region: "us-east1", res: "db", want: "us-east1::db"},
		{
			// The isolation property RFC 005 §2.6 exists for.
			name: "same id different environment", environment: "dev", region: "us-east1", res: "db",
			want: "dev::us-east1::db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stackNameFor(tt.account, tt.environment, tt.region, tt.res); got != tt.want {
				t.Errorf("stackNameFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewProviderRequiresPassphrase(t *testing.T) {
	t.Setenv(passphraseEnv, "")
	t.Setenv(stateDirEnv, t.TempDir())

	if _, err := NewProvider(); err == nil {
		t.Fatal("NewProvider() error = nil, want an error when the passphrase is unset")
	}
}

func TestNewProviderUsesStateDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(passphraseEnv, "test-passphrase")
	t.Setenv(stateDirEnv, dir)

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider() error: %v", err)
	}
	if p.stateDir != dir {
		t.Errorf("stateDir = %q, want %q", p.stateDir, dir)
	}
	if p.Name() != string(spec.ProviderGCP) {
		t.Errorf("Name() = %q, want %q", p.Name(), spec.ProviderGCP)
	}
}
