// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// testNetworkName is the scope network a declaration test places its
// resources in, standing in for the name a resource program derives.
const testNetworkName = "cloudsdd-dev--europe-west1"

const (
	networkToken       = "gcp:compute/network:Network"
	subnetworkToken    = "gcp:compute/subnetwork:Subnetwork"
	globalAddressToken = "gcp:compute/globalAddress:GlobalAddress"
	connectionToken    = "gcp:servicenetworking/connection:Connection"
)

func testScope() provider.NetworkScope {
	return provider.NetworkScope{
		Provider:    spec.ProviderGCP,
		Environment: "dev",
		Region:      "europe-west1",
		Sealed:      true,
	}
}

// TestDeclareScopeNetwork covers RFC 016 §2.3 for GCP. The assertion that
// matters most is the servicenetworking connection: without it a Cloud
// SQL instance with Ipv4Enabled false has no address of any kind, which
// is the defect RFC 016 §1 recorded — "private" delivered as *absent*.
func TestDeclareScopeNetwork(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr)
	})

	vpc := findResource(t, recorded, networkToken)
	// Auto mode would create a subnet in every region out of a range
	// Google picks, which is exactly the overlap the address plan exists
	// to prevent.
	if vpc.Inputs["autoCreateSubnetworks"].BoolValue() {
		t.Error("autoCreateSubnetworks = true; Google would choose ranges outside the address plan")
	}
	if got := vpc.Inputs["name"].StringValue(); got != scopeNetworkName(testScope()) {
		t.Errorf("network name = %q, want the derived %q", got, scopeNetworkName(testScope()))
	}

	subnet := findResource(t, recorded, subnetworkToken)
	if got := subnet.Inputs["ipCidrRange"].StringValue(); got != "10.42.0.0/24" {
		t.Errorf("subnet range = %q, want the first /24 of the scope", got)
	}
	if got := subnet.Inputs["region"].StringValue(); got != "europe-west1" {
		t.Errorf("subnet region = %q, want the scope's", got)
	}
	// Without this an instance with no external address cannot reach
	// Google's APIs, which is how it reaches OS Login and Cloud SQL's
	// admin API in a network with no route to the internet.
	if !subnet.Inputs["privateIpGoogleAccess"].BoolValue() {
		t.Error("privateIpGoogleAccess = false; a private instance cannot reach Google APIs")
	}

	addr := findResource(t, recorded, globalAddressToken)
	if got := addr.Inputs["purpose"].StringValue(); got != peeringPurpose {
		t.Errorf("peering range purpose = %q, want %q", got, peeringPurpose)
	}
	// Carved from the scope's own range rather than left to Google: a
	// Google-chosen range would fall outside the address plan and could
	// overlap another scope.
	if got := addr.Inputs["address"].StringValue(); got != "10.42.1.0" {
		t.Errorf("peering range address = %q, want the second /24 of the scope", got)
	}
	if got := addr.Inputs["prefixLength"].NumberValue(); int(got) != peeringPrefixLength {
		t.Errorf("peering prefix = /%v, want /%d", got, peeringPrefixLength)
	}

	conn := findResource(t, recorded, connectionToken)
	if got := conn.Inputs["service"].StringValue(); got != peeringService {
		t.Errorf("peering service = %q, want %q", got, peeringService)
	}
	if n := len(conn.Inputs["reservedPeeringRanges"].ArrayValue()); n != 1 {
		t.Errorf("connection reserves %d ranges, want 1", n)
	}

	// The scope network ships no rules, so there is nothing to override
	// here; the instance still declares its own deny.
	if hasResource(recorded, firewallToken) {
		t.Error("a firewall rule was declared on the scope network; it ships allow-nothing already")
	}
}

// TestEnsureNetworkSkipsRegionlessScopes: object_storage is global on GCP
// and reaches EnsureNetwork with no region. It lives in no network.
func TestEnsureNetworkSkipsRegionlessScopes(t *testing.T) {
	p := &GCPProvider{}

	err := p.EnsureNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.ProviderGCP,
		Environment: "dev",
	}, spec.Policies{})

	if err != nil {
		t.Errorf("EnsureNetwork() error = %v, want nil for a scope with no region", err)
	}
}

// TestScopeNetworkNameIsDerivable is what lets a resource program find
// its network without an invoke: GCP names are unique in a project and
// resolvable directly, so both halves compute the same string from the
// same scope.
func TestScopeNetworkNameIsDerivable(t *testing.T) {
	tests := []struct {
		name  string
		scope provider.NetworkScope
		want  string
	}{
		{
			name:  "environment and region",
			scope: provider.NetworkScope{Environment: "dev", Region: "europe-west1"},
			want:  "cloudsdd-dev--europe-west1",
		},
		{
			name:  "account included",
			scope: provider.NetworkScope{Account: "prod", Environment: "live", Region: "europe-west1"},
			want:  "cloudsdd-prod--live--europe-west1",
		},
		{
			name:  "unscoped",
			scope: provider.NetworkScope{},
			want:  "cloudsdd-default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scopeNetworkName(tt.scope)
			if got != tt.want {
				t.Errorf("scopeNetworkName() = %q, want %q", got, tt.want)
			}
			assertValidGCPName(t, got)
		})
	}
}

// TestScopeNetworkNameStaysUniqueWhenTruncated covers the long-scope
// case. GCP caps a network name at 63 characters, and truncating two long
// scopes to the same prefix would put two environments on one network —
// the exact failure RFC 016 exists to prevent, arriving through a string
// length rather than an address.
func TestScopeNetworkNameStaysUniqueWhenTruncated(t *testing.T) {
	long := strings.Repeat("environment", 4) // 44 chars, plus account and region

	a := scopeNetworkName(provider.NetworkScope{
		Account: "account-one", Environment: long, Region: "europe-west1",
	})
	b := scopeNetworkName(provider.NetworkScope{
		Account: "account-two", Environment: long, Region: "europe-west1",
	})

	if len(a) > 63 || len(b) > 63 {
		t.Fatalf("names exceed the 63-character limit: %d, %d", len(a), len(b))
	}
	if a == b {
		t.Errorf("two distinct scopes derived the same network name %q", a)
	}
	assertValidGCPName(t, a)
	assertValidGCPName(t, b)
}

func assertValidGCPName(t *testing.T, name string) {
	t.Helper()

	if len(name) > 63 {
		t.Errorf("name %q is %d characters, over GCP's 63 limit", name, len(name))
	}
	if name == "" {
		t.Fatal("name is empty")
	}
	if first := name[0]; first < 'a' || first > 'z' {
		t.Errorf("name %q does not start with a lowercase letter", name)
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("name %q ends with a hyphen, which GCP rejects", name)
	}
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			t.Errorf("name %q contains %q, which GCP rejects", name, r)
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
		{name: "workload subnet", cidr: "10.42.0.0/20", index: 0, want: "10.42.0.0/24"},
		{name: "peering range", cidr: "10.42.0.0/20", index: 1, want: "10.42.1.0/24"},
		{name: "past the end", cidr: "10.42.0.0/20", index: 16, wantErr: true},
		{name: "negative", cidr: "10.42.0.0/20", index: -1, wantErr: true},
		{name: "scope smaller than a subnet", cidr: "10.42.0.0/25", index: 0, wantErr: true},
		// The wrap the AWS provider's equivalent had: an octet increment
		// is correct only while the scope is a /16 or smaller.
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

// TestWorkloadAndPeeringDoNotOverlap: Google allocates its managed
// services out of the peering range, so an overlap with the workload
// subnet would be two things claiming the same addresses.
func TestWorkloadAndPeeringDoNotOverlap(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	workload, err := subnetBlock(cidr, 0)
	if err != nil {
		t.Fatalf("subnetBlock(0) error = %v", err)
	}
	peering, err := subnetBlock(cidr, 1)
	if err != nil {
		t.Fatalf("subnetBlock(1) error = %v", err)
	}

	if workload.Overlaps(peering) {
		t.Errorf("workload %s overlaps the peering range %s", workload, peering)
	}
	for _, p := range []netip.Prefix{workload, peering} {
		if !cidr.Overlaps(p) {
			t.Errorf("%s falls outside the scope range %s", p, cidr)
		}
	}
}
