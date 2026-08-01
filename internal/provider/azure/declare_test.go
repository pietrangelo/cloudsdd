// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

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

// mockMonitor captures declared resources without contacting Azure.
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
	if args.TypeToken == "random:index/randomPassword:RandomPassword" {
		outputs["result"] = resource.NewStringProperty("generated-password")
	}
	if args.TypeToken == "azure:core/resourceGroup:ResourceGroup" {
		outputs["name"] = resource.NewStringProperty(args.Name)
	}
	return args.Name + "-id", outputs, nil
}

func (m mockMonitor) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func runProgram(t *testing.T, fn func(ctx *pulumi.Context) error) []recordedResource {
	t.Helper()

	rec := &recorder{}
	if err := pulumi.RunErr(fn, pulumi.WithMocks("cloudsdd-azure", "test", mockMonitor{rec: rec})); err != nil {
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

func hasResource(recorded []recordedResource, typeToken string) bool {
	for _, r := range recorded {
		if r.Type == typeToken {
			return true
		}
	}
	return false
}

func typeTokens(recorded []recordedResource) []string {
	out := make([]string, 0, len(recorded))
	for _, r := range recorded {
		out = append(out, r.Type)
	}
	return out
}

// TestDeclareObjectStorageUsesRequestedName is the end-to-end regression
// test for RFC 011 §1.1A4: the storage account name was derived from the
// Resource ID via strings.ReplaceAll(id, "-", "") and truncated at 24
// characters, while the decoded, required bucket_name was discarded.
func TestDeclareObjectStorageUsesRequestedName(t *testing.T) {
	props := objectStorageProperties{BucketName: "mystorageacct"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		// A Resource ID that the old mangling would have produced a
		// different (and invalid) account name from.
		return declareObjectStorage(ctx, "My_Bucket-With-Dashes", "westeurope", props)
	})

	account := findResource(t, recorded, "azure:storage/account:Account")
	if got := account.Inputs["name"].StringValue(); got != "mystorageacct" {
		t.Errorf("storage account name = %q, want the requested %q", got, "mystorageacct")
	}
}

func TestDeclareObjectStorageSecureDefaults(t *testing.T) {
	props := objectStorageProperties{BucketName: "mystorageacct"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareObjectStorage(ctx, "bucket", "westeurope", props)
	})
	account := findResource(t, recorded, "azure:storage/account:Account")

	if !account.Inputs["httpsTrafficOnlyEnabled"].BoolValue() {
		t.Error("httpsTrafficOnlyEnabled = false, want true")
	}
	if got := account.Inputs["minTlsVersion"].StringValue(); got != "TLS1_2" {
		t.Errorf("minTlsVersion = %q, want TLS1_2", got)
	}
	if account.Inputs["allowNestedItemsToBePublic"].BoolValue() {
		t.Error("allowNestedItemsToBePublic = true, want false by default")
	}
	if !account.Inputs["infrastructureEncryptionEnabled"].BoolValue() {
		t.Error("infrastructureEncryptionEnabled = false, want true")
	}

	// RFC 011 §2.5: AllowNestedItemsToBePublic only governs anonymous
	// blob access, so the posture is incomplete without a default-deny
	// network rule.
	rules, ok := account.Inputs["networkRules"]
	if !ok {
		t.Fatal("no networkRules were declared; the account is reachable from any network")
	}
	if got := rules.ObjectValue()["defaultAction"].StringValue(); got != "Deny" {
		t.Errorf("networkRules.defaultAction = %q, want Deny", got)
	}

	container := findResource(t, recorded, "azure:storage/container:Container")
	if got := container.Inputs["containerAccessType"].StringValue(); got != "private" {
		t.Errorf("containerAccessType = %q, want private", got)
	}

	rg := findResource(t, recorded, "azure:core/resourceGroup:ResourceGroup")
	if got := rg.Inputs["location"].StringValue(); got != "westeurope" {
		t.Errorf("resource group location = %q, want the requested location", got)
	}
}

func TestDeclareObjectStorageAllowsExplicitPublicAccess(t *testing.T) {
	no := false
	props := objectStorageProperties{BucketName: "mystorageacct", BlockPublicAccess: &no}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareObjectStorage(ctx, "bucket", "westeurope", props)
	})
	account := findResource(t, recorded, "azure:storage/account:Account")

	if !account.Inputs["allowNestedItemsToBePublic"].BoolValue() {
		t.Error("allowNestedItemsToBePublic = false, want the explicitly requested true")
	}
	if _, ok := account.Inputs["networkRules"]; ok {
		t.Error("networkRules were applied despite public access being explicitly allowed")
	}
}

// TestDeclareRelationalDatabaseSelectsEngine is the end-to-end regression
// test for RFC 011 §1.1A3: the provider always called
// postgresql.NewFlexibleServer regardless of engine.
func TestDeclareRelationalDatabaseSelectsEngine(t *testing.T) {
	const (
		postgresToken = "azure:postgresql/flexibleServer:FlexibleServer"
		mysqlToken    = "azure:mysql/flexibleServer:FlexibleServer"
	)

	tests := []struct {
		name       string
		engine     string
		wantToken  string
		otherToken string
	}{
		{name: "postgres", engine: "postgres", wantToken: postgresToken, otherToken: mysqlToken},
		{name: "mysql", engine: "mysql", wantToken: mysqlToken, otherToken: postgresToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := relationalDatabaseProperties{Engine: tt.engine, Version: "15"}

			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				return declareRelationalDatabase(ctx, "app-db", "westeurope", props)
			})

			if !hasResource(recorded, tt.wantToken) {
				t.Errorf("no %q was declared for engine %q; got %v", tt.wantToken, tt.engine, typeTokens(recorded))
			}
			if hasResource(recorded, tt.otherToken) {
				t.Errorf("engine %q silently produced a %q", tt.engine, tt.otherToken)
			}
		})
	}
}

// TestDeclareRelationalDatabaseHonoursHighAvailability covers the other
// half of §1.1A3: HighAvailability was decoded and never read.
func TestDeclareRelationalDatabaseHonoursHighAvailability(t *testing.T) {
	tests := []struct {
		name      string
		engine    string
		typeToken string
	}{
		{name: "postgres", engine: "postgres", typeToken: "azure:postgresql/flexibleServer:FlexibleServer"},
		{name: "mysql", engine: "mysql", typeToken: "azure:mysql/flexibleServer:FlexibleServer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("without HA", func(t *testing.T) {
				props := relationalDatabaseProperties{Engine: tt.engine, Version: "15"}
				recorded := runProgram(t, func(ctx *pulumi.Context) error {
					return declareRelationalDatabase(ctx, "app-db", "westeurope", props)
				})

				server := findResource(t, recorded, tt.typeToken)
				if _, ok := server.Inputs["highAvailability"]; ok {
					t.Error("highAvailability was configured despite high_availability being false")
				}
			})

			t.Run("with HA", func(t *testing.T) {
				props := relationalDatabaseProperties{Engine: tt.engine, Version: "15", HighAvailability: true}
				recorded := runProgram(t, func(ctx *pulumi.Context) error {
					return declareRelationalDatabase(ctx, "app-db", "westeurope", props)
				})

				server := findResource(t, recorded, tt.typeToken)
				ha, ok := server.Inputs["highAvailability"]
				if !ok {
					t.Fatal("high_availability was requested but never reached the server configuration")
				}
				if got := ha.ObjectValue()["mode"].StringValue(); got != "ZoneRedundant" {
					t.Errorf("highAvailability.mode = %q, want ZoneRedundant", got)
				}
				if !server.Inputs["geoRedundantBackupEnabled"].BoolValue() {
					t.Error("geoRedundantBackupEnabled = false, want true for an HA instance")
				}
			})
		})
	}
}

// TestDeclarePostgresIsNotPubliclyReachable covers RFC 011 §1.1E: the
// source claimed "Flexible server is private by default without firewall
// rules", but Azure's default is public network access enabled.
func TestDeclarePostgresIsNotPubliclyReachable(t *testing.T) {
	props := relationalDatabaseProperties{Engine: "postgres", Version: "15"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareRelationalDatabase(ctx, "app-db", "westeurope", props)
	})
	server := findResource(t, recorded, "azure:postgresql/flexibleServer:FlexibleServer")

	enabled, ok := server.Inputs["publicNetworkAccessEnabled"]
	if !ok {
		t.Fatal("publicNetworkAccessEnabled was not set; the server defaults to publicly reachable")
	}
	if enabled.BoolValue() {
		t.Error("publicNetworkAccessEnabled = true, want false")
	}
	if got := server.Inputs["backupRetentionDays"].NumberValue(); got != backupRetentionDays {
		t.Errorf("backupRetentionDays = %v, want %d", got, backupRetentionDays)
	}
}

func TestEffectiveEncryptionDefaultsOn(t *testing.T) {
	var p objectStorageProperties
	if !p.EffectiveEncryption() {
		t.Error("EffectiveEncryption() = false, want true when absent")
	}
}
