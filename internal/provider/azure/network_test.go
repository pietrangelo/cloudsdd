// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

const (
	vnetToken     = "azure:network/virtualNetwork:VirtualNetwork"
	subnetToken   = "azure:network/subnet:Subnet"
	nsgAssocToken = "azure:network/subnetNetworkSecurityGroupAssociation:SubnetNetworkSecurityGroupAssociation"
	dnsZoneToken  = "azure:privatedns/zone:Zone"
	dnsLinkToken  = "azure:privatedns/zoneVirtualNetworkLink:ZoneVirtualNetworkLink"
	rgToken       = "azure:core/resourceGroup:ResourceGroup"

	natGatewayToken   = "azure:network/natGateway:NatGateway"
	natIPAssocToken   = "azure:network/natGatewayPublicIpAssociation:NatGatewayPublicIpAssociation"
	natSubnetAssocTok = "azure:network/subnetNatGatewayAssociation:SubnetNatGatewayAssociation"
)

func testScope() provider.NetworkScope {
	return provider.NetworkScope{
		Provider:    spec.ProviderAzure,
		Environment: "dev",
		Region:      "westeurope",
		Sealed:      true,
	}
}

// TestDeclareScopeNetwork covers RFC 016 §2.3 for Azure. The shape it
// replaces is RFC 015's: a VNet per database, containing nothing but that
// database, which was private and left no way to put an application
// beside the thing it was meant to talk to.
func TestDeclareScopeNetwork(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	// The network's resource group is its own. A shared network inside a
	// resource's group would be destroyed with that resource, taking the
	// rest of the scope's connectivity with it.
	rg := findResource(t, recorded, rgToken)
	if got := rg.Inputs["name"].StringValue(); got != scopeResourceGroupName(testScope()) {
		t.Errorf("resource group = %q, want the derived %q", got, scopeResourceGroupName(testScope()))
	}

	vnet := findResource(t, recorded, vnetToken)
	spaces := vnet.Inputs["addressSpaces"].ArrayValue()
	if len(spaces) != 1 || spaces[0].StringValue() != cidr.String() {
		t.Errorf("vnet address space = %v, want the derived %s", spaces, cidr)
	}

	// One general subnet, one per engine, and one for Container Apps: a
	// delegated subnet names one service and excludes every other
	// occupant, so none of them can be shared.
	subnets := resourcesOfType(recorded, subnetToken)
	if len(subnets) != 4 {
		t.Fatalf("declared %d subnets, want 4 (general, postgres, mysql, containerapps)", len(subnets))
	}

	byName := map[string]recordedResource{}
	for _, s := range subnets {
		byName[s.Inputs["name"].StringValue()] = s
	}
	for _, want := range []string{generalSubnetName, "postgres", "mysql", containerAppsSubnetName} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no %q subnet was declared", want)
		}
	}

	// The general subnet takes no delegation, or it could not host a VM.
	if d, ok := byName[generalSubnetName].Inputs["delegations"]; ok && len(d.ArrayValue()) != 0 {
		t.Error("the general subnet is delegated; it could not then host a virtual machine")
	}

	for engine, delegation := range map[string]string{
		"postgres":              "Microsoft.DBforPostgreSQL/flexibleServers",
		"mysql":                 "Microsoft.DBforMySQL/flexibleServers",
		containerAppsSubnetName: containerAppsDelegation,
	} {
		delegations := byName[engine].Inputs["delegations"].ArrayValue()
		if len(delegations) != 1 {
			t.Errorf("%s subnet has %d delegations, want 1", engine, len(delegations))
			continue
		}
		svc := delegations[0].ObjectValue()["serviceDelegation"].ObjectValue()
		if got := svc["name"].StringValue(); got != delegation {
			t.Errorf("%s delegation = %q, want %q", engine, got, delegation)
		}
	}

	// Subnets must not overlap, or two of them claim the same addresses.
	var blocks []netip.Prefix
	for name, s := range byName {
		prefixes := s.Inputs["addressPrefixes"].ArrayValue()
		if len(prefixes) != 1 {
			t.Fatalf("subnet %q has %d prefixes, want 1", name, len(prefixes))
		}
		p := netip.MustParsePrefix(prefixes[0].StringValue())
		if !cidr.Overlaps(p) {
			t.Errorf("subnet %q at %s falls outside the scope range %s", name, p, cidr)
		}
		for _, prev := range blocks {
			if prev.Overlaps(p) {
				t.Errorf("subnets overlap: %s and %s", prev, p)
			}
		}
		blocks = append(blocks, p)
	}

	// The perimeter is a property of the network, attached to the subnet
	// rather than to each interface, so a resource cannot end up outside
	// it by forgetting to attach one.
	if !hasResource(recorded, nsgToken) {
		t.Error("no network security group was declared")
	}
	if !hasResource(recorded, nsgAssocToken) {
		t.Error("the security group was not attached to the subnet")
	}

	// One DNS zone per engine, shared by every database of that engine in
	// the scope — the change from RFC 015, where each database had one.
	zones := resourcesOfType(recorded, dnsZoneToken)
	if len(zones) != 2 {
		t.Fatalf("declared %d private DNS zones, want one per engine", len(zones))
	}
	for _, z := range zones {
		name := z.Inputs["name"].StringValue()
		if !strings.HasSuffix(name, ".postgres.database.azure.com") &&
			!strings.HasSuffix(name, ".mysql.database.azure.com") {
			t.Errorf("zone %q does not end in an engine's domain; Azure rejects it", name)
		}
	}
	if n := len(resourcesOfType(recorded, dnsLinkToken)); n != 2 {
		t.Errorf("declared %d zone links, want one per zone — an unlinked zone resolves for nobody", n)
	}
}

// TestDeclareScopeEgress covers RFC 017 §2.7 for Azure.
//
// Two assertions carry the design. One NAT gateway for the scope, because
// the cost argument is the same one AWS makes. And it is attached to the
// general subnet only: a managed flexible server never pulls a package, so
// an egress path from its delegated subnet is a path only an exfiltrating
// query would use.
func TestDeclareScopeEgress(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	if n := len(resourcesOfType(recorded, natGatewayToken)); n != 1 {
		t.Fatalf("declared %d nat gateways, want exactly 1 for the scope", n)
	}

	// A NAT gateway rejects a dynamically allocated or Basic-SKU address
	// at create time, and Azure's default allocation is dynamic. Both are
	// stated in the args; a plan that omits either fails on apply, after
	// the user approved it.
	address := findResource(t, recorded, publicIPToken)
	if got := address.Inputs["allocationMethod"].StringValue(); got != natIPAllocationStatic {
		t.Errorf("nat address allocation = %q, want %q — the gateway rejects anything else",
			got, natIPAllocationStatic)
	}
	if got := address.Inputs["sku"].StringValue(); got != natPublicIPSkuStandard {
		t.Errorf("nat address sku = %q, want %q", got, natPublicIPSkuStandard)
	}
	if got := findResource(t, recorded, natGatewayToken).Inputs["skuName"].StringValue(); got != natSkuStandard {
		t.Errorf("nat gateway sku = %q, want %q", got, natSkuStandard)
	}

	if !hasResource(recorded, natIPAssocToken) {
		t.Error("the nat address was not attached to the gateway; the gateway translates to nothing")
	}

	// One subnet association, and it must be the general one. The
	// delegated database subnets are deliberately left without egress.
	assocs := resourcesOfType(recorded, natSubnetAssocTok)
	if len(assocs) != 1 {
		t.Fatalf("declared %d subnet associations, want 1 (the general subnet only)", len(assocs))
	}

	var general recordedResource
	for _, s := range resourcesOfType(recorded, subnetToken) {
		if s.Inputs["name"].StringValue() == generalSubnetName {
			general = s
		}
	}
	if got := assocs[0].Inputs["subnetId"].StringValue(); !strings.Contains(got, general.Name) {
		t.Errorf("nat is attached to subnet %q, want the general subnet %q", got, general.Name)
	}
}

// TestEnsureNetworkSkipsRegionlessScopes: a scope with no region has no
// network to build.
func TestEnsureNetworkSkipsRegionlessScopes(t *testing.T) {
	p := &AzureProvider{}

	err := p.EnsureNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.ProviderAzure,
		Environment: "dev",
	}, provider.ScopeContents{}, spec.Policies{})

	if err != nil {
		t.Errorf("EnsureNetwork() error = %v, want nil for a scope with no region", err)
	}
}

// TestScopeNamesAreDerivable is what lets two stacks agree without a
// shared state backend: Azure addresses resources by (resource group,
// name), and both derive from the scope.
func TestScopeNamesAreDerivable(t *testing.T) {
	tests := []struct {
		name     string
		scope    provider.NetworkScope
		wantNet  string
		wantRG   string
		wantZone string
	}{
		{
			name:     "environment and region",
			scope:    provider.NetworkScope{Environment: "dev", Region: "westeurope"},
			wantNet:  "cloudsdd-dev--westeurope",
			wantRG:   "cloudsdd-dev--westeurope-net-rg",
			wantZone: "cloudsdd-dev--westeurope.postgres.database.azure.com",
		},
		{
			name:     "unscoped",
			scope:    provider.NetworkScope{},
			wantNet:  "cloudsdd-default",
			wantRG:   "cloudsdd-default-net-rg",
			wantZone: "cloudsdd-default.postgres.database.azure.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeNetworkName(tt.scope); got != tt.wantNet {
				t.Errorf("scopeNetworkName() = %q, want %q", got, tt.wantNet)
			}
			if got := scopeResourceGroupName(tt.scope); got != tt.wantRG {
				t.Errorf("scopeResourceGroupName() = %q, want %q", got, tt.wantRG)
			}
			if got := engineZoneName(tt.scope, "postgres"); got != tt.wantZone {
				t.Errorf("engineZoneName() = %q, want %q", got, tt.wantZone)
			}
		})
	}
}

// TestScopeNetworkNameStaysUniqueWhenTruncated: two long scopes cut to
// the same prefix would share one network, which is RFC 016's central
// failure arriving through a string length rather than an address.
func TestScopeNetworkNameStaysUniqueWhenTruncated(t *testing.T) {
	long := strings.Repeat("environment", 4)

	a := scopeNetworkName(provider.NetworkScope{
		Account: "account-one", Environment: long, Region: "westeurope",
	})
	b := scopeNetworkName(provider.NetworkScope{
		Account: "account-two", Environment: long, Region: "westeurope",
	})

	if a == b {
		t.Errorf("two distinct scopes derived the same network name %q", a)
	}
	// The zone name is built on top of this, and Azure caps a private DNS
	// zone label too, so the network name has to leave room for a suffix.
	for _, name := range []string{a, b} {
		if len(name) > 40 {
			t.Errorf("name %q is %d characters, too long once the DNS suffix is added", name, len(name))
		}
		if strings.HasSuffix(name, "-") {
			t.Errorf("name %q ends with a hyphen, which Azure rejects", name)
		}
	}
}

func TestSubnetBlock(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		index   int
		want    string
		wantErr bool
	}{
		{name: "general", cidr: "10.42.0.0/20", index: subnetIndexGeneral, want: "10.42.0.0/24"},
		{name: "postgres", cidr: "10.42.0.0/20", index: subnetIndexPostgres, want: "10.42.1.0/24"},
		{name: "mysql", cidr: "10.42.0.0/20", index: subnetIndexMySQL, want: "10.42.2.0/24"},
		{name: "past the end", cidr: "10.42.0.0/20", index: 16, wantErr: true},
		{name: "negative", cidr: "10.42.0.0/20", index: -1, wantErr: true},
		// The wrap the AWS provider's equivalent had before it was fixed.
		{name: "no wrap on a wide range", cidr: "10.0.0.0/8", index: 256, want: "10.1.0.0/24"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := subnetBlock(netip.MustParsePrefix(tt.cidr), tt.index)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("subnetBlock() = %s, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("subnetBlock() error = %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("subnetBlock() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestDestroyNetworkSkipsRegionlessScopes mirrors EnsureNetwork: a scope
// with no region never had a network, so tearing one down is a no-op
// rather than an error about a missing region.
func TestDestroyNetworkSkipsRegionlessScopes(t *testing.T) {
	p := &AzureProvider{}

	err := p.DestroyNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.Provider("azure"),
		Environment: "dev",
	}, provider.ScopeContents{}, spec.Policies{})

	if err != nil {
		t.Errorf("DestroyNetwork() error = %v, want nil for a scope with no region", err)
	}
}

// TestDestroyNetworkRefusesAnUnusableAddressPlan: the scope's range is
// derived before the stack is touched, so a policy that cannot produce
// one fails without going near the cloud.
func TestDestroyNetworkRefusesAnUnusableAddressPlan(t *testing.T) {
	p := &AzureProvider{}

	err := p.DestroyNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.Provider("azure"),
		Environment: "dev",
		Region:      "test-region",
	}, provider.ScopeContents{}, spec.Policies{Network: &spec.NetworkPolicy{BaseCIDR: "8.8.0.0/16"}})

	if err == nil {
		t.Fatal("DestroyNetwork() error = nil, want the public base_cidr to be refused")
	}
	if !strings.Contains(err.Error(), "RFC 1918") {
		t.Errorf("error = %q, want it to name the address-plan problem", err)
	}
}

// TestResourceScope moved with the function it covered: the local
// resourceScope is now provider.ResourceScope (RFC 019 §2.4), and
// TestResourceScope in internal/provider/scope_test.go asserts the same
// three cases against all three provider constants rather than Azure's
// alone.
