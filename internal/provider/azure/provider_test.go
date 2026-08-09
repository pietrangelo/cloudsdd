// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

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
		Provider: spec.ProviderAzure,
		Scope:    spec.Scope{Region: "westeurope"},
		Properties: map[string]any{
			"bucket_name": "mystorageacct",
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
		Provider: spec.ProviderAzure,
		Scope:    spec.Scope{Region: "westeurope"},
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

func pipelineResource(mutate func(*spec.Resource)) spec.Resource {
	r := spec.Resource{
		ID:         "api-build",
		Type:       spec.ResourceTypeBuildPipeline,
		Provider:   spec.ProviderAzure,
		Scope:      spec.Scope{Region: "westeurope"},
		Properties: pipelineProperties(),
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
		{name: "valid bucket", resource: bucketResource(nil)},
		{name: "valid database", resource: dbResource(nil)},
		{
			name:     "missing region",
			resource: bucketResource(func(r *spec.Resource) { r.Scope.Region = "" }),
			wantErr:  "region required",
		},
		{
			name:     "malformed location",
			resource: bucketResource(func(r *spec.Resource) { r.Scope.Region = "west-europe" }),
			wantErr:  "not a valid Azure location",
		},
		{
			// RFC 011 §1.1A4: Azure account names are 3-24 lowercase
			// alphanumeric only. The old code truncated silently instead
			// of rejecting, so two distinct resources could collide.
			name:     "bucket name with hyphens rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["bucket_name"] = "my-bucket" }),
			wantErr:  "property validation failed",
		},
		{
			name:     "bucket name with underscore rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["bucket_name"] = "my_bucket" }),
			wantErr:  "property validation failed",
		},
		{
			name:     "bucket name uppercase rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["bucket_name"] = "MyBucket" }),
			wantErr:  "property validation failed",
		},
		{
			name:     "bucket name too long rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["bucket_name"] = strings.Repeat("a", 25) }),
			wantErr:  "property validation failed",
		},
		{
			name:     "unknown property rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["nope"] = 1 }),
			wantErr:  "unknown or malformed property",
		},
		{
			name:     "credential-like property rejected",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["password"] = "hunter2" }),
			wantErr:  "looks like a credential",
		},
		{
			name:     "region outside allowed_regions",
			resource: bucketResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"eastus"}},
			wantErr:  "not in allowed_regions",
		},
		{
			name:     "region inside allowed_regions",
			resource: bucketResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"westeurope"}},
		},
		{
			name:     "unsupported database engine rejected",
			resource: dbResource(func(r *spec.Resource) { r.Properties["engine"] = "oracle" }),
			wantErr:  "property validation failed",
		},
		{
			// RFC 018 §2.8: without the dispatch case the type decodes and
			// declares but is unreachable, and Validate refuses it as
			// unsupported.
			name:     "valid build pipeline",
			resource: pipelineResource(nil),
		},
		{
			name:     "build pipeline missing region",
			resource: pipelineResource(func(r *spec.Resource) { r.Scope.Region = "" }),
			wantErr:  "region required",
		},
		{
			// An ACR Task runs on a pool ACR places itself and the registry
			// is regional, so a pinned zone is a placement the user asked
			// for and will not get.
			name:     "build pipeline with zones",
			resource: pipelineResource(func(r *spec.Resource) { r.Scope.Zones = []string{"1"} }),
			wantErr:  "does not support scope.zones",
		},
		{
			name: "build pipeline with an unsupported runtime version",
			resource: pipelineResource(func(r *spec.Resource) {
				r.Properties["stack"] = map[string]any{"runtime": "go", "version": "0.1"}
			}),
			wantErr: "unsupported runtime version",
		},
		{
			name:     "build pipeline outside allowed_regions",
			resource: pipelineResource(nil),
			policies: spec.Policies{AllowedRegions: []string{"eastus"}},
			wantErr:  "not in allowed_regions",
		},
		{
			// cross_account_role stands in here now. compute_instance held
			// the role until RFC 013 and container_service until RFC 017
			// step 4; this one is AWS's own IAM construct and is not going
			// to acquire an Azure implementation by the same route.
			name:     "unsupported resource type",
			resource: bucketResource(func(r *spec.Resource) { r.Type = spec.ResourceTypeCrossAccountRole }),
			wantErr:  "unsupported resource type",
		},
	}

	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

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
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	err := p.Validate(context.Background(), bucketResource(nil),
		spec.Policies{AllowedRegions: []string{"eastus"}})
	if !errors.Is(err, provider.ErrRegionNotAllowed) {
		t.Fatalf("Validate() error = %v, want it to wrap provider.ErrRegionNotAllowed", err)
	}
}

// TestDestroyDoesNotPanicOnUndecodableProperties is the direct regression
// test for RFC 011 §1.1B1.
//
// resourceProgram discarded the decode error and dereferenced the
// resulting nil pointer inside the Pulumi closure. Plan and Apply happened
// to call Validate first, but Destroy did not — so `cloudsdd destroy`
// panicked on any Azure resource whose properties failed to decode.
func TestDestroyDoesNotPanicOnUndecodableProperties(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	cases := []struct {
		name     string
		resource spec.Resource
	}{
		{
			name:     "missing bucket_name",
			resource: bucketResource(func(r *spec.Resource) { r.Properties = map[string]any{} }),
		},
		{
			name:     "nil properties",
			resource: bucketResource(func(r *spec.Resource) { r.Properties = nil }),
		},
		{
			name:     "invalid bucket_name",
			resource: bucketResource(func(r *spec.Resource) { r.Properties["bucket_name"] = "!!" }),
		},
		{
			name:     "database missing engine",
			resource: dbResource(func(r *spec.Resource) { r.Properties = map[string]any{"version": "15"} }),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Destroy() panicked instead of returning an error: %v", r)
				}
			}()

			err := p.Destroy(context.Background(), tc.resource, spec.Policies{})
			if err == nil {
				t.Fatal("Destroy() error = nil, want a validation error")
			}
		})
	}
}

// TestResourceProgramPropagatesDecodeErrors covers the same defect at the
// layer where it originated.
func TestResourceProgramPropagatesDecodeErrors(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	program, _, err := p.resourceProgram(bucketResource(func(r *spec.Resource) {
		r.Properties = map[string]any{}
	}), spec.Policies{})
	if err == nil {
		t.Fatal("resourceProgram() error = nil, want a decode error")
	}
	if program != nil {
		t.Error("resourceProgram() returned a non-nil program alongside an error")
	}
}

// TestResourceProgramDispatchesBuildPipeline is the other half of the
// wiring: Validate accepting the type is not enough, because Plan, Apply
// and Destroy all reach infrastructure through resourceProgram. Without
// the dispatch case the type validates and then fails as unsupported the
// moment anything is done with it.
func TestResourceProgramDispatchesBuildPipeline(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	r := pipelineResource(func(r *spec.Resource) { r.Resolved = testResolved })

	program, region, err := p.resourceProgram(r, spec.Policies{})
	if err != nil {
		t.Fatalf("resourceProgram() error: %v", err)
	}
	if program == nil {
		t.Fatal("resourceProgram() returned a nil program for build_pipeline")
	}
	if region != r.Scope.Region {
		t.Errorf("resourceProgram() region = %q, want the resource's scope region %q", region, r.Scope.Region)
	}

	// The program is the thing Pulumi runs, so running it is the only way
	// to know the dispatch reaches declareBuildPipeline rather than a stub.
	recorded := runProgram(t, program)
	findResource(t, recorded, acrRegistryToken)
	findNamed(t, recorded, acrTaskToken, r.ID)
}

// TestObjectStoragePreservesBucketName is the regression test for
// RFC 011 §1.1A4: the requested name was decoded, validated as required,
// and then thrown away in favour of a mangled Resource ID.
func TestObjectStoragePreservesBucketName(t *testing.T) {
	props, err := decodeObjectStorageProperties(map[string]any{"bucket_name": "myaccount123"})
	if err != nil {
		t.Fatalf("decodeObjectStorageProperties() error: %v", err)
	}
	if props.BucketName != "myaccount123" {
		t.Errorf("BucketName = %q, want the requested name %q", props.BucketName, "myaccount123")
	}
}

func TestObjectStorageSecureDefaults(t *testing.T) {
	props, err := decodeObjectStorageProperties(map[string]any{"bucket_name": "myaccount123"})
	if err != nil {
		t.Fatalf("decodeObjectStorageProperties() error: %v", err)
	}
	if !props.EffectiveBlockPublicAccess() {
		t.Error("EffectiveBlockPublicAccess() = false, want true by default (RFC 011 §2.5)")
	}
	if props.EffectiveVersioning() {
		t.Error("EffectiveVersioning() = true, want false by default")
	}
}

func TestObjectStorageRejectsDisabledEncryption(t *testing.T) {
	_, err := decodeObjectStorageProperties(map[string]any{
		"bucket_name": "myaccount123",
		"encryption":  false,
	})
	if err == nil {
		t.Fatal("decodeObjectStorageProperties() accepted encryption:false")
	}
	if !strings.Contains(err.Error(), "cannot be disabled") {
		t.Errorf("error = %q, want an explanation that encryption cannot be disabled", err)
	}
}

// TestRelationalDatabaseHonoursEngineAndHA is the regression test for
// RFC 011 §1.1A3: the provider always built a PostgreSQL server, and
// decoded high_availability without ever reading it.
func TestRelationalDatabaseHonoursEngineAndHA(t *testing.T) {
	tests := []struct {
		name       string
		props      map[string]any
		wantEngine string
		wantHA     bool
		wantErr    bool
	}{
		{
			name:       "postgres without HA",
			props:      map[string]any{"engine": "postgres", "version": "15"},
			wantEngine: "postgres",
		},
		{
			name:       "mysql is preserved, not substituted",
			props:      map[string]any{"engine": "mysql", "version": "8.0"},
			wantEngine: "mysql",
		},
		{
			name:       "high_availability is carried through",
			props:      map[string]any{"engine": "mysql", "version": "8.0", "high_availability": true},
			wantEngine: "mysql",
			wantHA:     true,
		},
		{
			name:    "unsupported engine rejected",
			props:   map[string]any{"engine": "oracle", "version": "19"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeRelationalDatabaseProperties(tt.props)

			if tt.wantErr {
				if err == nil {
					t.Fatal("decodeRelationalDatabaseProperties() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRelationalDatabaseProperties() error: %v", err)
			}
			if p.Engine != tt.wantEngine {
				t.Errorf("Engine = %q, want %q", p.Engine, tt.wantEngine)
			}
			if p.HighAvailability != tt.wantHA {
				t.Errorf("HighAvailability = %v, want %v", p.HighAvailability, tt.wantHA)
			}
		})
	}
}

// TestRelationalDatabaseDeletionProtection covers the decoding half of
// RFC 015 §2.1. The row that matters most is "explicitly false": before
// this RFC the property was not declared on Azure, so strict decoding
// rejected it as unknown — the documented way to relax the default was
// an error on the one provider that did not implement the default.
func TestRelationalDatabaseDeletionProtection(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		want    bool
		wantErr bool
	}{
		{
			name:  "absent defaults to protected",
			props: map[string]any{"engine": "postgres", "version": "15"},
			want:  true,
		},
		{
			name:  "explicitly true",
			props: map[string]any{"engine": "postgres", "version": "15", "deletion_protection": true},
			want:  true,
		},
		{
			name:  "explicitly false is accepted, not rejected as unknown",
			props: map[string]any{"engine": "postgres", "version": "15", "deletion_protection": false},
			want:  false,
		},
		{
			name:    "non-boolean rejected",
			props:   map[string]any{"engine": "postgres", "version": "15", "deletion_protection": "yes"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeRelationalDatabaseProperties(tt.props)

			if tt.wantErr {
				if err == nil {
					t.Fatal("decodeRelationalDatabaseProperties() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRelationalDatabaseProperties() error: %v", err)
			}
			if got := p.EffectiveDeletionProtection(); got != tt.want {
				t.Errorf("EffectiveDeletionProtection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStackNameFor(t *testing.T) {
	tests := []struct {
		name                              string
		account, environment, region, res string
		want                              string
	}{
		{name: "fully scoped", account: "prod", environment: "live", region: "westeurope", res: "db", want: "prod::live::westeurope::db"},
		{name: "unscoped", res: "db", want: "db"},
		{name: "environment isolates", environment: "dev", region: "westeurope", res: "db", want: "dev::westeurope::db"},
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

// TestNewProviderReportsUnusableStateDir covers RFC 011 §1.1F3: the
// MkdirAll error was discarded here, unlike in the GCP twin.
func TestNewProviderReportsUnusableStateDir(t *testing.T) {
	// A regular file cannot become a directory.
	blocker := t.TempDir() + "/not-a-dir"
	if err := writeFile(blocker); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Setenv(passphraseEnv, "test-passphrase")
	t.Setenv(stateDirEnv, blocker+"/state")

	if _, err := NewProvider(); err == nil {
		t.Fatal("NewProvider() error = nil, want the MkdirAll failure to be reported")
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
	if p.Name() != string(spec.ProviderAzure) {
		t.Errorf("Name() = %q, want %q", p.Name(), spec.ProviderAzure)
	}
}
