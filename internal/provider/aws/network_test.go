// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

const (
	getAvailabilityZonesToken = "aws:index/getAvailabilityZones:getAvailabilityZones"
	vpcToken                  = "aws:ec2/vpc:Vpc"
	subnetToken               = "aws:ec2/subnet:Subnet"
	dbSubnetGroupToken        = "aws:rds/subnetGroup:SubnetGroup"
	internetGatewayToken      = "aws:ec2/internetGateway:InternetGateway"
	natGatewayToken           = "aws:ec2/natGateway:NatGateway"
	eipToken                  = "aws:ec2/eip:Eip"
	routeTableToken           = "aws:ec2/routeTable:RouteTable"
	routeTableAssocToken      = "aws:ec2/routeTableAssociation:RouteTableAssociation"
)

func testScope() provider.NetworkScope {
	return provider.NetworkScope{
		Provider:    spec.ProviderAWS,
		Environment: "dev",
		Region:      "eu-central-1",
		Sealed:      true,
	}
}

// TestDeclareScopeNetwork covers the shape RFC 016 §2.3 specifies for
// AWS. The assertions that matter are the ones that were false before it:
// a VPC of CloudSDD's own rather than the account's default, and subnets
// that never hand out a public address.
func TestDeclareScopeNetwork(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	vpc := findResource(t, recorded, vpcToken)
	if got := vpc.Inputs["cidrBlock"].StringValue(); got != cidr.String() {
		t.Errorf("vpc cidr = %q, want the derived %q", got, cidr)
	}
	// Both are needed for RDS to hand out an endpoint anything can
	// resolve; without them the database is created and unusable.
	if !vpc.Inputs["enableDnsHostnames"].BoolValue() {
		t.Error("enableDnsHostnames = false; the database endpoint will not resolve")
	}
	if !vpc.Inputs["enableDnsSupport"].BoolValue() {
		t.Error("enableDnsSupport = false; the database endpoint will not resolve")
	}

	// The tags are the contract resource programs find the network by.
	tags := vpc.Inputs["tags"].ObjectValue()
	if got := tags[tagManagedBy].StringValue(); got != managedByValue {
		t.Errorf("%s tag = %q, want %q", tagManagedBy, got, managedByValue)
	}
	if got := tags[tagScope].StringValue(); got != "dev::eu-central-1" {
		t.Errorf("%s tag = %q, want the scope label", tagScope, got)
	}

	// Every subnet, both tiers. A subnet that assigns public addresses on
	// launch would make "private by default" a property of nothing — and
	// that holds for the public tier too, which exists to hold a NAT
	// gateway rather than anything that launches.
	all := resourcesOfType(recorded, subnetToken)
	if len(all) != subnetCount+publicSubnetCount {
		t.Fatalf("declared %d subnets, want %d private plus %d public",
			len(all), subnetCount, publicSubnetCount)
	}

	var privateSubnets []recordedResource
	zones := map[string]bool{}
	for _, s := range all {
		if s.Inputs["mapPublicIpOnLaunch"].BoolValue() {
			t.Error("mapPublicIpOnLaunch = true; instances would get public addresses")
		}
		if s.Inputs["tags"].ObjectValue()[tagSubnetTier].StringValue() != tierPrivate {
			continue
		}
		privateSubnets = append(privateSubnets, s)
		zones[s.Inputs["availabilityZone"].StringValue()] = true
	}
	if len(privateSubnets) != subnetCount {
		t.Fatalf("declared %d private subnets, want %d — RDS refuses a subnet group with fewer",
			len(privateSubnets), subnetCount)
	}
	// Spread, not stacked: a subnet group whose subnets share one zone
	// is a database that cannot fail over.
	if len(zones) != subnetCount {
		t.Errorf("private subnets occupy %d availability zones, want %d: %v",
			len(zones), subnetCount, zones)
	}

	group := findResource(t, recorded, dbSubnetGroupToken)
	if got := group.Inputs["name"].StringValue(); got != dbSubnetGroupNameFor(testScope()) {
		t.Errorf("db subnet group name = %q, want the derived %q", got, dbSubnetGroupNameFor(testScope()))
	}
	// The subnet group must span the private tier only. A public subnet in
	// it would let RDS place a database where the internet gateway route
	// reaches it.
	if n := len(group.Inputs["subnetIds"].ArrayValue()); n != subnetCount {
		t.Errorf("db subnet group spans %d subnets, want the %d private ones", n, subnetCount)
	}
}

// TestDeclareScopeEgress covers RFC 017 §2.7: the private subnets get a
// route out, and getting one does not turn into a route in.
//
// The assertion that matters most is the count. One NAT gateway per scope
// is the whole cost argument — a loop that accidentally declared one per
// availability zone would be correct infrastructure at double the standing
// bill of every environment CloudSDD creates.
func TestDeclareScopeEgress(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	if n := len(resourcesOfType(recorded, natGatewayToken)); n != 1 {
		t.Fatalf("declared %d nat gateways, want exactly 1 for the scope", n)
	}
	if n := len(resourcesOfType(recorded, eipToken)); n != 1 {
		t.Errorf("declared %d elastic addresses, want 1 — one per nat gateway", n)
	}
	if !hasResource(recorded, internetGatewayToken) {
		t.Error("no internet gateway; the nat gateway has nothing to leave through")
	}

	// Exactly one public subnet, carved from the scope's own /20 and
	// overlapping none of the private blocks. subnetBlock is now asked for
	// one more index than before, which is where an off-by-one would put
	// the public tier on top of a private one.
	var public, private []recordedResource
	for _, s := range resourcesOfType(recorded, subnetToken) {
		switch s.Inputs["tags"].ObjectValue()[tagSubnetTier].StringValue() {
		case tierPublic:
			public = append(public, s)
		case tierPrivate:
			private = append(private, s)
		}
	}
	// Two, not one: an internet-facing ALB refuses to exist in a single
	// availability zone (RFC 017 §2.3.1). One NAT gateway all the same —
	// the extra subnet is empty until a load balancer needs it.
	if len(public) != publicSubnetCount {
		t.Fatalf("declared %d public subnets, want %d — an internet-facing ALB needs two zones",
			len(public), publicSubnetCount)
	}
	publicZones := map[string]bool{}
	for _, p := range public {
		publicZones[p.Inputs["availabilityZone"].StringValue()] = true

		publicBlock := netip.MustParsePrefix(p.Inputs["cidrBlock"].StringValue())
		if !cidr.Overlaps(publicBlock) {
			t.Errorf("public subnet %s falls outside the scope range %s", publicBlock, cidr)
		}
		for _, s := range private {
			block := netip.MustParsePrefix(s.Inputs["cidrBlock"].StringValue())
			if block.Overlaps(publicBlock) {
				t.Errorf("public subnet %s overlaps private subnet %s", publicBlock, block)
			}
		}
	}
	if len(publicZones) != publicSubnetCount {
		t.Errorf("public subnets occupy %d zones, want %d: %v", len(publicZones), publicSubnetCount, publicZones)
	}

	// Two route tables, and each with the right kind of default route:
	// the public one through the internet gateway, the private one through
	// the NAT. A private table pointing at the gateway would be the whole
	// scope on the internet.
	var publicRT, privateRT recordedResource
	for _, rt := range resourcesOfType(recorded, routeTableToken) {
		switch rt.Inputs["tags"].ObjectValue()[tagSubnetTier].StringValue() {
		case tierPublic:
			publicRT = rt
		case tierPrivate:
			privateRT = rt
		}
	}
	assertDefaultRoute(t, publicRT, "gatewayId", tierPublic)
	assertDefaultRoute(t, privateRT, "natGatewayId", tierPrivate)
	if privateRT.Inputs["routes"].ArrayValue()[0].ObjectValue()["gatewayId"].HasValue() {
		t.Error("the private route table routes through an internet gateway; the whole scope would be reachable")
	}

	// Every private subnet is associated, plus the public one. A subnet
	// left unassociated falls back to the VPC's main route table, which
	// has no default route: silently no egress, which is the bug this
	// section exists to fix.
	if n := len(resourcesOfType(recorded, routeTableAssocToken)); n != subnetCount+publicSubnetCount {
		t.Errorf("declared %d route table associations, want %d — one per subnet",
			n, subnetCount+publicSubnetCount)
	}
}

// assertDefaultRoute checks a route table carries a default route through
// the expected kind of target.
func assertDefaultRoute(t *testing.T, rt recordedResource, targetKey, tier string) {
	t.Helper()

	routes := rt.Inputs["routes"].ArrayValue()
	if len(routes) != 1 {
		t.Fatalf("%s route table has %d routes, want 1 default route", tier, len(routes))
	}
	route := routes[0].ObjectValue()
	if got := route["cidrBlock"].StringValue(); got != defaultRoute {
		t.Errorf("%s route table destination = %q, want %q", tier, got, defaultRoute)
	}
	if !route[resource.PropertyKey(targetKey)].HasValue() {
		t.Errorf("%s route table has no %s; the route goes nowhere", tier, targetKey)
	}
}

// TestEnsureNetworkSkipsRegionlessScopes: object_storage and
// cross_account_role reach EnsureNetwork without a region, and neither
// lives in a network. Attempting to build one would fail for want of a
// region rather than doing nothing.
func TestEnsureNetworkSkipsRegionlessScopes(t *testing.T) {
	p := &AWSProvider{}

	err := p.EnsureNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.ProviderAWS,
		Environment: "dev",
	}, provider.ScopeContents{}, spec.Policies{})

	if err != nil {
		t.Errorf("EnsureNetwork() error = %v, want nil for a scope with no region", err)
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
		{name: "first subnet of a /20", cidr: "10.42.0.0/20", index: 0, want: "10.42.0.0/24"},
		{name: "second subnet of a /20", cidr: "10.42.0.0/20", index: 1, want: "10.42.1.0/24"},
		{name: "last subnet of a /20", cidr: "10.42.0.0/20", index: 15, want: "10.42.15.0/24"},
		{name: "past the end of a /20", cidr: "10.42.0.0/20", index: 16, wantErr: true},
		{name: "scope smaller than a subnet", cidr: "10.42.0.0/25", index: 0, wantErr: true},
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

// TestSubnetsDoNotOverlap is the property the arithmetic exists to
// guarantee, checked over every subnet a scope can hold rather than the
// two it uses today.
func TestSubnetsDoNotOverlap(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	var blocks []netip.Prefix
	for i := 0; i < 16; i++ {
		b, err := subnetBlock(cidr, i)
		if err != nil {
			t.Fatalf("subnetBlock(%d) error = %v", i, err)
		}
		if !cidr.Overlaps(b) {
			t.Errorf("subnet %s falls outside the scope range %s", b, cidr)
		}
		for _, prev := range blocks {
			if prev.Overlaps(b) {
				t.Errorf("subnets %s and %s overlap", prev, b)
			}
		}
		blocks = append(blocks, b)
	}
}

// TestDBSubnetGroupNameIsDerivable is what lets two stacks agree on a
// name without talking to each other: the network stack sets it and the
// database's stack computes the same string from the same scope.
func TestDBSubnetGroupNameIsDerivable(t *testing.T) {
	tests := []struct {
		name  string
		scope provider.NetworkScope
		want  string
	}{
		{
			name:  "environment and region",
			scope: provider.NetworkScope{Environment: "dev", Region: "eu-central-1"},
			want:  "cloudsdd-dev--eu-central-1",
		},
		{
			name:  "account included",
			scope: provider.NetworkScope{Account: "prod", Environment: "live", Region: "eu-west-1"},
			want:  "cloudsdd-prod--live--eu-west-1",
		},
		{
			name:  "unscoped",
			scope: provider.NetworkScope{},
			want:  "cloudsdd-default",
		},
		{
			name:  "uppercase folded",
			scope: provider.NetworkScope{Environment: "Dev", Region: "EU-Central-1"},
			want:  "cloudsdd-dev--eu-central-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dbSubnetGroupNameFor(tt.scope)
			if got != tt.want {
				t.Errorf("dbSubnetGroupNameFor() = %q, want %q", got, tt.want)
			}
			// RDS accepts lowercase letters, digits and hyphens only; a
			// name it rejects fails at apply, after the plan was approved.
			for _, r := range got {
				valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
				if !valid {
					t.Errorf("name %q contains %q, which RDS rejects", got, r)
				}
			}
			if !strings.HasPrefix(got, "cloudsdd-") {
				t.Errorf("name %q does not identify itself as CloudSDD's", got)
			}
		})
	}
}

func TestScopeNetworkSubnetSelection(t *testing.T) {
	net := scopeNetwork{subnetIDs: []string{"subnet-a", "subnet-b"}}

	tests := []struct {
		name string
		zone string
		want string
	}{
		{name: "no preference takes the first", zone: "", want: "subnet-a"},
		{name: "zone a", zone: "eu-central-1a", want: "subnet-a"},
		{name: "zone b", zone: "eu-central-1b", want: "subnet-b"},
		// A zone the network does not span falls back rather than
		// returning nothing: an empty subnet ID fails at apply with a
		// message about the wrong thing.
		{name: "zone beyond the network", zone: "eu-central-1f", want: "subnet-a"},
		{name: "malformed zone", zone: "eu-central-1", want: "subnet-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := net.subnet(tt.zone); got != tt.want {
				t.Errorf("subnet(%q) = %q, want %q", tt.zone, got, tt.want)
			}
		})
	}

	if got := (scopeNetwork{}).subnet("eu-central-1a"); got != "" {
		t.Errorf("subnet() on an empty network = %q, want the empty string", got)
	}
}

const (
	getVpcToken     = "aws:ec2/getVpc:getVpc"
	getSubnetsToken = "aws:ec2/getSubnets:getSubnets"
)

// TestLookupScopeNetwork covers the other half of RFC 016 §2.2's
// contract. The network stack tags what it builds; a resource program
// finds it by those tags. If the two halves ever disagree about a tag
// name, every deploy lands in the wrong network — or, before this RFC,
// in the account's default VPC, which is the failure the whole design
// exists to remove.
func TestLookupScopeNetwork(t *testing.T) {
	r := spec.Resource{
		ID:       "app-db",
		Type:     spec.ResourceTypeRelationalDatabase,
		Provider: spec.ProviderAWS,
		Scope:    spec.Scope{Environment: "dev", Region: "eu-central-1"},
	}

	var got scopeNetwork
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		var err error
		got, err = lookupScopeNetwork(ctx, r)
		return err
	})

	if got.vpcID != "vpc-0123456789" {
		t.Errorf("vpcID = %q, want the looked-up vpc", got.vpcID)
	}
	if got.cidr != "10.42.0.0/20" {
		t.Errorf("cidr = %q, want the vpc's range", got.cidr)
	}
	if len(got.subnetIDs) != 2 {
		t.Errorf("subnetIDs = %v, want the two private subnets", got.subnetIDs)
	}
	// The name the network stack set, recomputed rather than looked up.
	if want := dbSubnetGroupNameFor(testScope()); got.dbSubnetName != want {
		t.Errorf("dbSubnetName = %q, want %q", got.dbSubnetName, want)
	}

	// The VPC must be found by *both* tags. Matching on the scope alone
	// would pick up a VPC somebody else tagged the same way; matching on
	// ManagedBy alone would pick up another environment's.
	lookup := findResource(t, recorded, getVpcToken)
	tags := lookup.Inputs["tags"].ObjectValue()
	if got := tags[tagManagedBy].StringValue(); got != managedByValue {
		t.Errorf("lookup %s tag = %q, want %q", tagManagedBy, got, managedByValue)
	}
	if got := tags[tagScope].StringValue(); got != "dev::eu-central-1" {
		t.Errorf("lookup %s tag = %q, want the scope label", tagScope, got)
	}

	// Subnets are filtered to the private tier: a database placed in a
	// public subnet would be one route table away from the internet.
	filters := findResource(t, recorded, getSubnetsToken).Inputs["filters"].ArrayValue()
	var sawTier bool
	for _, f := range filters {
		if f.ObjectValue()["name"].StringValue() == "tag:"+tagSubnetTier {
			sawTier = true
			if v := f.ObjectValue()["values"].ArrayValue(); len(v) != 1 || v[0].StringValue() != tierPrivate {
				t.Errorf("tier filter = %v, want only %q", v, tierPrivate)
			}
		}
	}
	if !sawTier {
		t.Errorf("subnets were not filtered by tier; a public subnet could be selected")
	}
}

// TestLookupScopeNetworkUsesTheResourceAccount pins the account into the
// discovery path. Two accounts never share a network (RFC 016 §2.1), so
// a lookup that ignored Resource.Account would find the wrong one — or,
// worse, the right-looking one in the wrong place.
func TestLookupScopeNetworkUsesTheResourceAccount(t *testing.T) {
	r := spec.Resource{
		ID:       "app-db",
		Account:  "prod-account",
		Provider: spec.ProviderAWS,
		Scope:    spec.Scope{Environment: "live", Region: "eu-central-1"},
	}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := lookupScopeNetwork(ctx, r)
		return err
	})

	tags := findResource(t, recorded, getVpcToken).Inputs["tags"].ObjectValue()
	if got := tags[tagScope].StringValue(); got != "prod-account::live::eu-central-1" {
		t.Errorf("lookup scope tag = %q, want the account to be part of it", got)
	}
}

// TestSubnetBlockDoesNotWrapOnWideRanges is the regression test for the
// bug gosec surfaced. The original arithmetic incremented the third
// octet, which is correct only while the scope is a /16 or smaller; for a
// wider block the index passes 255 and the octet wraps, silently naming a
// subnet in a different part of the range. Scopes are /20 today, so this
// was latent — and latent is exactly how it would have stayed until
// somebody widened a scope.
func TestSubnetBlockDoesNotWrapOnWideRanges(t *testing.T) {
	cidr := netip.MustParsePrefix("10.0.0.0/8")

	// Index 256 is where a byte increment wraps back to the start.
	got, err := subnetBlock(cidr, 256)
	if err != nil {
		t.Fatalf("subnetBlock() error = %v", err)
	}
	if got.String() != "10.1.0.0/24" {
		t.Errorf("subnetBlock(/8, 256) = %s, want 10.1.0.0/24", got)
	}
	if first, _ := subnetBlock(cidr, 0); first == got {
		t.Errorf("subnet 256 wrapped onto subnet 0 (%s)", got)
	}

	// And the far end of the block still lands inside it.
	last, err := subnetBlock(cidr, 65535)
	if err != nil {
		t.Fatalf("subnetBlock(/8, 65535) error = %v", err)
	}
	if !cidr.Overlaps(last) {
		t.Errorf("subnetBlock(/8, 65535) = %s, outside %s", last, cidr)
	}
	if _, err := subnetBlock(cidr, 65536); err == nil {
		t.Error("subnetBlock(/8, 65536) = nil error, want the range to be exhausted")
	}
	if _, err := subnetBlock(cidr, -1); err == nil {
		t.Error("subnetBlock(/8, -1) = nil error, want a negative index refused")
	}
}

// TestDestroyNetworkSkipsRegionlessScopes mirrors EnsureNetwork: a scope
// with no region never had a network, so tearing one down is a no-op
// rather than an error about a missing region.
func TestDestroyNetworkSkipsRegionlessScopes(t *testing.T) {
	p := &AWSProvider{}

	err := p.DestroyNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.Provider("aws"),
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
	p := &AWSProvider{}

	err := p.DestroyNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.Provider("aws"),
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
