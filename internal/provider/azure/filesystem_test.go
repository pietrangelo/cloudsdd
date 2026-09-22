// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

const (
	storageAccountToken  = "azure:storage/account:Account"
	fileShareToken       = "azure:storage/share:Share"
	privateEndpointToken = "azure:privatelink/endpoint:Endpoint"
)

// fileZoneName is the one private DNS zone name Azure resolves a storage
// account's file endpoint through once a private endpoint exists. It is
// not ours to choose: any other name and the account's public hostname
// keeps resolving to the public address it no longer answers on.
const fileZoneName = "privatelink.file.core.windows.net"

// storageAccountNamePattern is what Azure accepts for a storage account:
// 3 to 24 characters, lowercase letters and digits only.
var storageAccountNamePattern = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

// fileShareNamePattern is what Azure accepts for a file share: 3 to 63
// characters of lowercase letters, digits and hyphens, beginning and
// ending with a letter or digit. The ban on consecutive hyphens is checked
// separately; a regular expression that encodes it hides it.
var fileShareNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// testFileScopeContents is what the Engine hands the network stack for a
// scope whose resources mount two filesystems (RFC 020 §2.3).
//
// One volume states a size and the other does not, so both halves of the
// §2.7 rule are exercised by one program: an explicit quota honoured, an
// absent one defaulted rather than refused.
func testFileScopeContents() provider.ScopeContents {
	return provider.ScopeContents{Volumes: []spec.Volume{
		{Name: "uploads", SizeGB: 250},
		{Name: "cache"},
	}}
}

// TestDeclareScopeFilesystems covers the Azure Files mapping of RFC 020
// §2.6: one storage account for the scope, one share per volume in it,
// and no path to either from the internet.
func TestDeclareScopeFilesystems(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")
	contents := testFileScopeContents()

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, contents)
	})

	// One account per scope, not per volume: the account is the thing
	// that is network-attached, and the quota lives on the share.
	accounts := resourcesOfType(recorded, storageAccountToken)
	if len(accounts) != 1 {
		t.Fatalf("declared %d storage accounts for %d volumes, want exactly 1 for the scope",
			len(accounts), len(contents.Volumes))
	}
	account := accounts[0]

	if got, want := account.Inputs["name"].StringValue(), storageAccountNameFor(testSubscriptionID, testScope()); got != want {
		t.Errorf("storage account name = %q, want the derived %q; the mounting stack derives that name "+
			"and would find nothing", got, want)
	}

	assertStorageAccountHardened(t, account)
	assertFileShares(t, recorded, contents)
	assertFilePrivateEndpoint(t, recorded, account)
}

// assertStorageAccountHardened pins the account's posture. Every value is
// asserted present rather than merely not-wrong, because Azure's default
// for each one is the more open choice.
func assertStorageAccountHardened(t *testing.T, account recordedResource) {
	t.Helper()

	// Unset means enabled. A share reachable from the internet with only
	// an account key in front of it is the exposure this RFC exists to
	// rule out.
	public, set := account.Inputs["publicNetworkAccessEnabled"]
	if !set || !public.IsBool() || public.BoolValue() {
		t.Errorf("publicNetworkAccessEnabled = %v, want an explicit false", public)
	}

	if got := stringOrEmpty(account.Inputs["minTlsVersion"]); got != "TLS1_2" {
		t.Errorf("minTlsVersion = %q, want TLS1_2", got)
	}

	shareProps := objectOrEmpty(account.Inputs["shareProperties"])

	// SMB 3.1.1 with AES-256-GCM is the only combination that encrypts on
	// the wire. Allowing an older dialect alongside it lets a client
	// negotiate down to an unencrypted channel.
	smb := objectOrEmpty(shareProps["smb"])
	if got := stringsOf(smb["versions"]); len(got) != 1 || got[0] != "SMB3.1.1" {
		t.Errorf("smb versions = %v, want only SMB3.1.1", got)
	}
	if got := stringsOf(smb["channelEncryptionTypes"]); len(got) != 1 || got[0] != "AES-256-GCM" {
		t.Errorf("smb channelEncryptionTypes = %v, want only AES-256-GCM", got)
	}

	// Destroying the scope destroys the data (RFC 020 §2.9). Soft-delete
	// is the only undo Azure Files has, and it is off unless asked for.
	retention := objectOrEmpty(shareProps["retentionPolicy"])
	if got := numberOrZero(retention["days"]); got != 7 {
		t.Errorf("share soft-delete retention = %v days, want 7", got)
	}
}

// assertFileShares pins one share per volume, in the scope's account,
// under the name the mounting stack derives, with the quota §2.7 gives it.
func assertFileShares(t *testing.T, recorded []recordedResource, contents provider.ScopeContents) {
	t.Helper()

	shares := resourcesOfType(recorded, fileShareToken)
	if len(shares) != len(contents.Volumes) {
		t.Fatalf("declared %d file shares, want one per volume (%d)", len(shares), len(contents.Volumes))
	}

	wantQuota := map[string]float64{"uploads": 250, "cache": 100}
	for _, v := range contents.Volumes {
		want := fileShareNameFor(v.Name)
		share, ok := resourceNamed(shares, want)
		if !ok {
			t.Errorf("volume %q: no file share named %q", v.Name, want)
			continue
		}
		if got := numberOrZero(share.Inputs["quota"]); got != wantQuota[v.Name] {
			t.Errorf("volume %q (size_gb %d): quota = %v GiB, want %v", v.Name, v.SizeGB, got, wantQuota[v.Name])
		}
		if got, want := stringOrEmpty(share.Inputs["storageAccountName"]), storageAccountNameFor(testSubscriptionID, testScope()); got != want {
			t.Errorf("volume %q: share is in account %q, want the scope's account %q", v.Name, got, want)
		}
	}
}

// assertFilePrivateEndpoint pins the one way into the account: a private
// endpoint in the scope's general subnet, and the private DNS zone that
// makes the account's hostname resolve to it from inside the VNet.
//
// The general subnet, because every other subnet in the scope is delegated
// to one service and a delegated subnet refuses a private endpoint.
func assertFilePrivateEndpoint(t *testing.T, recorded []recordedResource, account recordedResource) {
	t.Helper()

	endpoints := resourcesOfType(recorded, privateEndpointToken)
	if len(endpoints) != 1 {
		t.Fatalf("declared %d private endpoints, want exactly 1 for the scope's account", len(endpoints))
	}
	endpoint := endpoints[0]

	general, ok := resourceNamed(resourcesOfType(recorded, subnetToken), generalSubnetName)
	if !ok {
		t.Fatalf("no %q subnet was declared", generalSubnetName)
	}
	if got, want := stringOrEmpty(endpoint.Inputs["subnetId"]), general.Name+"-id"; got != want {
		t.Errorf("private endpoint subnetId = %q, want the general subnet %q", got, want)
	}

	connection := objectOrEmpty(endpoint.Inputs["privateServiceConnection"])
	if got, want := stringOrEmpty(connection["privateConnectionResourceId"]), account.Name+"-id"; got != want {
		t.Errorf("private endpoint connects to %q, want the scope's storage account %q", got, want)
	}
	if got := stringsOf(connection["subresourceNames"]); len(got) != 1 || got[0] != "file" {
		t.Errorf("private endpoint subresources = %v, want only file", got)
	}
	// A manual connection waits for someone to approve it in the portal,
	// and until then the share is unreachable from the scope too.
	if manual := connection["isManualConnection"]; !manual.IsBool() || manual.BoolValue() {
		t.Errorf("isManualConnection = %v, want an explicit false", manual)
	}

	zone, ok := resourceNamed(resourcesOfType(recorded, dnsZoneToken), fileZoneName)
	if !ok {
		t.Fatalf("no %q private DNS zone was declared; the account's hostname would still resolve "+
			"to its public address", fileZoneName)
	}
	group := objectOrEmpty(endpoint.Inputs["privateDnsZoneGroup"])
	if got := stringsOf(group["privateDnsZoneIds"]); len(got) != 1 || got[0] != zone.Name+"-id" {
		t.Errorf("private endpoint registers in zones %v, want only %q", got, zone.Name+"-id")
	}

	vnet := findResource(t, recorded, vnetToken)
	linked := false
	for _, link := range resourcesOfType(recorded, dnsLinkToken) {
		if stringOrEmpty(link.Inputs["privateDnsZoneName"]) == fileZoneName &&
			stringOrEmpty(link.Inputs["virtualNetworkId"]) == vnet.Name+"-id" {
			linked = true
		}
	}
	if !linked {
		t.Errorf("the %q zone is not linked to the scope VNet; it resolves for nobody", fileZoneName)
	}
}

// TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem keeps every
// existing scope unchanged: a scope that mounts nothing gets no account,
// no share, no endpoint and no file zone.
func TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	for _, token := range []string{storageAccountToken, fileShareToken, privateEndpointToken} {
		if n := len(resourcesOfType(recorded, token)); n != 0 {
			t.Errorf("declared %d %s with no volumes in the scope, want none", n, token)
		}
	}
	if _, ok := resourceNamed(resourcesOfType(recorded, dnsZoneToken), fileZoneName); ok {
		t.Errorf("declared the %q zone with no volumes in the scope", fileZoneName)
	}
}

// TestStorageAccountNameIsDerivable is what lets the network stack and the
// mounting stack agree on an account without talking to each other (RFC
// 020 §2.5).
//
// Pinned literally rather than recomputed: the name is the identity of a
// live account holding data, and a formula that drifts renames it — which
// on Azure is a replacement.
func TestStorageAccountNameIsDerivable(t *testing.T) {
	got := storageAccountNameFor(testSubscriptionID, testScope())
	if want := "cloudsdd1d0d4087c5866615"; got != want {
		t.Errorf("storageAccountNameFor() = %q, want %q", got, want)
	}
}

// TestStorageAccountNameIsAValidAndDistinctName attacks the derivation.
//
// A storage account name is drawn from a namespace shared with every other
// Azure customer, so the subscription is part of the identity: two
// subscriptions deploying the same scope must not name the same account.
// If they did, the second apply would fail at best — and the name is the
// only thing standing between two tenants' data.
func TestStorageAccountNameIsAValidAndDistinctName(t *testing.T) {
	const otherSubscription = "00000000-0000-0000-0000-000000000001"
	long := provider.NetworkScope{
		Provider:    spec.ProviderAzure,
		Account:     strings.Repeat("a", 32),
		Environment: strings.Repeat("E", 32),
		Region:      "westeurope",
	}

	type input struct {
		subscription string
		scope        provider.NetworkScope
	}
	inputs := []input{
		{testSubscriptionID, testScope()},
		{otherSubscription, testScope()},
		{testSubscriptionID, provider.NetworkScope{Provider: spec.ProviderAzure, Environment: "prod", Region: "westeurope"}},
		{testSubscriptionID, provider.NetworkScope{Provider: spec.ProviderAzure, Environment: "dev", Region: "northeurope"}},
		{testSubscriptionID, provider.NetworkScope{}},
		{testSubscriptionID, long},
		// A subscription ending where a scope begins must not hash as
		// another pair.
		{"", testScope()},
	}

	seen := make(map[string]input, len(inputs))
	for _, in := range inputs {
		name := storageAccountNameFor(in.subscription, in.scope)
		if !storageAccountNamePattern.MatchString(name) {
			t.Errorf("subscription %q, scope %q: %q is not a storage account name Azure accepts "+
				"(3–24 lowercase letters and digits)", in.subscription, provider.ScopeName(in.scope), name)
		}
		if prior, taken := seen[name]; taken {
			t.Errorf("scope %q in subscription %q and scope %q in subscription %q both derive %q",
				provider.ScopeName(prior.scope), prior.subscription,
				provider.ScopeName(in.scope), in.subscription, name)
		}
		seen[name] = in
	}
}

// TestFileShareNameIsDerivable pins the share's name for the same reason
// as the account's: the mounting stack names it to link it.
func TestFileShareNameIsDerivable(t *testing.T) {
	if got, want := fileShareNameFor("uploads"), "uploads-9ba88c41"; got != want {
		t.Errorf("fileShareNameFor() = %q, want %q", got, want)
	}
}

// TestFileShareNameIsAValidAndDistinctName feeds the derivation what a
// volume name admits and a share name does not: uppercase, underscores,
// leading and trailing separators, runs of them, a single character, the
// full 32.
//
// The pairs are the ones a fold to Azure's charset would merge — "Cache"
// and "cache" are two volumes in the Specification, and one share for both
// would be two services silently sharing files neither asked to share.
func TestFileShareNameIsAValidAndDistinctName(t *testing.T) {
	volumes := []string{
		"cache", "Cache",
		"a_b", "a-b", "a__b", "a--b",
		"a", "a-", "-a", "_",
		"___", "---",
		"9lives",
		strings.Repeat("Z", 32),
		strings.Repeat("v_", 16),
	}

	seen := make(map[string]string, len(volumes))
	for _, v := range volumes {
		name := fileShareNameFor(v)
		if !fileShareNamePattern.MatchString(name) || strings.Contains(name, "--") {
			t.Errorf("volume %q: %q is not a file share name Azure accepts (3–63 lowercase letters, "+
				"digits and single hyphens, a letter or digit at each end)", v, name)
		}
		if prior, taken := seen[name]; taken {
			t.Errorf("volumes %q and %q both derive the share %q", prior, v, name)
		}
		seen[name] = v

		if again := fileShareNameFor(v); again != name {
			t.Errorf("volume %q derived %q then %q; the two stacks would disagree", v, name, again)
		}
	}
}

func resourceNamed(recorded []recordedResource, name string) (recordedResource, bool) {
	for _, r := range recorded {
		if stringOrEmpty(r.Inputs["name"]) == name {
			return r, true
		}
	}
	return recordedResource{}, false
}

// The accessors below read an input that may be absent or unknown without
// panicking, so a missing input fails with the assertion written for it
// rather than with a stack trace from the property library.

func stringOrEmpty(v resource.PropertyValue) string {
	if !v.IsString() {
		return ""
	}
	return v.StringValue()
}

func numberOrZero(v resource.PropertyValue) float64 {
	if !v.IsNumber() {
		return 0
	}
	return v.NumberValue()
}

func objectOrEmpty(v resource.PropertyValue) resource.PropertyMap {
	if !v.IsObject() {
		return resource.PropertyMap{}
	}
	return v.ObjectValue()
}

func stringsOf(v resource.PropertyValue) []string {
	if !v.IsArray() {
		return nil
	}
	out := make([]string, 0, len(v.ArrayValue()))
	for _, e := range v.ArrayValue() {
		out = append(out, stringOrEmpty(e))
	}
	return out
}
