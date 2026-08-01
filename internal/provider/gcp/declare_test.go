// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// recordedResource is one resource a Pulumi program declared.
type recordedResource struct {
	Type   string
	Name   string
	Inputs resource.PropertyMap
}

// recorder collects declared resources. Pulumi registers resources
// concurrently, so access to the slice must be synchronized.
type recorder struct {
	mu        sync.Mutex
	resources []recordedResource
}

func (r *recorder) add(res recordedResource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = append(r.resources, res)
}

func (r *recorder) snapshot() []recordedResource {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedResource(nil), r.resources...)
}

// mockMonitor captures declared resources without contacting GCP, so the
// tests can assert the inputs the provider actually produces rather than
// only what it passes around internally.
type mockMonitor struct {
	rec *recorder
}

func (m mockMonitor) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.rec.add(recordedResource{
		Type:   args.TypeToken,
		Name:   args.Name,
		Inputs: args.Inputs,
	})

	outputs := args.Inputs.Copy()
	// RandomPassword's consumers read .Result.
	if args.TypeToken == "random:index/randomPassword:RandomPassword" {
		outputs["result"] = resource.NewStringProperty("generated-password")
	}
	return args.Name + "-id", outputs, nil
}

func (m mockMonitor) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

// runProgram executes fn under the Pulumi mock monitor and returns every
// resource it declared.
func runProgram(t *testing.T, fn func(ctx *pulumi.Context) error) []recordedResource {
	t.Helper()

	rec := &recorder{}
	if err := pulumi.RunErr(fn, pulumi.WithMocks("cloudsdd-gcp", "test", mockMonitor{rec: rec})); err != nil {
		t.Fatalf("pulumi program failed: %v", err)
	}
	return rec.snapshot()
}

func findResource(t *testing.T, recorded []recordedResource, typeToken string) recordedResource {
	t.Helper()
	for _, r := range recorded {
		if r.Type == typeToken {
			return r
		}
	}
	t.Fatalf("no %q resource was declared; got %v", typeToken, typeTokens(recorded))
	return recordedResource{}
}

func typeTokens(recorded []recordedResource) []string {
	out := make([]string, 0, len(recorded))
	for _, r := range recorded {
		out = append(out, r.Type)
	}
	return out
}

// TestDeclareObjectStorageUsesRequestedRegion is the end-to-end
// regression test for RFC 011 §1.1A1. The bucket Location was hardcoded
// to "EU" while Validate required a region, so a bucket requested in
// us-east1 was created in Europe — a data-residency violation the user
// had no way to see.
func TestDeclareObjectStorageUsesRequestedRegion(t *testing.T) {
	for _, region := range []string{"us-east1", "europe-west4", "asia-northeast1"} {
		t.Run(region, func(t *testing.T) {
			props := objectStorageProperties{BucketName: "my-bucket"}

			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				return declareObjectStorage(ctx, "my-bucket", region, props)
			})

			bucket := findResource(t, recorded, "gcp:storage/bucket:Bucket")

			got := bucket.Inputs["location"].StringValue()
			if got != region {
				t.Errorf("bucket location = %q, want the requested region %q", got, region)
			}
			if got == "EU" && region != "EU" {
				t.Error("bucket location fell back to the old hardcoded \"EU\"")
			}
		})
	}
}

func TestDeclareObjectStorageSecureDefaults(t *testing.T) {
	props := objectStorageProperties{BucketName: "my-bucket"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareObjectStorage(ctx, "my-bucket", "us-east1", props)
	})
	bucket := findResource(t, recorded, "gcp:storage/bucket:Bucket")

	if got := bucket.Inputs["publicAccessPrevention"].StringValue(); got != "enforced" {
		t.Errorf("publicAccessPrevention = %q, want %q by default", got, "enforced")
	}
	if !bucket.Inputs["uniformBucketLevelAccess"].BoolValue() {
		t.Error("uniformBucketLevelAccess = false, want true")
	}
	// RFC 011 §1.1E: this shipped hardcoded true "for simplified
	// teardown", turning an accidental destroy into data loss.
	if bucket.Inputs["forceDestroy"].BoolValue() {
		t.Error("forceDestroy = true, want false by default (RFC 011 §2.5)")
	}
	if got := bucket.Inputs["name"].StringValue(); got != "my-bucket" {
		t.Errorf("bucket name = %q, want the requested name", got)
	}
}

func TestDeclareObjectStorageHonoursExplicitOverrides(t *testing.T) {
	yes, no := true, false
	props := objectStorageProperties{
		BucketName:        "my-bucket",
		Versioning:        &yes,
		BlockPublicAccess: &no,
		ForceDestroy:      &yes,
	}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareObjectStorage(ctx, "my-bucket", "us-east1", props)
	})
	bucket := findResource(t, recorded, "gcp:storage/bucket:Bucket")

	if !bucket.Inputs["forceDestroy"].BoolValue() {
		t.Error("forceDestroy = false, want the explicitly requested true")
	}
	if got := bucket.Inputs["publicAccessPrevention"].StringValue(); got != "inherited" {
		t.Errorf("publicAccessPrevention = %q, want %q when public access is allowed", got, "inherited")
	}
	versioning := bucket.Inputs["versioning"].ObjectValue()
	if !versioning["enabled"].BoolValue() {
		t.Error("versioning.enabled = false, want the explicitly requested true")
	}
}

// TestDeclareRelationalDatabaseHonoursEngine is the end-to-end regression
// test for RFC 011 §1.1A2: the version string was built as
// "POSTGRES_%s" regardless of engine, so asking for MySQL provisioned
// PostgreSQL.
func TestDeclareRelationalDatabaseHonoursEngine(t *testing.T) {
	tests := []struct {
		name        string
		props       relationalDatabaseProperties
		wantVersion string
		wantAvail   string
	}{
		{
			name:        "postgres",
			props:       relationalDatabaseProperties{Engine: "postgres", Version: "15"},
			wantVersion: "POSTGRES_15",
			wantAvail:   "ZONAL",
		},
		{
			name:        "mysql is not substituted",
			props:       relationalDatabaseProperties{Engine: "mysql", Version: "8.0"},
			wantVersion: "MYSQL_8_0",
			wantAvail:   "ZONAL",
		},
		{
			name:        "high availability is regional",
			props:       relationalDatabaseProperties{Engine: "postgres", Version: "15", HighAvailability: true},
			wantVersion: "POSTGRES_15",
			wantAvail:   "REGIONAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				return declareRelationalDatabase(ctx, "app-db", "us-east1", tt.props)
			})

			instance := findResource(t, recorded, "gcp:sql/databaseInstance:DatabaseInstance")

			if got := instance.Inputs["databaseVersion"].StringValue(); got != tt.wantVersion {
				t.Errorf("databaseVersion = %q, want %q", got, tt.wantVersion)
			}
			if got := instance.Inputs["region"].StringValue(); got != "us-east1" {
				t.Errorf("region = %q, want the requested region", got)
			}

			settings := instance.Inputs["settings"].ObjectValue()
			if got := settings["availabilityType"].StringValue(); got != tt.wantAvail {
				t.Errorf("availabilityType = %q, want %q", got, tt.wantAvail)
			}
		})
	}
}

func TestDeclareRelationalDatabaseSecureDefaults(t *testing.T) {
	props := relationalDatabaseProperties{Engine: "postgres", Version: "15"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareRelationalDatabase(ctx, "app-db", "us-east1", props)
	})
	instance := findResource(t, recorded, "gcp:sql/databaseInstance:DatabaseInstance")

	// RFC 011 §1.1E: shipped hardcoded false "for simplified teardown".
	if !instance.Inputs["deletionProtection"].BoolValue() {
		t.Error("deletionProtection = false, want true by default (RFC 011 §2.5)")
	}

	settings := instance.Inputs["settings"].ObjectValue()
	ipConfig := settings["ipConfiguration"].ObjectValue()
	if ipConfig["ipv4Enabled"].BoolValue() {
		t.Error("ipv4Enabled = true, want false (private IP only)")
	}
	backup := settings["backupConfiguration"].ObjectValue()
	if !backup["enabled"].BoolValue() {
		t.Error("backupConfiguration.enabled = false, want true")
	}

	// The master user must exist and must not carry a literal password.
	user := findResource(t, recorded, "gcp:sql/user:User")
	if got := user.Inputs["name"].StringValue(); got != "masteruser" {
		t.Errorf("user name = %q, want %q", got, "masteruser")
	}
}

func TestDeclareRelationalDatabaseAllowsExplicitDisposability(t *testing.T) {
	no := false
	props := relationalDatabaseProperties{Engine: "postgres", Version: "15", DeletionProtection: &no}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareRelationalDatabase(ctx, "app-db", "us-east1", props)
	})
	instance := findResource(t, recorded, "gcp:sql/databaseInstance:DatabaseInstance")

	if instance.Inputs["deletionProtection"].BoolValue() {
		t.Error("deletionProtection = true, want the explicitly requested false")
	}
}

func TestEffectiveEncryptionDefaultsOn(t *testing.T) {
	var p objectStorageProperties
	if !p.EffectiveEncryption() {
		t.Error("EffectiveEncryption() = false, want true when absent")
	}
}
