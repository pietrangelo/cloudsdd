// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type recordedResource struct {
	Type   string
	Name   string
	Inputs resource.PropertyMap
	// DependsOn holds the URNs the resource was registered after, both
	// the explicit DependsOn and those its input outputs imply.
	DependsOn []string
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
	rec   *recorder
	files fileScope
}

// fileScope is what the scope's network stack left behind for a mounting
// resource stack to find (RFC 020 §2.8). The zero value is a scope holding
// every account and share asked for; each field removes one of them, which
// is how a test reaches the refusal of a filesystem gone out of band.
//
// A refused lookup names nothing, as Azure's own errors need not, so a
// test asserting the refusal names the account or share proves the
// provider says so itself.
type fileScope struct {
	missingAccount bool
	missingShare   string
}

// testStorageAccountKey is the key the mock hands out for an account.
// Derived from the name, so an assertion can tell which account's key
// reached which storage link, and distinctive, so a walk of every recorded
// input can find it wherever it leaked.
func testStorageAccountKey(account string) string {
	return "storage-key-for-" + account + "=="
}

func (m mockMonitor) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.rec.add(recordedResource{
		Type:      args.TypeToken,
		Name:      args.Name,
		Inputs:    args.Inputs,
		DependsOn: args.RegisterRPC.GetDependencies(),
	})

	outputs := args.Inputs.Copy()
	if args.TypeToken == "random:index/randomPassword:RandomPassword" {
		outputs["result"] = resource.NewStringProperty("generated-password")
	}
	if args.TypeToken == "azure:core/resourceGroup:ResourceGroup" {
		outputs["name"] = resource.NewStringProperty(args.Name)
	}
	// The custom-domain records read these back (RFC 017 §2.3.1): a CNAME
	// pointing at the app's FQDN, and a TXT record carrying the
	// verification id Azure issues the managed certificate against.
	if args.TypeToken == containerAppToken {
		outputs["customDomainVerificationId"] = resource.NewStringProperty("verification-id")
		if ingress, ok := args.Inputs["ingress"]; ok && ingress.IsObject() {
			withFqdn := ingress.ObjectValue().Copy()
			withFqdn["fqdn"] = resource.NewStringProperty(args.Name + ".westeurope.azurecontainerapps.io")
			outputs["ingress"] = resource.NewObjectProperty(withFqdn)
		}
	}
	// Outputs the real provider computes and that dependent resources
	// read back (RFC 012 §4.3): the automation account's managed identity
	// and the names the job schedules refer to.
	if args.TypeToken == automationAccountToken {
		outputs["name"] = resource.NewStringProperty(args.Name)
		outputs["identity"] = resource.NewObjectProperty(resource.PropertyMap{
			"type":        resource.NewStringProperty("SystemAssigned"),
			"principalId": resource.NewStringProperty(testPrincipalID),
		})
	}
	if args.TypeToken == runbookToken || args.TypeToken == automationScheduleToken {
		outputs["name"] = args.Inputs["name"]
	}
	// The login server is computed by ACR from the registry's name, and
	// both halves of RFC 018 §2.4.1 read it back: the build task tags what
	// it pushes with it, and the service resolves its image through it.
	if args.TypeToken == acrRegistryToken {
		outputs["loginServer"] = resource.NewStringProperty(
			args.Inputs["name"].StringValue() + acrLoginServerSuffix)
	}
	// The generated host key of RFC 013 §2.4: the VM reads back the
	// public half, so without it the declaration sees an unknown.
	if args.TypeToken == privateKeyToken {
		outputs["publicKeyOpenssh"] = resource.NewStringProperty("ssh-ed25519 AAAAC3Nz test")
		outputs["privateKeyOpenssh"] = resource.NewStringProperty("-----BEGIN OPENSSH PRIVATE KEY-----")
	}
	return args.Name + "-id", outputs, nil
}

// Scope-network discovery (RFC 016 §2.2). Azure resource IDs carry the
// subscription, which a resource program cannot compute, so unlike GCP
// the IDs are looked up — from names both stacks derive from the scope.
const (
	getSubnetToken  = "azure:network/getSubnet:getSubnet"
	getDnsZoneToken = "azure:privatedns/getDnsZone:getDnsZone"
	// The public DNS zone a container service's custom domain is verified
	// in (RFC 017 §2.3.1). Distinct from the private zones a database
	// uses.
	getPublicDNSZoneToken = "azure:dns/getZone:getZone"
	// The scope's storage account and one share in it, as a mounting
	// resource stack finds them (RFC 020 §2.8). The account lookup is also
	// where the key comes from: classic Azure has no separate key-listing
	// call, and getAccount answers with the primary key.
	getStorageAccountToken = "azure:storage/getAccount:getAccount"
	getFileShareToken      = "azure:storage/getShare:getShare"
)

const (
	testSubnetID  = "/subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/general"
	testDNSZoneID = "/subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.Network/privateDnsZones/z"
)

func (m mockMonitor) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	switch args.Token {
	case getSubnetToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getSubnetToken, Inputs: args.Args})
		return resource.PropertyMap{
			"id":            resource.NewStringProperty(testSubnetID),
			"addressPrefix": resource.NewStringProperty("10.42.0.0/24"),
		}, nil
	case getDnsZoneToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getDnsZoneToken, Inputs: args.Args})
		return resource.PropertyMap{
			"id": resource.NewStringProperty(testDNSZoneID),
		}, nil
	case getPublicDNSZoneToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getPublicDNSZoneToken, Inputs: args.Args})
		return resource.PropertyMap{
			"id":                resource.NewStringProperty("/subscriptions/sub-id/zones/acme.example"),
			"name":              args.Args["name"],
			"resourceGroupName": resource.NewStringProperty("dns-rg"),
		}, nil
	// The subscription the ambient credentials resolve to (RFC 018 §2.8.1).
	// An ACR name is globally unique, so it is part of the derivation on
	// both sides — which means both sides have to ask this question.
	case getClientConfigToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getClientConfigToken, Inputs: args.Args})
		return resource.PropertyMap{
			"subscriptionId": resource.NewStringProperty(testSubscriptionID),
			"tenantId":       resource.NewStringProperty("00000000-0000-0000-0000-00000000000t"),
			"clientId":       resource.NewStringProperty("00000000-0000-0000-0000-00000000000c"),
			"objectId":       resource.NewStringProperty("00000000-0000-0000-0000-00000000000o"),
			"id":             resource.NewStringProperty("client-config"),
		}, nil
	// The registry the pipeline created, as the service's side of RFC 018
	// §2.4.1 looks it up: by a name and a resource group both derived from
	// the image name, which is all the Engine hands across.
	case getAcrRegistryToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getAcrRegistryToken, Inputs: args.Args})
		name := args.Args["name"].StringValue()
		return resource.PropertyMap{
			"id": resource.NewStringProperty(
				"/subscriptions/sub-id/resourceGroups/" +
					args.Args["resourceGroupName"].StringValue() +
					"/providers/Microsoft.ContainerRegistry/registries/" + name),
			"name":              args.Args["name"],
			"resourceGroupName": args.Args["resourceGroupName"],
			"loginServer":       resource.NewStringProperty(name + acrLoginServerSuffix),
			"location":          resource.NewStringProperty("westeurope"),
			"sku":               resource.NewStringProperty(acrSKUBasic),
			"adminEnabled":      resource.NewBoolProperty(false),
		}, nil
	// The scope's storage account as the mounting side of RFC 020 §2.8
	// finds it, carrying the key the Container Apps storage link needs.
	case getStorageAccountToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getStorageAccountToken, Inputs: args.Args})
		name := args.Args["name"].StringValue()
		if m.files.missingAccount {
			return nil, errors.New("storage account not found")
		}
		return resource.PropertyMap{
			"id": resource.NewStringProperty(
				"/subscriptions/sub-id/resourceGroups/" + args.Args["resourceGroupName"].StringValue() +
					"/providers/Microsoft.Storage/storageAccounts/" + name),
			"name":              args.Args["name"],
			"resourceGroupName": args.Args["resourceGroupName"],
			"primaryAccessKey":  resource.NewStringProperty(testStorageAccountKey(name)),
		}, nil
	case getFileShareToken:
		m.rec.add(recordedResource{Type: args.Token, Name: getFileShareToken, Inputs: args.Args})
		name := args.Args["name"].StringValue()
		if name == m.files.missingShare {
			return nil, errors.New("share not found")
		}
		return resource.PropertyMap{
			"id":                 resource.NewStringProperty("https://files.example/" + name),
			"name":               args.Args["name"],
			"storageAccountName": args.Args["storageAccountName"],
			"quota":              resource.NewNumberProperty(100),
		}, nil
	}
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
				_, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(), props)
				return err
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
					_, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(), props)
					return err
				})

				server := findResource(t, recorded, tt.typeToken)
				if _, ok := server.Inputs["highAvailability"]; ok {
					t.Error("highAvailability was configured despite high_availability being false")
				}
			})

			t.Run("with HA", func(t *testing.T) {
				props := relationalDatabaseProperties{Engine: tt.engine, Version: "15", HighAvailability: true}
				recorded := runProgram(t, func(ctx *pulumi.Context) error {
					_, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(), props)
					return err
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

// TestDeclareDatabaseIsNotPubliclyReachable is the regression test for
// the gap RFC 015 §1 records: MySQL Flexible Server exposes no
// PublicNetworkAccessEnabled at all, so it shipped with a public endpoint
// while docs/cli.md claimed the resource was not publicly reachable.
//
// Both engines are in one table because RFC 015 §2.2's whole point is
// that they no longer differ: a test that checked them separately would
// let them drift apart again without failing.
func TestDeclareDatabaseIsNotPubliclyReachable(t *testing.T) {
	tests := []struct {
		name       string
		engine     string
		version    string
		typeToken  string
		delegation string
		dnsSuffix  string
	}{
		{
			name:       "postgres",
			engine:     "postgres",
			version:    "15",
			typeToken:  "azure:postgresql/flexibleServer:FlexibleServer",
			delegation: "Microsoft.DBforPostgreSQL/flexibleServers",
			dnsSuffix:  "postgres.database.azure.com",
		},
		{
			name:       "mysql",
			engine:     "mysql",
			version:    "8.0.21",
			typeToken:  "azure:mysql/flexibleServer:FlexibleServer",
			delegation: "Microsoft.DBforMySQL/flexibleServers",
			dnsSuffix:  "mysql.database.azure.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := relationalDatabaseProperties{Engine: tt.engine, Version: tt.version}

			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				_, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(), props)
				return err
			})

			// The server is in a VNet, which is what removes the public
			// endpoint rather than leaving it merely unfirewalled.
			server := findResource(t, recorded, tt.typeToken)
			if _, ok := server.Inputs["delegatedSubnetId"]; !ok {
				t.Error("delegatedSubnetId was not set; the server keeps a public endpoint")
			}
			if _, ok := server.Inputs["privateDnsZoneId"]; !ok {
				t.Error("privateDnsZoneId was not set; the server's hostname will not resolve privately")
			}
			if got := server.Inputs["backupRetentionDays"].NumberValue(); got != backupRetentionDays {
				t.Errorf("backupRetentionDays = %v, want %d", got, backupRetentionDays)
			}

			// The subnet and zone come from the scope's network, which
			// the Engine provisioned first (RFC 016 §2.2). RFC 015
			// declared both per database; asserting they are *not*
			// declared here is what pins the change.
			for _, token := range []string{
				"azure:network/subnet:Subnet",
				"azure:privatedns/zone:Zone",
				"azure:network/virtualNetwork:VirtualNetwork",
			} {
				if hasResource(recorded, token) {
					t.Errorf("%s was declared per database; it belongs to the scope network", token)
				}
			}

			// The lookup must ask for this engine's delegated subnet and
			// its zone, or the server lands in the wrong one.
			lookup := findResource(t, recorded, getSubnetToken)
			if got := lookup.Inputs["name"].StringValue(); got != tt.engine {
				t.Errorf("looked up subnet %q, want the %q delegated subnet", got, tt.engine)
			}
			if got := lookup.Inputs["virtualNetworkName"].StringValue(); got != scopeNetworkName(testScope()) {
				t.Errorf("looked up vnet %q, want the scope's %q", got, scopeNetworkName(testScope()))
			}

			zoneLookup := findResource(t, recorded, getDnsZoneToken)
			if got := zoneLookup.Inputs["name"].StringValue(); !strings.HasSuffix(got, tt.dnsSuffix) {
				t.Errorf("looked up zone %q, want one ending in %q", got, tt.dnsSuffix)
			}

			// And the looked-up identifiers are what reach the server.
			if got := server.Inputs["delegatedSubnetId"].StringValue(); got != testSubnetID {
				t.Errorf("delegatedSubnetId = %q, want the looked-up subnet", got)
			}
			if got := server.Inputs["privateDnsZoneId"].StringValue(); got != testDNSZoneID {
				t.Errorf("privateDnsZoneId = %q, want the looked-up zone", got)
			}
		})
	}
}

// TestDeclareDatabaseDeletionProtection covers RFC 015 §2.1. The property
// existed on AWS and GCP and was documented as universal, while Azure had
// no field at all — so the documented escape hatch, deletion_protection:
// false, was rejected as an unknown property on the one provider where it
// could not be relaxed.
func TestDeclareDatabaseDeletionProtection(t *testing.T) {
	const lockToken = "azure:management/lock:Lock"

	tests := []struct {
		name     string
		props    relationalDatabaseProperties
		wantLock bool
	}{
		{
			name:     "on by default",
			props:    relationalDatabaseProperties{Engine: "postgres", Version: "15"},
			wantLock: true,
		},
		{
			name: "explicitly on",
			props: relationalDatabaseProperties{
				Engine: "mysql", Version: "8.0.21", DeletionProtection: boolPtr(true),
			},
			wantLock: true,
		},
		{
			// Asserting the absence matters: a lock left behind at false
			// would make the documented escape hatch a lie.
			name: "explicitly off declares no lock",
			props: relationalDatabaseProperties{
				Engine: "postgres", Version: "15", DeletionProtection: boolPtr(false),
			},
			wantLock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				_, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(), tt.props)
				return err
			})

			if got := hasResource(recorded, lockToken); got != tt.wantLock {
				t.Fatalf("management lock declared = %v, want %v", got, tt.wantLock)
			}
			if !tt.wantLock {
				return
			}

			lock := findResource(t, recorded, lockToken)
			// CanNotDelete, never ReadOnly: ReadOnly is the stronger lock
			// and would break RFC 012's start/stop runbook, whose only
			// symptom would be an unchanged bill.
			if got := lock.Inputs["lockLevel"].StringValue(); got != lockLevelCanNotDelete {
				t.Errorf("lockLevel = %q, want %q", got, lockLevelCanNotDelete)
			}
			if _, ok := lock.Inputs["scope"]; !ok {
				t.Error("lock has no scope; it must name the server it protects")
			}
		})
	}
}

func TestEffectiveEncryptionDefaultsOn(t *testing.T) {
	var p objectStorageProperties
	if !p.EffectiveEncryption() {
		t.Error("EffectiveEncryption() = false, want true when absent")
	}
}
