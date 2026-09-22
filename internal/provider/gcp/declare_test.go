// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"crypto/sha256"
	"fmt"
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

	// filestore is what the scope's network stack left behind, as the
	// lookup of RFC 020 §2.8 sees it. The zero value is the ordinary
	// scope, which is what every test that is not about a broken one
	// wants.
	filestore filestoreScope
}

// filestoreScope lets a test describe a scope whose Filestore instances
// are not what the mounting stack expects.
//
// Both cases are broken invariants rather than races: the Engine
// provisions a scope's network stack before any resource in it, so an
// instance absent here was removed out of band. The provider must refuse,
// because a Cloud Run revision pointed at an NFS server that is not there
// fails at start, long after the user approved a plan that looked fine.
type filestoreScope struct {
	// missingInstance is an instance name nothing answers to. The real
	// getInstance invoke fails when nothing matches, and so does this.
	missingInstance string

	// withoutAddress strips every instance of its private address, which
	// is what an instance still being created, or one detached from the
	// scope's VPC, reports. The instance is then present and unreachable.
	withoutAddress bool
}

// testFilestoreAddress is the private address the mock reports for an
// instance. Derived from the name rather than fixed, so a test mounting
// two volumes can tell which server each one reaches.
func testFilestoreAddress(instance string) string {
	sum := sha256.Sum256([]byte(instance))
	return fmt.Sprintf("10.200.%d.%d", sum[0], sum[1])
}

func (m mockMonitor) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.rec.add(recordedResource{
		Type:   args.TypeToken,
		Name:   args.Name,
		Inputs: args.Inputs,
	})

	outputs := args.Inputs.Copy()
	// Outputs the real provider computes and that dependent resources
	// read back, without which the power schedules of RFC 012 §4.2 would
	// see an unknown project and their inputs could not be asserted on.
	switch args.TypeToken {
	case "random:index/randomPassword:RandomPassword":
		outputs["result"] = resource.NewStringProperty("generated-password")
	case databaseInstanceToken:
		outputs["project"] = resource.NewStringProperty(testProjectID)
		outputs["name"] = resource.NewStringProperty(args.Name)
	case serviceAccountToken:
		outputs["email"] = resource.NewStringProperty(
			args.Inputs["accountId"].StringValue() + "@" + testProjectID + ".iam.gserviceaccount.com")
	case artifactRepositoryToken:
		outputs["project"] = resource.NewStringProperty(testProjectID)
	case customRoleToken:
		outputs["name"] = resource.NewStringProperty(
			"projects/" + testProjectID + "/roles/" + args.Inputs["roleId"].StringValue())
	case resourcePolicyTokn:
		outputs["selfLink"] = resource.NewStringProperty(
			"https://www.googleapis.com/compute/v1/projects/" + testProjectID + "/regions/europe-west1/resourcePolicies/" + args.Name)
	}
	return args.Name + "-id", outputs, nil
}

func (m mockMonitor) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	// The image repository a container_service fed by a pipeline resolves
	// its project from (RFC 018 §2.4.1).
	if args.Token == getArtifactRepositoryToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getArtifactRepositoryToken, Inputs: args.Args})
		return resource.PropertyMap{
			"project":      resource.NewStringProperty(testProjectID),
			"repositoryId": args.Args["repositoryId"],
			"location":     args.Args["location"],
		}, nil
	}
	// The Filestore instance a mounting resource stack finds rather than
	// creates (RFC 020 §2.8), by the name both halves derive from the same
	// scope and volume name.
	if args.Token == getFilestoreInstanceToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getFilestoreInstanceToken, Inputs: args.Args})
		name := args.Args["name"].StringValue()
		if name == m.filestore.missingInstance {
			return nil, fmt.Errorf("no Filestore instance named %q", name)
		}
		addresses := []resource.PropertyValue{resource.NewStringProperty(testFilestoreAddress(name))}
		if m.filestore.withoutAddress {
			addresses = nil
		}
		return resource.PropertyMap{
			"id":       resource.NewStringProperty(name),
			"name":     resource.NewStringProperty(name),
			"location": args.Args["location"],
			"tier":     resource.NewStringProperty(filestoreTier),
			"fileShares": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewObjectProperty(resource.PropertyMap{
					"name":       resource.NewStringProperty(filestoreShareName),
					"capacityGb": resource.NewNumberProperty(1024),
				}),
			}),
			"networks": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewObjectProperty(resource.PropertyMap{
					"network":     resource.NewStringProperty(testNetworkName),
					"connectMode": resource.NewStringProperty(filestoreConnectMode),
					"ipAddresses": resource.NewArrayProperty(addresses),
				}),
			}),
		}, nil
	}
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
				_, err := declareRelationalDatabase(ctx, "app-db", "us-east1", testNetworkName, tt.props)
				return err
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
		_, err := declareRelationalDatabase(ctx, "app-db", "us-east1", testNetworkName, props)
		return err
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
		_, err := declareRelationalDatabase(ctx, "app-db", "us-east1", testNetworkName, props)
		return err
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
